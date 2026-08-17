// Package neighbor enumerates the relay host's own kernel neighbour (ARP/NDP)
// table into the shared candidate store: every L2-present host the kernel has
// already resolved becomes an UNCLASSIFIED candidate, keyed by its MAC — the
// one identity stable across every discovery lane (Discovery spec §4.1).
//
// This is the foundation of enumerate-all. Watch-driven discovery listed only
// the devices an extension had declared a pattern for (one, on the lab box);
// the neighbour table already holds every host that has exchanged a packet with
// this relay, so reading it turns "one device" into "the whole L2 segment" with
// no extension installed.
//
// It is a LOCAL READ, not a scan: the kernel populates the neighbour table from
// ordinary traffic, so this lane emits NOTHING on the wire. That is why it runs
// with the discovery capability by default, unlike the active scan lanes that
// probe the network and are opt-in (Discovery spec §8 / review H2).
package neighbor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/lanaddr"
)

const (
	// Driver is the discovery-source namespace for a host known only at L2. The
	// identity (Driver, MAC) is STABLE across classification (spec §4.1): when a
	// pattern later recognises this host, its device_class changes and this
	// identity does not, so a classified device is not a second row.
	Driver = "net"

	// ClassUnclassified is the device_class a host carries until an extension's
	// pattern recognises it. Non-empty on purpose — REL-110a requires a class,
	// and an empty one would make the whole candidate report unapplyable, so
	// "unclassified" is a real value, not a blank.
	ClassUnclassified = "unclassified"

	defaultInterval = 30 * time.Second
)

// Entry is one resolved neighbour: an IP and the MAC the kernel resolved it to.
// An entry with no hardware address (INCOMPLETE/FAILED) is not one of these —
// the reader drops it, because a host with no MAC has no stable identity.
type Entry struct {
	IP  string
	MAC string
}

// Config configures a Lane.
type Config struct {
	// Store is the shared candidate store every lane Observes into. Required.
	Store *deviceplane.Store
	// NowMillis stamps each Observe. Required — inject a fake in tests.
	NowMillis func() int64
	// Interval is the read cadence. Zero or negative defaults to 30s.
	Interval time.Duration
	// Read returns the current neighbour table. The seam: production reads the
	// kernel table (readKernelNeighbours); a test injects a fixed set without
	// exec'ing anything. Zero uses the kernel reader.
	Read func() ([]Entry, error)
}

// Lane periodically reads the neighbour table and Observes each L2-present host
// as an unclassified MAC-keyed candidate (REL-110/111, spec §4.1).
type Lane struct {
	store     *deviceplane.Store
	nowMillis func() int64
	interval  time.Duration
	read      func() ([]Entry, error)
}

// New returns a Lane for cfg.
func New(cfg Config) (*Lane, error) {
	if cfg.Store == nil {
		return nil, errors.New("neighbor: Config.Store must not be nil")
	}
	if cfg.NowMillis == nil {
		return nil, errors.New("neighbor: Config.NowMillis must not be nil")
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	read := cfg.Read
	if read == nil {
		read = readKernelNeighbours
	}
	return &Lane{store: cfg.Store, nowMillis: cfg.NowMillis, interval: interval, read: read}, nil
}

// Run reads the neighbour table immediately, then every Interval, until ctx is
// cancelled. A read error is non-fatal: the table is transient host state, and
// one failed read (a transient exec failure, a momentarily empty table) must
// not stop the lane from trying again — the next sweep recovers.
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

// sweep reads the table once and Observes every usable neighbour.
//
// A neighbour is usable when it has a MAC (its identity) AND a dialable LAN
// address (lanaddr.Host) — the same address policy the store enforces, applied
// here so a link-local or otherwise non-LAN neighbour becomes noise the lane
// never mints rather than a candidate the store later blanks the address of.
// Two neighbour rows for one host (its v4 and v6 entries share a MAC) dedup in
// the store by identity, and orKeep leaves whichever address was learned.
func (l *Lane) sweep() {
	entries, err := l.read()
	if err != nil {
		return
	}
	now := l.nowMillis()
	for _, e := range entries {
		// A neighbour is minted only with all three of: a dialable LAN address
		// (the store's own policy, applied here so a link-local neighbour is
		// never-minted noise rather than a candidate with a blanked address); a
		// valid MAC-OUI (the candidate's MAN-071 Match — a candidate whose Match
		// will not marshal is silently DROPPED at report time, so it is filtered
		// here at mint time instead); and the MAC as its identity.
		if !lanaddr.Host(e.IP) {
			continue
		}
		oui := macOUI(e.MAC)
		if oui == "" {
			continue
		}
		l.store.Observe(deviceplane.Observation{
			Match:       deviceplane.Match{MacOui: oui},
			Provenance:  deviceplane.ProvenanceDiscovered,
			Driver:      Driver,
			NativeID:    strings.ToLower(e.MAC),
			DeviceClass: ClassUnclassified,
			Address:     e.IP,
		}, now)
	}
}

// macOUI is the 6-hex-digit vendor prefix of a MAC (the manufacturer half): the
// candidate's Match, so a MAC-OUI pattern (MAN-071) an extension declares can
// later recognise this host. A kernel-resolved MAC is always well-formed; this
// is defensive against garbage input, returning "" (skip the neighbour) rather
// than a short prefix the Match's own MAN-071 encoding would reject.
func macOUI(mac string) string {
	hex := strings.ToLower(strings.ReplaceAll(mac, ":", ""))
	if len(hex) < 6 {
		return ""
	}
	for _, c := range hex[:6] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ""
		}
	}
	return hex[:6]
}

// readKernelNeighbours reads the host's neighbour table via `ip neigh show`.
// The command is present on every modern Linux and reads the table without
// privilege (it is a read of kernel state, not a change), so this lane needs no
// capability the relay does not already have.
func readKernelNeighbours() ([]Entry, error) {
	out, err := exec.Command("ip", "neigh", "show").Output()
	if err != nil {
		return nil, fmt.Errorf("neighbor: read neighbour table: %w", err)
	}
	return parseNeighbours(string(out)), nil
}

// parseNeighbours parses `ip neigh show` output. Each line is
// `<ip> dev <dev> lladdr <mac> <state>`; a line with no `lladdr` token
// (INCOMPLETE/FAILED — a neighbour the kernel could not resolve) has no MAC and
// is skipped. Parsing is forgiving of extra fields and whitespace: this is
// system-tool output, and one odd line must never drop the rest.
func parseNeighbours(out string) []Entry {
	var entries []Entry
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ip := fields[0]
		mac := ""
		for i := 1; i+1 < len(fields); i++ {
			if fields[i] == "lladdr" {
				mac = fields[i+1]
				break
			}
		}
		if mac == "" {
			continue
		}
		entries = append(entries, Entry{IP: ip, MAC: mac})
	}
	return entries
}
