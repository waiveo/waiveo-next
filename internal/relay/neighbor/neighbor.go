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
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/lanaddr"
)

// Driver is the identity namespace this lane mints under — the shared canonical
// MAC identity (deviceplane.HostDriver), re-exported so a caller can name it
// without reaching across packages.
const Driver = deviceplane.HostDriver

// ClassUnclassified is the device_class a host carries until something
// recognises it — the shared generic default (deviceplane.ClassUnclassified),
// re-exported so a caller names it without reaching across packages.
const ClassUnclassified = deviceplane.ClassUnclassified

const defaultInterval = 30 * time.Second

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
// as an unclassified MAC-keyed candidate (REL-110/111, spec §4.1). It also
// serves as the IP→MAC RESOLVER the protocol lanes use to merge their sightings
// onto the same MAC identity rather than mint a second row.
type Lane struct {
	store     *deviceplane.Store
	nowMillis func() int64
	interval  time.Duration
	read      func() ([]Entry, error)

	// ipMAC is the last sweep's IP→MAC map, swapped whole under mu so a reader
	// (MAC, called from another lane's goroutine) sees one coherent sweep, and
	// a departed host is forgotten rather than resolved to a stale MAC.
	mu    sync.RWMutex
	ipMAC map[string]string
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
	return &Lane{store: cfg.Store, nowMillis: cfg.NowMillis, interval: interval, read: read, ipMAC: map[string]string{}}, nil
}

// MAC resolves an IP to the MAC the last sweep saw it at, or ("", false) when
// this relay's neighbour table does not know it (a cross-subnet host the table
// cannot see). This is the resolver a protocol lane calls to key a sighting
// under the canonical MAC identity (spec §4.1); its signature is the seam a
// lane depends on, not this concrete type.
func (l *Lane) MAC(ip string) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	mac, ok := l.ipMAC[ip]
	return mac, ok
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
	fresh := make(map[string]string, len(entries))
	for _, e := range entries {
		// A neighbour is minted only with all three of: a dialable LAN address
		// (the store's own policy, applied here so a link-local neighbour is
		// never-minted noise rather than a candidate with a blanked address); a
		// valid MAC identity/OUI (a candidate whose Match will not marshal is
		// silently DROPPED at report time, so it is filtered here at mint time
		// instead); and the MAC as its canonical identity.
		if !lanaddr.Host(e.IP) {
			continue
		}
		driver, nativeID, match, ok := deviceplane.MACIdentity(e.MAC)
		if !ok {
			continue
		}
		fresh[e.IP] = nativeID
		l.store.Observe(deviceplane.Observation{
			Match:       match,
			Provenance:  deviceplane.ProvenanceDiscovered,
			Driver:      driver,
			NativeID:    nativeID,
			DeviceClass: ClassUnclassified,
			Address:     e.IP,
		}, now)
	}
	l.mu.Lock()
	l.ipMAC = fresh
	l.mu.Unlock()
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
