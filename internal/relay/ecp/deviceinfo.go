package ecp

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// deviceinfo.go is ECP's IDENTIFICATION half, the read direction of the same
// unauthenticated HTTP surface Dispatch writes to.
//
// Discovery answers "something at 192.168.1.31:8060 responded to the `roku:ecp`
// search target". That is a claim made by whatever chose to answer a multicast
// packet, and it is all SSDP can ever say: an M-SEARCH response carries a search
// target and a USN, and any host on the LAN can emit both. What it does NOT say
// is what the device is — its model, its serial, or the name its owner gave it —
// and without those an operator deciding whether to adopt a device is choosing
// between opaque UUIDs.
//
// A GET of `/query/device-info` answers all three, and answering it AT ALL is
// the classification: ECP is Roku's own control protocol, and a host that
// returns a well-formed `<device-info>` document on that path is speaking it.
// So this probe is both the enrichment and the confirmation — a discovered
// address that answers is a Roku the relay can control, one that does not is a
// sighting the relay reports without claiming more than SSDP told it.
//
// It is a READ of public device metadata over a protocol REL-114 already
// establishes carries no credential, so there is nothing here to protect and
// nothing to log-redact. What there IS to bound is trust in the response: the
// document comes off the same LAN the sighting did, so its body is capped, its
// fields are capped, and a malformed or oversized answer is an error rather
// than a partially-decoded device.

// deviceInfoPath is Roku ECP's device-metadata query. It is a GET (unlike every
// path Dispatch uses, which are POSTs) because it reads rather than acts.
const deviceInfoPath = "/query/device-info"

// deviceInfoTimeout bounds one identification probe. Deliberately shorter than
// defaultTimeout: a probe runs inside a discovery sweep, once per newly seen
// address, and a host that answered a multicast search milliseconds ago either
// answers ECP promptly or is not a Roku. Waiting the full command timeout on
// every non-Roku that happens to answer the search target would stretch a sweep
// by seconds per stranger.
const deviceInfoTimeout = 2 * time.Second

// maxDeviceInfoBytes caps the response body read. Roku's real document is a
// couple of kilobytes; 64 KiB is far above it and far below what an unbounded
// io.ReadAll from a hostile LAN host could hand a relay that has one probe in
// flight per discovered address.
const maxDeviceInfoBytes = 64 << 10

// maxDeviceInfoFieldBytes caps each field carried out of the document. It
// matches internal/relay/deviceplane's own observation-field cap, which is the
// gate these values pass through next: a field that could not survive Observe
// must not be built into a candidate here, or the whole sighting is silently
// dropped one layer down for a reason this layer already knew.
const maxDeviceInfoFieldBytes = 256

// DeviceInfo is what one successful probe established about the device at a
// discovered address: the name its owner sees, what model it is, and the serial
// that identifies the physical unit.
//
// Name and Model are RESOLVED rather than raw fields — Roku's document carries
// several candidates for each and populates different ones on different
// firmware — so a caller gets the best available answer without re-deriving the
// precedence at every call site. Every field may be empty: a device that
// answers ECP is a Roku whether or not it filled in a friendly name, and the
// answering itself is the classification.
type DeviceInfo struct {
	// Name is the device's own self-reported name, in the order Roku's own
	// UI prefers: the name the owner set, then the vendor's friendly name,
	// then the factory default.
	Name string
	// NameSource says WHICH of those three elements Name came from. The
	// precedence used to be resolved and then thrown away, which made an
	// owner-set name and a factory model string indistinguishable one layer up —
	// and a merge that cannot tell them apart cannot refuse the worse one
	// (internal/relay/deviceplane.NameRank). It is NameSourceNone exactly when
	// Name is empty.
	NameSource NameSource
	// Model is the best human-readable model string the device reports: the
	// vendor's friendly model name, then the model name, then the model number —
	// a number is a worse label than a name but a much better one than nothing.
	//
	// Unlike Name it carries NO source, and that asymmetry is deliberate rather
	// than unfinished — see the resolution in QueryDeviceInfo for why a rank here
	// would have nothing to refuse.
	Model string
	// SerialNumber is the physical unit's serial, stable across renames,
	// reboots and DHCP leases.
	SerialNumber string
}

// deviceInfoDoc is the subset of Roku's `<device-info>` document this probe
// decodes. Every other element in it (power state, screen size, feature flags,
// dozens more) is deliberately absent: unmarshaling only what is used means a
// firmware that adds elements cannot change what this returns, and a firmware
// that drops one leaves an empty string rather than failing the decode.
//
// Decoding only what is used is right; deciding what is used from the element
// NAMES was not. The lab's Roku answers with 80 child elements and this struct
// read six of them, and the one carrying the only human-readable model string was
// among the 74 it skipped (#207) — so the merge layers downstream were
// arbitrating between candidates while the best answer never entered the system
// at all. A field this small is worth checking against a real document rather
// than a plausible one.
type deviceInfoDoc struct {
	XMLName      xml.Name `xml:"device-info"`
	SerialNumber string   `xml:"serial-number"`
	// FriendlyModelName is not in Roku's published `/query/device-info` reference
	// (checked 2026-08-20: the documented response lists `model-name` and
	// `model-number` and not this element). That is weaker evidence against it
	// than it looks, because `friendly-device-name` and `default-device-name` —
	// two elements this struct has read since it was written — are absent from the
	// same list and present on the wire. Undocumented is not rare here.
	//
	// SAMPLE SIZE: ONE. The only Roku in the lab is 192.168.50.31, an onn-branded
	// TV on firmware 15.3.4, and it is the sole real document anyone has read this
	// against. No claim is made about other vendors or other firmware, because
	// none was measured. That is exactly why the element rides BEHIND a
	// fall-through rather than replacing anything: on a firmware that omits it the
	// candidate is skipped and the result is byte-for-byte what it was before.
	FriendlyModelName  string `xml:"friendly-model-name"`
	ModelName          string `xml:"model-name"`
	ModelNumber        string `xml:"model-number"`
	UserDeviceName     string `xml:"user-device-name"`
	FriendlyDeviceName string `xml:"friendly-device-name"`
	DefaultDeviceName  string `xml:"default-device-name"`
}

// NewIdentifyClient returns the *http.Client an identification probe should
// use: a plain client with the short deviceInfoTimeout, no keep-alive tuning,
// and no redirect policy of its own. It exists so the deployment wiring does not
// have to restate the timeout — and so a probe cannot accidentally be handed the
// command client, whose longer deadline is chosen for a different job.
func NewIdentifyClient() *http.Client {
	return &http.Client{Timeout: deviceInfoTimeout}
}

// QueryDeviceInfo GETs `/query/device-info` from the ECP endpoint at addr (a
// dialable "host:port" authority) and returns what the device says about itself.
//
// An error means the address is NOT a confirmed Roku: nothing answered, it
// answered non-2xx, or what it returned was not a `<device-info>` document. The
// caller treats that as "unidentified", never as "absent" — the device was seen
// by discovery either way, and this probe only ever adds facts.
//
// ctx bounds the exchange alongside the client's own timeout, so a sweep that is
// canceled mid-probe returns immediately instead of waiting out a wedged host.
//
// REACHABILITY — read this before believing any claim that fixing what this
// function RETURNS changes what an operator SEES. There is exactly one caller
// (cmd/waiveo-relay/main.go, wired as discovery.Config.Identify), reached only
// through discovery.identityOf, which has two call sites: searchPattern, which
// runs once per member of the Discoverer's watch set, and observeAlive, which
// returns early unless an alive NOTIFY's NT is in that same set. So the probe
// fires for SSDP-watched targets and nothing else — the scheduled scan's ARP
// sweep and port scan never call it, and internal/relay/portscan records open
// ports, never a model.
//
// The watch set is not a constant. Core deliberately declares no SSDP pattern
// (cmd/waiveo-relay/main.go's empty builtinSSDP, guarded by
// coreisdeviceblind_test.go: recognising a device kind is an extension's
// contribution, owner 2026-08-17), so every member comes from an installed
// pack's match pattern. MEASURED on box .12 on 2026-08-20, pid 3135258:
// "discovery watches: 0 ssdp, 1 mdns live (0 pack pattern(s))". The same box
// logged "1 ssdp, 1 mdns live (1 pack pattern(s))" until 2026-08-19 18:17:47,
// when generation 27 applied with the pattern gone — alongside a pack uninstall
// at 18:17:09. With zero SSDP watches the sweep iterates nothing, no probe is
// issued, and this function is unreachable on that deployment however correct
// its body is.
//
// That is why nothing here promises a self-heal. The mirror row for
// 192.168.50.31 holds model=100012587 today and will keep holding it — Model is
// presence-only at all three merge layers — until some pack re-declares an SSDP
// pattern this device answers AND a relay carrying this fix scans. Both halves
// are required; this change supplies only the second.
func QueryDeviceInfo(ctx context.Context, client *http.Client, addr string) (DeviceInfo, error) {
	if client == nil {
		return DeviceInfo{}, fmt.Errorf("ecp: QueryDeviceInfo: no HTTP client")
	}
	if addr == "" {
		return DeviceInfo{}, fmt.Errorf("ecp: QueryDeviceInfo: no address")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+deviceInfoPath, nil)
	if err != nil {
		return DeviceInfo{}, fmt.Errorf("ecp: QueryDeviceInfo: building request for %s: %w", addr, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return DeviceInfo{}, fmt.Errorf("ecp: QueryDeviceInfo: %s did not answer: %w", addr, err)
	}
	defer func() {
		// Drain then close, for the same keep-alive reason Dispatch does: the
		// next probe (or the first command) to this device reuses the connection
		// instead of forcing the transport to tear it down.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DeviceInfo{}, fmt.Errorf("ecp: QueryDeviceInfo: %s returned status %d", addr, resp.StatusCode)
	}

	// Bounded read: the body arrives from an unauthenticated LAN host, so the
	// cap is applied to the reader rather than checked after the fact.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDeviceInfoBytes))
	if err != nil {
		return DeviceInfo{}, fmt.Errorf("ecp: QueryDeviceInfo: reading %s: %w", addr, err)
	}
	var doc deviceInfoDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		// Not a Roku, or not answering with one. Either way the address is
		// unidentified, which is exactly what an error here means.
		return DeviceInfo{}, fmt.Errorf("ecp: QueryDeviceInfo: %s did not return a device-info document: %w", addr, err)
	}

	// The name and WHICH element supplied it are read together, because they are
	// one answer: "The Hanger" set by an owner and "Roku Streaming Stick" printed
	// at a factory arrive through the same field and only their source tells them
	// apart.
	name, which := pickField(doc.UserDeviceName, doc.FriendlyDeviceName, doc.DefaultDeviceName)
	return DeviceInfo{
		Name:       name,
		NameSource: nameSourceAt(which),
		// The model ladder answers "what KIND of thing is this", which is the
		// question an operator deciding whether to adopt a row is asking. WHICH
		// unit it is they answer three other ways in the same view — the name the
		// owner typed, the address, and the serial in the adopt dialog.
		//
		// WHAT WAS MEASURED, and it is one device: 192.168.50.31 answers
		// `model-name=100012587` (a retailer's part number),
		// `model-number=L810X` (Roku's hardware catalog code) and
		// `friendly-model-name=onn•Roku TV`. Only the third answers the question,
		// and the ladder is ordered on that single observation plus the shape of
		// the three elements: a catalog code is machine-grade by construction, so
		// `model-number` stays the last rung whatever else is true.
		//
		// WHAT WAS NOT MEASURED, stated because the ordering rests on it: no Roku
		// -branded box, no TCL, no Hisense, no second firmware. The failure mode
		// that would make this column WORSE than it is today is a firmware whose
		// `friendly-model-name` carries only a vendor string ("TCL") while
		// `model-name` carries the real SKU ("TCL 55S435") — every TV in such a
		// fleet would collapse to one indistinguishable label. No such document has
		// been seen, and none has been ruled out either. What would detect it is a
		// second Roku of a different make in the lab; until one exists this rung is
		// a one-device generalisation and should be read as one.
		//
		// The trade is taken because the two directions are not symmetric: an
		// operator can recover WHICH unit a row is from Name, Address or Serial,
		// but nobody can recover a model from a part number. If a SKU is genuinely
		// wanted it should become a SECOND rendered field, not a worse value in the
		// only one.
		//
		// NO SOURCE RIDES OUT WITH IT, and that is a decision, not the omission
		// NameSource exists to correct. A source is only worth carrying if some
		// merge downstream must REFUSE a value, and Model has exactly one writer in
		// the whole relay: this probe. Every other lane reports it empty, and both
		// merge layers already refuse empty, so there is no competing report for a
		// rank to rule against. Making one durable would also pin it: Model has no
		// continuously sweeping producer at all (passive sightings never probe), so
		// under deviceplane's own invariant — a rank may not sit above any rank a
		// sweeping lane can produce — every rung would be claimable once and then
		// permanent. That is harmless only because a TV cannot change model, which
		// is the argument AGAINST building it: a rank that can never be wrong is a
		// rank that is never needed. Presence-only is additionally what ALLOWS a
		// stored part number to be corrected, since a non-empty "onn•Roku TV"
		// replaces it with no rank to argue past — but ALLOWING is all it does. The
		// correction happens only if this probe RUNS, and on box .12 as configured
		// today it does not: see the reachability note on QueryDeviceInfo below.
		// The day a second lane learns to report a model, this needs a source and
		// may legally have one; internal/app/store/discovereddevices.go records
		// that trigger.
		Model: infoField(doc.FriendlyModelName, doc.ModelName, doc.ModelNumber),
		// Serial has ONE candidate, so infoField here is the honest wrapper and not
		// a discarded precedence: there is no ordering to lose. It stays that way
		// on purpose. The same document carries `<device-id>`, which is Roku's
		// cloud identifier for the unit and not the serial printed on it — a
		// tempting-looking fallback that would silently populate the field
		// identifying physical hardware with a different namespace's value.
		SerialNumber: infoField(doc.SerialNumber),
	}, nil
}

// NameSource names the `<device-info>` element a resolved Name came from.
//
// It is ECP's own vocabulary and deliberately not the device plane's rank type:
// this package speaks one vendor's protocol and should not import the plane's
// merge policy to describe its own document. The composition root maps the two
// (cmd/waiveo-relay/deviceplane.go), which is the same seam the Identify probe
// is already wired through.
type NameSource int

const (
	// NameSourceNone is "the document carried no usable name at all".
	NameSourceNone NameSource = iota
	// NameSourceDefault is `default-device-name`, the factory string — a model
	// label on a device nobody has renamed.
	NameSourceDefault
	// NameSourceFriendly is `friendly-device-name`, the vendor's display name.
	NameSourceFriendly
	// NameSourceUser is `user-device-name`, the name the owner typed in.
	NameSourceUser
)

// nameSourceAt maps pickField's candidate index — the ORDER QueryDeviceInfo
// passes them in — onto the source it stands for. Kept beside that call so the
// two lists cannot drift apart silently.
func nameSourceAt(which int) NameSource {
	switch which {
	case 0:
		return NameSourceUser
	case 1:
		return NameSourceFriendly
	case 2:
		return NameSourceDefault
	default:
		return NameSourceNone
	}
}

// infoField returns the first candidate that carries a usable value, trimmed and
// capped. Trimming matters because Roku's XML pretty-prints some elements onto
// their own lines, so a naive read picks up the surrounding whitespace and turns
// a present value into one that renders as blank in an operator's device list.
//
// A candidate over the byte cap is SKIPPED rather than truncated, and the next
// one is tried: a truncated serial is a wrong serial, and a truncated name reads
// as a real one. Falling through to the next candidate (or to empty) is the
// honest answer.
func infoField(candidates ...string) string {
	v, _ := pickField(candidates...)
	return v
}

// pickField is infoField plus the answer to "which one won" — the index of the
// candidate that supplied the value, or -1 when none did. A caller that needs
// only the string uses infoField; a caller that must RANK the value needs to
// know which element it came from, and re-deriving that at the call site would
// duplicate the trim-and-cap rules that decide it.
func pickField(candidates ...string) (string, int) {
	for i, c := range candidates {
		v := strings.TrimSpace(c)
		if v == "" || len(v) > maxDeviceInfoFieldBytes {
			continue
		}
		return v, i
	}
	return "", -1
}
