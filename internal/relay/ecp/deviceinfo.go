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
	// Model is the human-readable model name when the device reports one, else
	// its model number — a number is a worse label than a name but a much
	// better one than nothing.
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
type deviceInfoDoc struct {
	XMLName            xml.Name `xml:"device-info"`
	SerialNumber       string   `xml:"serial-number"`
	ModelName          string   `xml:"model-name"`
	ModelNumber        string   `xml:"model-number"`
	UserDeviceName     string   `xml:"user-device-name"`
	FriendlyDeviceName string   `xml:"friendly-device-name"`
	DefaultDeviceName  string   `xml:"default-device-name"`
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

	return DeviceInfo{
		Name:         infoField(doc.UserDeviceName, doc.FriendlyDeviceName, doc.DefaultDeviceName),
		Model:        infoField(doc.ModelName, doc.ModelNumber),
		SerialNumber: infoField(doc.SerialNumber),
	}, nil
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
	for _, c := range candidates {
		v := strings.TrimSpace(c)
		if v == "" || len(v) > maxDeviceInfoFieldBytes {
			continue
		}
		return v
	}
	return ""
}
