// Package mdns is the relay's mDNS LISTENER (contracts/relay-1.md
// REL-110/111): it listens for mDNS announcements on the standard multicast
// group 224.0.0.251:5353 (net.ListenMulticastUDP, RFC 6762), parses each
// received message by hand with golang.org/x/net/dns/dnsmessage — no
// third-party mDNS library, this package owns the parse/match logic itself —
// and feeds every PTR-record service-type hit for a configured manifest/1
// MAN-071 mdns discovery-match pattern into the device plane's candidate
// Store as a ProvenanceDiscovered candidate, exactly like
// internal/relay/discovery's SSDP lane does for its own patterns.
//
// A candidate this package Observes is a MATCH-PATTERN hit — the pattern
// that matched, not a resolved device identity (REL-110's frozen Candidate
// shape carries only the Match, provenance, lifecycle status, and
// first/last-seen marks).
//
// mDNS service-type matching (RFC 6763 §4.1, DNS-Based Service Discovery): a
// PTR record's own owner name (ResourceHeader.Name — NOT its RDATA, which
// names one specific service instance under that type) is the service type
// being enumerated, e.g. "_waiveo._tcp.local.". This package normalizes that
// to "_waiveo._tcp" — trimming the trailing root dot and the reserved
// ".local" pseudo-TLD (RFC 6762 §3) — before matching it against configured
// patterns, which already carry that same normalized form.
//
// This package plays the LISTENER role only: reactively parsing whatever
// mDNS traffic (queries, responses, and unsolicited announcements alike)
// arrives on the multicast group. It never sends a query of its own — an
// active mDNS querier/responder role, if ever needed, is a separate
// deliverable, mirroring discovery/ssdpresponder's own CONTROL-POINT/
// RESPONDER split for SSDP.
package mdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
)

// mdnsAddress is the standard mDNS multicast group + port (RFC 6762 §3).
const mdnsAddress = "224.0.0.251:5353"

// maxDatagramSize is the read buffer size for one UDP datagram: the UDP
// protocol ceiling. Real mDNS packets are far smaller (RFC 6762 recommends
// staying under the path MTU), so this is a safe upper bound rather than a
// tuned figure.
const maxDatagramSize = 65535

// Config configures a Listener (REL-110/111).
type Config struct {
	// Patterns is the manifest/1 MAN-071 match set to watch for. Only
	// entries with MDNS set are used; SSDP/MacOui entries are ignored here
	// — SSDP discovery is internal/relay/discovery's own separate lane, and
	// MacOui has no mDNS analogue. Multiple patterns sharing the same
	// service-type string collapse to one watched type.
	Patterns []deviceplane.Match

	// Store is the candidate store every matched pattern is Observe'd into
	// (REL-110/111). Required.
	Store *deviceplane.Store

	// NowMillis is the Timestamp-ms clock Observe calls are stamped with.
	// Required — inject a fake in tests rather than reading the wall clock.
	NowMillis func() int64
}

// Listener listens for mDNS announcements and Observes every PTR-record
// service-type hit against a configured set of manifest/1 MAN-071 patterns
// into a deviceplane.Store as a ProvenanceDiscovered candidate (REL-110/111).
// See the package doc for the LISTENER/(no responder) split.
type Listener struct {
	byServiceType map[string]deviceplane.Match // normalized service-type -> pattern
	store         *deviceplane.Store
	nowMillis     func() int64

	listen func() (packetSource, error)
}

// New returns a Listener for cfg. It errors if cfg.Store or cfg.NowMillis is
// nil, or if cfg.Patterns contains no usable (MDNS-set) pattern.
func New(cfg Config) (*Listener, error) {
	if cfg.Store == nil {
		return nil, errors.New("mdns: Config.Store must not be nil")
	}
	if cfg.NowMillis == nil {
		return nil, errors.New("mdns: Config.NowMillis must not be nil")
	}

	byServiceType := make(map[string]deviceplane.Match)
	for _, p := range cfg.Patterns {
		if p.MDNS == "" {
			continue
		}
		byServiceType[p.MDNS] = p
	}
	if len(byServiceType) == 0 {
		return nil, errors.New("mdns: Config.Patterns has no usable MDNS pattern")
	}

	return &Listener{
		byServiceType: byServiceType,
		store:         cfg.Store,
		nowMillis:     cfg.NowMillis,
		listen:        defaultListen,
	}, nil
}

// Run opens the mDNS multicast listener and processes datagrams until ctx is
// canceled (REL-110/111), Observing every matching PTR hit into the Store as
// it arrives. It returns ctx.Err() once ctx is done, or an error opening the
// listener / a fatal read error.
//
// Cancellation is honored PROMPTLY, including mid-read (C1, mirroring
// discovery.go's own concern): a background goroutine closes the
// packetSource as soon as ctx is done, which unblocks a currently-blocked
// ReadPacket immediately. Unlike discovery's SSDP search — which has no
// context parameter at all and must instead race a detached goroutine whose
// late result is discarded (see discovery.go's searchPattern doc) — every
// read here is handled synchronously in this one loop, so there is no
// detached goroutine that could land a late Observe: once Run returns, no
// further Observe call from this Run invocation can reach the Store.
func (l *Listener) Run(ctx context.Context) error {
	src, err := l.listen()
	if err != nil {
		return fmt.Errorf("mdns: opening multicast listener: %w", err)
	}

	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = src.Close()
		case <-watchDone:
		}
	}()
	defer func() {
		close(watchDone)
		_ = src.Close()
	}()

	for {
		data, err := src.ReadPacket()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("mdns: read packet: %w", err)
		}
		if ctx.Err() != nil {
			// Run has already been asked to stop: a packet that raced the
			// cancellation-triggered Close must never reach the Store this
			// late.
			return ctx.Err()
		}
		l.handlePacket(data)
	}
}

// handlePacket parses one raw mDNS/DNS datagram and Observes every PTR
// answer/additional record whose (normalized) owner name matches a
// configured pattern. Any parse error at any stage — a header too short, a
// truncated/inconsistent section — causes the whole packet to be silently
// skipped: malformed input on a public multicast group is expected traffic,
// never a reason to panic or abort the listener.
func (l *Listener) handlePacket(data []byte) {
	var p dnsmessage.Parser
	if _, err := p.Start(data); err != nil {
		return
	}
	if err := p.SkipAllQuestions(); err != nil {
		return
	}
	answers, err := p.AllAnswers()
	if err != nil {
		return
	}
	if err := p.SkipAllAuthorities(); err != nil {
		return
	}
	additionals, err := p.AllAdditionals()
	if err != nil {
		return
	}

	atMs := l.nowMillis()
	l.observePTRRecords(answers, atMs)
	l.observePTRRecords(additionals, atMs)
}

// observePTRRecords Observes every PTR resource in resources whose owner
// name, normalized, exactly matches a configured pattern (REL-110/111). A
// PTR record's RDATA (the specific service instance it names) is never
// consulted — only its owner name, the service type being enumerated
// (RFC 6763 §4.1) — and a non-PTR resource or a non-matching name is
// ignored.
func (l *Listener) observePTRRecords(resources []dnsmessage.Resource, atMs int64) {
	for _, r := range resources {
		if r.Header.Type != dnsmessage.TypePTR {
			continue
		}
		serviceType := normalizeServiceType(r.Header.Name.String())
		m, ok := l.byServiceType[serviceType]
		if !ok {
			continue
		}
		l.store.Observe(m, deviceplane.ProvenanceDiscovered, atMs)
	}
}

// normalizeServiceType converts a PTR record's owner name as unpacked off
// the wire (e.g. "_waiveo._tcp.local.", always root-dot-terminated once
// unpacked — see dnsmessage.Name.unpack) into MAN-071's mdns pattern form
// (e.g. "_waiveo._tcp"): the trailing root dot and the reserved ".local"
// pseudo-TLD (RFC 6762 §3) are both trimmed. The ".local" trim is
// case-insensitive (DNS names are case-insensitive, RFC 1035 §2.3.3); the
// result is otherwise compared byte-exact against configured patterns,
// mirroring discovery.go's own exact search-target-string match.
func normalizeServiceType(name string) string {
	s := strings.TrimSuffix(name, ".")
	return trimSuffixFold(s, ".local")
}

// trimSuffixFold trims suffix from s, case-insensitively, if present.
func trimSuffixFold(s, suffix string) string {
	if len(s) < len(suffix) {
		return s
	}
	if !strings.EqualFold(s[len(s)-len(suffix):], suffix) {
		return s
	}
	return s[:len(s)-len(suffix)]
}

// packetSource is the boundary between this package's parsing pipeline and
// wherever its raw UDP datagrams come from, decoupled so unit tests can
// inject canned packets without ever opening a socket — the same
// seam-injection shape discovery.go's searchFn/monitorFactory and
// ssdpresponder.go's advertiser interface use for their own third-party
// boundaries. The default (defaultListen) is a real multicast UDP socket;
// tests inject a fake.
type packetSource interface {
	// ReadPacket blocks until one UDP datagram is available, returning its
	// raw bytes, or returns a non-nil error once the source has been closed
	// (including a close Run's own ctx-cancellation watcher triggers).
	ReadPacket() ([]byte, error)
	Close() error
}

// udpPacketSource is the real packetSource: an mDNS multicast UDP socket
// (net.ListenMulticastUDP — no third-party mDNS library).
type udpPacketSource struct {
	conn *net.UDPConn
}

func (s *udpPacketSource) ReadPacket() ([]byte, error) {
	buf := make([]byte, maxDatagramSize)
	n, _, err := s.conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (s *udpPacketSource) Close() error {
	return s.conn.Close()
}

// defaultListen opens the real mDNS multicast socket on mdnsAddress
// (net.ListenMulticastUDP, REL-110/111's live listener). A nil interface
// selects the OS's default multicast-capable interface.
func defaultListen() (packetSource, error) {
	addr, err := net.ResolveUDPAddr("udp4", mdnsAddress)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", mdnsAddress, err)
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("listen multicast udp %s: %w", mdnsAddress, err)
	}
	return &udpPacketSource{conn: conn}, nil
}
