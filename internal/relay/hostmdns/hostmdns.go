// Package hostmdns enriches discovered hosts with the names the HOST's avahi
// daemon already learned over mDNS — the L6 decision (Discovery spec §11):
// read the host avahi cache rather than bind 5353 ourselves. Host avahi is this
// platform's canonical mDNS stack (the container's own avahi was removed over
// the waiveo.local collision), so reading its cache gives mDNS visibility by
// default without a second listener fighting it for the multicast group.
//
// It NAMES rather than mints: every service avahi resolved is correlated to a
// host the neighbour lane already knows (by IP → MAC) and merged onto that MAC
// candidate, contributing the human instance name. So a device that is
// mDNS-advertising but SSDP-quiet — a Roku still announcing AirPlay while it
// ignores an SSDP M-SEARCH — gets its real name onto the row it already has,
// instead of staying an anonymous MAC.
//
// It reads the cache via `avahi-browse`, so it is a local read like the
// neighbour lane, not a scan (avahi populated the cache from ordinary mDNS
// traffic); it runs with the discovery capability by default.
//
// KNOWN LIMITATION (deferred): a service avahi sees that is NOT in this relay's
// neighbour table — a cross-subnet device reachable only through mDNS
// reflection — has no MAC to key on and is skipped. Minting it needs a stable
// non-MAC identity (§4.1's cross-subnet case), which is its own slice; this one
// enriches the local segment, which is what the relay is an appliance for.
package hostmdns

import (
	"context"
	"errors"
	"net/netip"
	"os/exec"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
)

const (
	defaultInterval = 30 * time.Second
	browseTimeout   = 10 * time.Second
)

// Service is one resolved mDNS service: the instance name avahi shows, the DNS
// service type (`_airplay._tcp`), and the IPv4 address it resolved to. The type
// is Discovery's own GENERIC classification signal — a passive fact, from a
// cache avahi already holds, so it needs no active probe (review H1/H2). It is
// distinct from an EXTENSION pattern, which stays app-side: this is Discovery's
// built-in guess (spec §5), a sensible default an extension can later refine.
type Service struct {
	Name    string
	Type    string
	Address string
}

// Config configures a Lane.
type Config struct {
	// Store is the shared candidate store. Required.
	Store *deviceplane.Store
	// NowMillis stamps each Observe. Required.
	NowMillis func() int64
	// ResolveMAC maps a service's IP to the MAC the neighbour lane saw it at, so
	// the name merges onto the canonical MAC candidate rather than mint a row.
	// Required: with no resolver there is nothing to merge onto, and minting
	// under a non-MAC identity is the deferred cross-subnet case.
	ResolveMAC func(ip string) (string, bool)
	// Interval is the browse cadence. Zero or negative defaults to 30s.
	Interval time.Duration
	// Browse returns the resolved services in the host avahi cache. The seam:
	// production execs avahi-browse; a test injects services. Zero uses the
	// avahi reader.
	Browse func() ([]Service, error)
}

// Lane periodically reads the host avahi cache and merges each named,
// MAC-resolvable service onto its candidate.
type Lane struct {
	store      *deviceplane.Store
	nowMillis  func() int64
	resolveMAC func(ip string) (string, bool)
	interval   time.Duration
	browse     func() ([]Service, error)
}

// New returns a Lane for cfg.
func New(cfg Config) (*Lane, error) {
	if cfg.Store == nil {
		return nil, errors.New("hostmdns: Config.Store must not be nil")
	}
	if cfg.NowMillis == nil {
		return nil, errors.New("hostmdns: Config.NowMillis must not be nil")
	}
	if cfg.ResolveMAC == nil {
		return nil, errors.New("hostmdns: Config.ResolveMAC must not be nil")
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	browse := cfg.Browse
	if browse == nil {
		browse = browseAvahi
	}
	return &Lane{store: cfg.Store, nowMillis: cfg.NowMillis, resolveMAC: cfg.ResolveMAC, interval: interval, browse: browse}, nil
}

// Run reads the cache immediately, then every Interval, until ctx is cancelled.
// A browse error is non-fatal: the cache is transient, and one failed read must
// not stop the lane.
func (l *Lane) Run(ctx context.Context) error {
	l.sweep()
	t := time.NewTicker(l.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			l.sweep()
		}
	}
}

// sweep reads the cache once and merges the BEST name per host.
//
// One device advertises several mDNS services with names of very different
// quality — a Roku answers AirPlay as "The Hanger" and Spotify as a bare UUID —
// so a last-writer merge would leave a device showing whichever service was
// parsed last, often the ugly one. Instead the sweep groups resolved services
// by their MAC, cleans each name, and keeps the most human of them, then
// Observes each host once. A device whose only names are machine ids (a bare
// UUID, a hex blob) contributes no name and stays its neighbour identity rather
// than being labelled with a hardware string no operator would recognise.
func (l *Lane) sweep() {
	services, err := l.browse()
	if err != nil {
		return
	}
	type host struct {
		name      string
		nameRank  deviceplane.NameRank
		addr      string
		class     string
		classAuth int
	}
	best := map[string]*host{}
	for _, s := range services {
		if s.Address == "" {
			continue
		}
		mac, ok := l.resolveMAC(s.Address)
		if !ok {
			continue // cross-subnet / not in the neighbour table — deferred
		}
		h := best[mac]
		if h == nil {
			h = &host{class: deviceplane.ClassUnclassified, addr: s.Address}
			best[mac] = h
		}
		// The best NAME across the host's services, ranked by WHICH SERVICE said
		// it first and only then by how the string looks. Shape alone is what
		// this used to do, and it is a proxy that inverts on real devices: an
		// ecobee announces "Upstairs" over HomeKit and "ecobee-ares" over its own
		// service, and the len+space score prefers the machine name. The service
		// type is the fact the shape was standing in for, and unlike the shape it
		// also travels upward, so the store can refuse a worse-sourced name on a
		// later sweep instead of re-deciding from scratch every 30 seconds.
		if name := cleanName(s.Name); name != "" {
			if rank := nameRankFor(s.Type); betterName(rank, name, h.nameRank, h.name) {
				h.name, h.nameRank = name, rank
			}
		}
		// The best-AUTHORITY class its service types imply, decided by betterClass
		// for exactly the reason betterName decides the name: a device advertises
		// several types, they disagree, and the answer must be a function of the
		// cache rather than of the order avahi happened to list it in.
		if c := classFor(s.Type); betterClass(classAuthority(s.Type), c, h.classAuth, h.class) {
			h.class, h.classAuth = c, classAuthority(s.Type)
		}
	}

	now := l.nowMillis()
	for mac, h := range best {
		driver, nativeID, match, ok := deviceplane.MACIdentity(mac)
		if !ok {
			continue
		}
		l.store.Observe(deviceplane.Observation{
			Match:       match,
			Provenance:  deviceplane.ProvenanceDiscovered,
			Driver:      driver,
			NativeID:    nativeID,
			DeviceClass: h.class,
			Name:        h.name,
			NameRank:    h.nameRank,
			Address:     h.addr,
		}, now)
	}
}

// classFor maps an mDNS service type to Discovery's generic device class, or
// ClassUnclassified when the type says nothing about what a device IS (a plain
// _http or _ssh is any computer). The map is deliberately small and only claims
// what a service type reliably implies; a precise identity is an extension's
// pattern to assert, not this generic guess.
func classFor(serviceType string) string {
	switch serviceType {
	case "_airplay._tcp", "_raop._tcp", "_googlecast._tcp", "_spotify-connect._tcp",
		"_sonos._tcp", "_roku._tcp", "_androidtvremote2._tcp":
		return "media-player"
	case "_ipp._tcp", "_ipps._tcp", "_printer._tcp", "_pdl-datastream._tcp", "_scanner._tcp", "_uscan._tcp", "_uscans._tcp":
		return "printer"
	case "_smb._tcp", "_afpovertcp._tcp", "_nfs._tcp", "_ftp._tcp", "_webdav._tcp":
		return "storage"
	// `_home-assistant` (a hub/controller) and `_ecobee` (a thermostat) are
	// definitive home-automation signals a device advertises on its own — a Home
	// Assistant box showed unclassified until this line because it advertises
	// neither HomeKit nor Matter, only its own service. A device that ALSO
	// advertises a media type (the ecobee at 192.168.39.241 carries `_airplay`
	// and `_spotify-connect`) no longer classifies by whichever type the sweep
	// read first: classAuthority ranks the SOURCE, so the thermostat's own
	// product service outranks the media features bolted onto it.
	case "_hap._tcp", "_homekit._tcp", "_matter._tcp", "_matterc._tcp", "_hue._tcp",
		"_home-assistant._tcp", "_ecobee._tcp":
		return "smart-home"
	default:
		return deviceplane.ClassUnclassified
	}
}

// betterName decides which of two of one host's service names this sweep should
// report: the better-ranked source, then the more human-looking string, then —
// and this last one is the point — the lexicographically smaller name.
//
// The final tiebreak exists because avahi's browse output has no guaranteed
// order, so without it a host announcing two DIFFERENT equally-ranked,
// equally-scoring names would report whichever record the dump happened to list
// first, and could report the other one 30 seconds later. That is #198's
// signature arriving from a direction the store's merge cannot see: keepName
// takes an equal-ranked newer name on purpose (it is how a rename lands), so a
// lane that hands it a coin flip every sweep produces a permanent flap that
// looks like a permanent rename. A sweep must be a FUNCTION of the cache it
// read, not of the order it read it in.
func betterName(rank deviceplane.NameRank, name string, heldRank deviceplane.NameRank, held string) bool {
	if rank != heldRank {
		return rank > heldRank
	}
	if s, hs := nameScore(name), nameScore(held); s != hs {
		return s > hs
	}
	return name < held
}

// nameRankFor says how good a name announced under this mDNS service type is —
// the source half of deviceplane.NameRank, authored here because this is where
// the knowledge lives, next to classFor which answers the OTHER question about
// the same record. The two disagree on purpose: `_ecobee._tcp` is a definitive
// smart-home CLASS signal and a terrible NAME source ("ecobee-ares"), and one
// table could not say both.
//
// EVERY ENTRY BELOW THE DEFAULT IS A SIGHTING ON THE LAB LAN, quoted from one
// `avahi-browse -a -t -r -p -k` on box .12 — the exact command browseAvahi runs.
// That bar is not decoration. A friendly entry makes a type's name STICKY (the
// store will refuse worse-ranked names for the device afterwards), so a type
// promoted on plausibility rather than evidence is a name that cannot be
// corrected. The first draft of this table promoted four types nobody had
// looked at, and the LAN falsified one of them — see `_display._tcp` below.
//
//   - FRIENDLY types are the ones whose instance label is a name somebody CHOSE
//     for the device, INTACT. `_androidtvremote2._tcp | onn. 4K Streaming Box`
//     against `_googlecast._tcp | onn.-4K-Streaming-Bo-89edfc7ba221…` for one
//     box (192.168.50.63) is the sighting this whole rank exists for;
//     `_hap._tcp | Upstairs` against `_ecobee._tcp | ecobee-ares` for one
//     thermostat (192.168.39.241) is the second; `_airplay._tcp | The Hanger`
//     (192.168.50.31) and `_companion-link._tcp | Matt’s MacBook Pro`
//     (192.168.50.35) are the rest.
//   - MODEL types announce the product, not the unit: `_ipp._tcp`,
//     `_printer._tcp`, `_pdl-datastream._tcp`, `_scanner._tcp` and `_uscan._tcp`
//     all announce "Brother MFC-L2730DW series" for 192.168.50.36, whatever its
//     owner calls it.
//   - MACHINE is everything else, and it is wider than "an id". It is any label
//     the device DERIVED rather than any label a person wrote: ids
//     (`_spotify-connect._tcp | d3b79a8f-0c4e-…`, `_matter._tcp |
//     D731507D2F318A3E-…`), hostname forms, and LOSSY REWRITES of a chosen name.
//
// `_display._tcp` IS THAT LOSSY REWRITE, and it is why this list is now quoted
// rather than reasoned. It looks like the ideal friendly type — a TV's display
// name — and on short names it is one (`_display._tcp | The Hanger`). But the
// same sweep that produced that line also produced these two pairs:
//
//	192.168.39.110  _airplay | 43in office downstairs        _display | 43" office downs-0JX
//	192.168.39.238  _airplay | Office Upstairs small Bedroom  _display | Office Upstairs -6A5
//
// It truncates to 20 characters and appends a `-XXX` disambiguator — the same
// string class as `onn.-4K-Streaming-Bo`, the truncation this rank was created
// to demote. Ranked friendly it ties with `_airplay`, so a sweep whose browse
// held `_display` and not `_airplay` reproduced #198 exactly, on a different
// pair of records, AFTER the fix. Cache presence really does vary that way:
// three consecutive `-t` dumps 12s apart contained `_googlecast._tcp` for
// 192.168.50.63 and no `_androidtvremote2._tcp`, which a live browse then
// surfaced immediately.
//
// A hostname is deliberately NOT friendly either, however human it looks. `_ssh`,
// `_sftp-ssh` and `_smb` advertise readable host names ("MacMiniM4Lab", "NAS"),
// which is exactly why the temptation is there — but the same Mac announces
// "Matt’s MacBook Pro" over AirPlay and a hostname form over SSH. A hostname
// still NAMES a device nothing else names; it just must not displace a chosen
// one.
//
// An UNRECOGNISED type is NameRankMachine, not NameRankNone, and that choice is
// the conservative one in both directions: a name from a type this table has
// never heard of still fills an empty slot, competes on shape against other
// unranked names exactly as it did before this table existed, and can never
// displace a name a KNOWN-friendly record authored.
func nameRankFor(serviceType string) deviceplane.NameRank {
	switch serviceType {
	case "_airplay._tcp", "_androidtvremote2._tcp", "_hap._tcp",
		"_home-assistant._tcp", "_companion-link._tcp":
		return deviceplane.NameRankFriendly
	case "_ipp._tcp", "_ipps._tcp", "_printer._tcp", "_pdl-datastream._tcp",
		"_scanner._tcp", "_uscan._tcp", "_uscans._tcp":
		return deviceplane.NameRankModel
	default:
		// Includes every observed derived-label source — `_display._tcp` and
		// `_googlecast._tcp` (truncations), `_spotify-connect._tcp`,
		// `_matter._tcp`, `_raop._tcp`, `_ecobee._tcp`, `_sideplay._tcp`,
		// `_smb._tcp`, `_ssh._tcp`, `_sftp-ssh._tcp`, `_apple-mobdev2._tcp` —
		// and every type nobody has looked at yet, which are treated alike
		// because "I have no reason to trust this label" is the honest reading
		// of both. `_sonos._tcp` and `_roku._tcp` were in the friendly list on
		// speculation and are here now: neither appears on the lab LAN, and
		// Roku does not advertise itself over mDNS at all (it is an SSDP/ECP
		// device, which is why the ECP probe exists).
		return deviceplane.NameRankMachine
	}
}

// The authority of a service type's CLASS signal: how strongly the fact that a
// device advertises this type asserts what the device IS.
//
// This exists because "most specific wins" was a rank with only two values, so
// two DIFFERENT specific classes tied and the first one parsed simply held. That
// was written off as "a rare edge not worth a hierarchy", and while the lane
// filtered on the avahi transaction it was nearly unreachable. #203 makes it
// reachable — widening the parse to the records avahi cached on the v6
// transaction feeds classFor records it never saw before — and the merge below
// this lane makes it DURABLE: deviceplane.keepClass and the store's
// keepClassFact both treat one specific class replacing another as a genuine
// reclassification and take it. An order-dependent pick, taken durably, every 30
// seconds, is #198's signature on the class field.
const (
	classAuthorityNone = iota
	// A FEATURE any kind of device may implement, including fabric membership.
	// Every entry is a lab sighting on a device of a different kind:
	// `_matter._tcp` on 192.168.50.43, a Google speaker (OUI 84:28:59) whose
	// other service is `_spotify-connect._tcp`; `_hap._tcp` on the ecobee
	// thermostats at 192.168.39.241/242; `_airplay._tcp` AND
	// `_spotify-connect._tcp` on that same ecobee at 192.168.39.241, which is the
	// sighting that refutes ranking media above smart-home outright — the
	// thermostat really does advertise both, and it is not a media player.
	// `_smb._tcp` (192.168.51.147) and `_afpovertcp._tcp` (192.168.50.57) are
	// here on the same principle: any computer shares files.
	classAuthorityFeature
	// The device's OWN product service — advertised because of what the device
	// is, not as a feature bolted onto something else.
	classAuthorityProduct
)

// classAuthority says which of the three a service type is. A type classFor does
// not recognise has no class signal at all; a recognised type defaults to
// FEATURE, the weaker of the two real levels, so a type added to classFor
// without a deliberate decision here can never silently outrank an established
// product signal — the same conservative default nameRankFor takes.
func classAuthority(serviceType string) int {
	if classFor(serviceType) == deviceplane.ClassUnclassified {
		return classAuthorityNone
	}
	switch serviceType {
	// PRODUCT, and every one is a lab sighting: `_ecobee._tcp` (192.168.39.241,
	// beside the `_airplay`/`_spotify-connect` it must outrank),
	// `_home-assistant._tcp` (192.168.50.126 "HA-Barn"),
	// `_androidtvremote2._tcp` and `_googlecast._tcp` (192.168.50.63, the onn
	// box), and the printer set on 192.168.50.36, a Brother MFC-L2730DW —
	// nothing that is not a printer answers IPP. `_ipps`/`_uscans` ride with the
	// sighted `_ipp`/`_uscan` as their TLS spellings.
	//
	// `_sonos._tcp`, `_roku._tcp` and `_hue._tcp` are deliberately NOT here
	// despite being vendor services: none has ever appeared on the lab LAN, and
	// promoting a type nobody has looked at is what put `_display._tcp` in the
	// friendly name list. They classify perfectly well at FEATURE — the level
	// only decides ties.
	case "_ecobee._tcp", "_home-assistant._tcp",
		"_androidtvremote2._tcp", "_googlecast._tcp",
		"_ipp._tcp", "_ipps._tcp", "_printer._tcp", "_pdl-datastream._tcp",
		"_scanner._tcp", "_uscan._tcp", "_uscans._tcp":
		return classAuthorityProduct
	default:
		return classAuthorityFeature
	}
}

// betterClass decides which of two of one host's service types should say what
// the device is: the higher-authority signal, then the more concrete class.
//
// It is betterName's shape, and it is a total order for betterName's reason — a
// sweep must be a FUNCTION of the cache it read, not of the order it read it in.
// Both steps are load-bearing on live data:
//
//   - AUTHORITY settles the ecobee at 192.168.39.241, which advertises `_ecobee`
//     (smart-home) and `_airplay` + `_spotify-connect` (media-player). Product
//     beats feature, so it stays smart-home.
//   - CONCRETENESS settles 192.168.50.43, a Google speaker advertising `_matter`
//     (smart-home) and `_spotify-connect` (media-player) — both FEATURE, so
//     authority ties. Fabric membership is the usual way a smart-home class is
//     produced at this level, and it says far less about a device than a
//     concrete media/print/storage function does, so the concrete one wins and
//     the speaker stays media-player.
//
// The second step is a documented policy rather than a lexicographic tiebreak on
// purpose: alphabetical order would give the same answer here only by accident,
// and would silently invert if a class token were ever respelled.
func betterClass(auth int, class string, heldAuth int, held string) bool {
	if auth == classAuthorityNone {
		return false
	}
	if auth != heldAuth {
		return auth > heldAuth
	}
	if class != held {
		return classConcreteness(class) > classConcreteness(held)
	}
	return false
}

// classConcreteness ranks how much a class narrows what an operator can DO with
// a device — device_class governs the command vocabulary (REG-052). A media
// player, a printer and a storage box each name a concrete function; smart-home
// is the broad bucket a bare fabric membership lands in.
func classConcreteness(class string) int {
	switch class {
	case "", deviceplane.ClassUnclassified:
		return 0
	case "smart-home":
		return 1
	default:
		return 2
	}
}

// cleanName turns an mDNS instance name into a display name, or "" when there
// is no human name in it. It first strips a trailing hardware-id suffix — the
// long hex tail Google Cast and similar append to a friendly name
// (`onn 4K Box-89edfc…`) — then rejects a name that is nothing BUT a machine id
// (a UUID, or a hex/colon blob), because a hex string is worse than the
// neighbour identity it would replace.
func cleanName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if i := strings.LastIndexByte(name, '-'); i > 0 && isHexBlob(name[i+1:]) && len(name[i+1:]) >= 12 {
		name = strings.TrimSpace(name[:i])
	}
	if isHexBlob(name) {
		return ""
	}
	return name
}

// nameScore ranks display names so the most human wins a device with several:
// a name with a space (`The Hanger`) beats one without (`The-Hanger`), and a
// longer name beats a shorter one at equal spacing. It only ever compares names
// cleanName already admitted, so it need not re-check for machine ids.
func nameScore(name string) int {
	score := len(name)
	if strings.Contains(name, " ") {
		score += 1000
	}
	return score
}

// isHexBlob reports whether s, once its id separators (`:` `-` `.`) are
// dropped, is a non-empty run of at least 8 hex digits and nothing else — a
// UUID, a MAC, or a device-id string, none of which is a name.
func isHexBlob(s string) bool {
	hex := 0
	for _, c := range s {
		switch {
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
			hex++
		case c == ':' || c == '-' || c == '.':
		default:
			return false
		}
	}
	return hex >= 8
}

// browseAvahi dumps the host avahi cache via `avahi-browse -a -t -r -p` and
// parses the resolved (`=`) records that carry an IPv4 address — whichever
// transaction the cache happens to hold them on (parseAvahi). `-t` terminates
// after the cache is dumped (a poll, not a live browse); `-r` resolves each
// service to an address; `-p` is the stable parseable format.
func browseAvahi() ([]Service, error) {
	ctx, cancel := context.WithTimeout(context.Background(), browseTimeout)
	defer cancel()
	// -k keeps the service type in its DNS form (`_airplay._tcp`) rather than a
	// localised human string, so classFor matches a stable token, not a label.
	out, err := exec.CommandContext(ctx, "avahi-browse", "-a", "-t", "-r", "-p", "-k").Output()
	if err != nil {
		// A non-zero exit or a timeout is not fatal to discovery: the cache is
		// optional enrichment. Report no services rather than an error the caller
		// would have to special-case; the next sweep tries again.
		return nil, nil
	}
	return parseAvahi(string(out)), nil
}

// parseAvahi extracts (name, type, address) from avahi-browse -p resolved lines.
//
// A resolved line is `=;<iface>;<proto>;<name>;<type>;<domain>;<host>;<addr>;<port>;<txt…>`.
// A name containing `@` is skipped — that is avahi's hardware-addressed alias
// form (`AABBCC@Name`, the AirTunes/RAOP variant), and the clean human name
// (`Name`) is advertised beside it; keeping the alias would either duplicate the
// merge or overwrite the real name with a MAC prefix.
//
// # The IPv4 test is on the RESOLVED ADDRESS, never on the transaction
//
// The lane must only ever hand an IPv4 address upward: the address is what the
// sweep gives ResolveMAC to look up in the kernel neighbour table, and the
// device plane it feeds is IPv4 by contract (SSDP is IPv4-multicast). So the
// filter is real and stays. What it must ask is which address avahi RESOLVED
// the service to — field 7 — and that is NOT what `<proto>` in field 2 says.
//
// Field 2 is the protocol of the avahi TRANSACTION the cached record arrived
// on, and avahi caches one service under both when it hears it on both. The two
// are independent, and BOTH mismatches are live on box .12 in a single
// `avahi-browse -a -t -r -p -k` dump:
//
//	=;eth0;IPv6;onn\.\0324K\032Streaming\032Box;_androidtvremote2._tcp;local;…;192.168.50.63;6466
//	=;eth0;IPv4;4AB0E26A2FDE09DE-000000000001B669;_matter._tcp;local;…;fe80::e350:1ce0:2528:f37e;33969
//
// The first is a perfectly good IPv4 address on the v6 transaction, and the
// transaction test threw it away wholesale — the name, the class signal and the
// port with it. That record is the Friendly-ranked source of the onn box's
// display name (nameRankFor), so discarding it left the Machine-ranked
// `_googlecast` truncation "onn.-4K-Streaming-Bo" as the best name the lane
// could offer, and measured runs discarded it 16 dumps out of 16. The second is
// the mirror image the same test let THROUGH: a link-local v6 address on the v4
// transaction, handed to ResolveMAC as though it were a v4 host, where it can
// only ever miss.
//
// Reading the address itself refuses exactly the records that cannot be used —
// the `_googlecast` record resolving to fe80::225f:9d9b:8178:b8e3 in that same
// dump is still dropped — and admits exactly the ones that can. A v4-mapped
// v6 literal (`::ffff:192.168.50.63`) is deliberately NOT accepted: the string
// is used verbatim as the neighbour-table lookup key, so a form the table never
// spells could not resolve and would be a silent miss rather than a refusal.
//
// Admitting both transactions has two consequences for the sweep, and BOTH ride
// on the picks being total orders rather than on luck.
//
// One service can now be parsed twice with the same name, type and address —
// avahi really does cache one service under both, and 192.168.50.48 carries the
// same `_matter._tcp` instance on each. A duplicate cannot change the answer
// because betterName and betterClass both refuse an equal candidate.
//
// More importantly the sweep now sees records it never saw before, and the name
// is not the only thing it decides from them: classFor reads the same widened
// set. A name could only improve — betterName is a maximum over a total order,
// so a larger input set cannot make it worse — but the class had no such
// property until betterClass, because two specific classes used to tie and the
// first parsed simply held. Widening the input to a first-parsed-wins pick is
// how a correct parser fix reaches an operator as a device silently changing
// what it is; see the comment on classAuthority.
func parseAvahi(out string) []Service {
	var services []Service
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, ";")
		if len(fields) < 8 || fields[0] != "=" {
			continue
		}
		name := unescapeAvahi(fields[3])
		if name == "" || strings.Contains(name, "@") {
			continue
		}
		addr := strings.TrimSpace(fields[7])
		if !isIPv4(addr) {
			continue
		}
		services = append(services, Service{Name: name, Type: strings.TrimSpace(fields[4]), Address: addr})
	}
	return services
}

// isIPv4 reports whether addr is a dotted-quad IPv4 literal — the only address
// shape this lane may pass on, for the reasons parseAvahi gives.
//
// netip.ParseAddr with Is4 rather than net.ParseIP with To4: To4 also answers
// yes for a v4-mapped v6 literal, which is not a string the neighbour table is
// keyed by.
func isIPv4(addr string) bool {
	ip, err := netip.ParseAddr(addr)
	return err == nil && ip.Is4()
}

// unescapeAvahi decodes avahi's parseable-format escapes. The format has two:
// `\NNN`, one byte given as three decimal digits (a space is `\032`; a UTF-8
// codepoint arrives as its bytes, so an apostrophe is `\226\128\153`); and
// `\X`, a single literal character the format would otherwise read as structure
// — `\.` (a dot inside one label, as in the streaming brand "onn."), `\\` (a
// literal backslash), `\;` (a literal field separator). In BOTH forms the
// backslash is the escape and must be consumed; only a trailing backslash with
// nothing after it is preserved as written.
func unescapeAvahi(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		// `\NNN`: three decimal digits are one byte. Checked first so a real
		// digit escape is never mistaken for `\X` swallowing its first digit.
		if i+3 < len(s) && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]) {
			n := int(s[i+1]-'0')*100 + int(s[i+2]-'0')*10 + int(s[i+3]-'0')
			if n <= 255 {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		// `\X`: the character is literal, the backslash is not. This is the case
		// the first version dropped — it kept the backslash, so `onn\. 4K`
		// reached an operator as `onn\. 4K` rather than `onn. 4K`. `\\` is just
		// this case with X a backslash, so it needs no separate branch.
		if i+1 < len(s) {
			b.WriteByte(s[i+1])
			i++
			continue
		}
		// A trailing backslash with nothing to escape: preserve it as written.
		b.WriteByte('\\')
	}
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
