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
	"os/exec"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
)

const (
	// ClassUnclassified matches the neighbour lane: avahi NAMES a host, it does
	// not classify it — the device_class stays whatever it already is until a
	// pattern recognises it.
	ClassUnclassified = "unclassified"

	defaultInterval = 30 * time.Second
	browseTimeout   = 10 * time.Second
)

// Service is one resolved mDNS service: the instance name avahi shows and the
// IPv4 address it resolved to. The service type and TXT are not carried — this
// lane's job is the NAME; the richer facts a classifier wants are a later slice.
type Service struct {
	Name    string
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
	type named struct{ name, addr string }
	best := map[string]named{}
	for _, s := range services {
		if s.Address == "" {
			continue
		}
		name := cleanName(s.Name)
		if name == "" {
			continue
		}
		mac, ok := l.resolveMAC(s.Address)
		if !ok {
			continue // cross-subnet / not in the neighbour table — deferred
		}
		if cur, seen := best[mac]; !seen || nameScore(name) > nameScore(cur.name) {
			best[mac] = named{name: name, addr: s.Address}
		}
	}

	now := l.nowMillis()
	for mac, n := range best {
		driver, nativeID, match, ok := deviceplane.MACIdentity(mac)
		if !ok {
			continue
		}
		l.store.Observe(deviceplane.Observation{
			Match:       match,
			Provenance:  deviceplane.ProvenanceDiscovered,
			Driver:      driver,
			NativeID:    nativeID,
			DeviceClass: ClassUnclassified,
			Name:        n.name,
			Address:     n.addr,
		}, now)
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
// parses the resolved (`=`) IPv4 records. `-t` terminates after the cache is
// dumped (a poll, not a live browse); `-r` resolves each service to an address;
// `-p` is the stable parseable format.
func browseAvahi() ([]Service, error) {
	ctx, cancel := context.WithTimeout(context.Background(), browseTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "avahi-browse", "-a", "-t", "-r", "-p").Output()
	if err != nil {
		// A non-zero exit or a timeout is not fatal to discovery: the cache is
		// optional enrichment. Report no services rather than an error the caller
		// would have to special-case; the next sweep tries again.
		return nil, nil
	}
	return parseAvahi(string(out)), nil
}

// parseAvahi extracts (name, address) from avahi-browse -p resolved lines.
//
// A resolved line is `=;<iface>;<proto>;<name>;<type>;<domain>;<host>;<addr>;<port>;<txt…>`.
// Only IPv4 records are taken: an IPv6 mDNS address is link-local, which the
// neighbour lane deliberately does not carry, so it could never resolve to a
// MAC anyway. A name containing `@` is skipped — that is avahi's
// hardware-addressed alias form (`AABBCC@Name`, the AirTunes/RAOP variant), and
// the clean human name (`Name`) is advertised beside it; keeping the alias
// would either duplicate the merge or overwrite the real name with a MAC prefix.
func parseAvahi(out string) []Service {
	var services []Service
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, ";")
		if len(fields) < 8 || fields[0] != "=" || fields[2] != "IPv4" {
			continue
		}
		name := unescapeAvahi(fields[3])
		if name == "" || strings.Contains(name, "@") {
			continue
		}
		addr := strings.TrimSpace(fields[7])
		if addr == "" {
			continue
		}
		services = append(services, Service{Name: name, Address: addr})
	}
	return services
}

// unescapeAvahi decodes avahi's parseable-format escapes: `\NNN` is one byte
// given as three decimal digits (a space is `\032`, a UTF-8 apostrophe arrives
// as `\226\128\153`), and `\\` is a literal backslash. Anything else after a
// backslash is left as written rather than guessed at.
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
		if i+1 < len(s) && s[i+1] == '\\' {
			b.WriteByte('\\')
			i++
			continue
		}
		if i+3 < len(s) && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]) {
			n := int(s[i+1]-'0')*100 + int(s[i+2]-'0')*10 + int(s[i+3]-'0')
			if n <= 255 {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte('\\')
	}
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
