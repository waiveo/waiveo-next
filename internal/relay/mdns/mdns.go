// Package mdns is the relay's mDNS LISTENER (contracts/relay-1.md
// REL-110/111): it listens for mDNS announcements on the standard multicast
// group 224.0.0.251:5353 (RFC 6762), parses each received message by hand
// with golang.org/x/net/dns/dnsmessage — no third-party mDNS library, this
// package owns the parse/match logic itself —
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
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"

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
	// Watches is the set of declared discovery watches this lane listens for:
	// each pairs a manifest/1 MAN-071 match with the driver, device class and
	// entity fan-out a device found by it is reported under (REL-110a). Only
	// watches whose Match sets MDNS are used; SSDP/MacOui entries are ignored
	// here — SSDP discovery is internal/relay/discovery's own separate lane,
	// and MacOui has no mDNS analogue. Multiple watches sharing the same
	// service-type string collapse to one watched type, case-insensitively
	// (RFC 1035 §2.3.3 — see byServiceType's own doc).
	Watches []Watch

	// Store is the candidate store every matched pattern is Observe'd into
	// (REL-110/111). Required.
	Store *deviceplane.Store

	// NowMillis is the Timestamp-ms clock Observe calls are stamped with.
	// Required — inject a fake in tests rather than reading the wall clock.
	NowMillis func() int64
}

// Watch is one declared discovery watch: a manifest/1 MAN-071 match pattern
// plus the facts a device found by it is reported under (REL-110a). Driver
// names the adapter that speaks to such a device (half of REL-153's identity
// tuple), DeviceClass the vocabulary its commands resolve against, and
// Entities the addressable handles that driver exposes. None is discoverable
// from a PTR record — they come from the declaration that asked for the
// listen; the record supplies only which instance announced.
type Watch struct {
	Match       deviceplane.Match
	Driver      string
	DeviceClass string
	Entities    []deviceplane.CandidateEntity
}

// Listener listens for mDNS announcements and Observes every PTR-record
// service-type hit against a configured set of manifest/1 MAN-071 patterns
// into a deviceplane.Store as a ProvenanceDiscovered candidate (REL-110/111).
// See the package doc for the LISTENER/(no responder) split.
type Listener struct {
	// watches holds the CURRENT watch set as an atomically-swapped map of
	// normalized, lower-cased service-type → watch (see observePTRRecords).
	// DNS names are case-insensitive (RFC 1035 §2.3.3), so keys are folded to
	// lower case at installation and every lookup folds its own name the same
	// way — matching must not depend on whatever case a manifest author or
	// a particular device on the wire happened to use. A pointer swap rather
	// than a mutex for the same reason discovery.Discoverer.watches is one:
	// the packet loop reads it per PTR record while SetWatches (driven by
	// every signed desired-state apply, REL-064) replaces it, and each reader
	// wants one coherent snapshot, never a mid-update view.
	watches   atomic.Pointer[map[string]Watch]
	store     *deviceplane.Store
	nowMillis func() int64

	listen func() (packetSource, error)
}

// New returns a Listener for cfg. It errors if cfg.Store or cfg.NowMillis is
// nil. An EMPTY (or mDNS-free) initial watch set is legal, not an error:
// watches follow the signed desired state (REL-064, SetWatches), so a lane
// that starts before the first pack pattern arrives observes nothing and then
// picks the patterns up on the first inventory apply.
func New(cfg Config) (*Listener, error) {
	if cfg.Store == nil {
		return nil, errors.New("mdns: Config.Store must not be nil")
	}
	if cfg.NowMillis == nil {
		return nil, errors.New("mdns: Config.NowMillis must not be nil")
	}

	l := &Listener{
		store:     cfg.Store,
		nowMillis: cfg.NowMillis,
		listen:    defaultListen,
	}
	l.SetWatches(cfg.Watches)
	return l, nil
}

// SetWatches REPLACES the whole watch set — the REL-064 join, mirroring
// discovery.Discoverer.SetWatches: every signed desired-state apply derives
// the current pack-declared patterns into watches and installs them here. A
// full replace rather than a diff because the patterns section is itself a
// full set (REL-064) and a replace cannot leak a watch whose pack is gone.
//
// Only mDNS-form watches are usable on this lane; others are skipped.
// Returns how many watches are now live.
func (l *Listener) SetWatches(ws []Watch) int {
	byServiceType := make(map[string]Watch)
	for _, w := range ws {
		if w.Match.MDNS == "" {
			continue
		}
		byServiceType[strings.ToLower(w.Match.MDNS)] = w
	}
	l.watches.Store(&byServiceType)
	return len(byServiceType)
}

// watchSet returns the current watch map — one coherent snapshot; callers
// must not mutate it.
func (l *Listener) watchSet() map[string]Watch {
	return *l.watches.Load()
}

// WatchCount reports how many watches are currently live — the far side of
// SetWatches, mirroring discovery.Discoverer.WatchCount.
func (l *Listener) WatchCount() int {
	return len(l.watchSet())
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
//
// src is closed exactly once no matter which of the two paths below gets
// there first (the ctx-cancellation watcher, or Run's own deferred
// cleanup on a non-cancellation return) — sync.Once, not two independent
// unconditional Close calls, since both paths run on every ctx-cancellation
// return and a real packetSource has no obligation to tolerate a redundant
// Close cleanly.
func (l *Listener) Run(ctx context.Context) error {
	src, err := l.listen()
	if err != nil {
		return fmt.Errorf("mdns: opening multicast listener: %w", err)
	}

	var closeOnce sync.Once
	closeSrc := func() { closeOnce.Do(func() { _ = src.Close() }) }

	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeSrc()
		case <-watchDone:
		}
	}()
	defer func() {
		close(watchDone)
		closeSrc()
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
//
// Answers and additionals are processed as ONE record set: an RFC 6763
// announcement puts the PTR in answers and the SRV/A records that make it
// USABLE in additionals, so resolution (below) must correlate across the
// section split, and a PTR in either section is a sighting either way.
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

	records := append(answers, additionals...)
	l.observePTRRecords(records, l.nowMillis(), srvTargets(records), hostAddrs(records))
}

// srvEndpoint is one SRV record's RDATA half this lane consumes: where the
// service instance says it can be reached (RFC 2782 via RFC 6763 §5).
type srvEndpoint struct {
	target string // host name, as unpacked (root-dot-terminated)
	port   uint16
}

// srvTargets indexes every SRV record by its (case-folded) owner name — the
// service INSTANCE it locates. First record wins a duplicate owner: one
// packet stating two endpoints for one instance is already malformed, and
// deterministic beats last-writer.
func srvTargets(resources []dnsmessage.Resource) map[string]srvEndpoint {
	var out map[string]srvEndpoint
	for _, r := range resources {
		if r.Header.Type != dnsmessage.TypeSRV {
			continue
		}
		srv, ok := r.Body.(*dnsmessage.SRVResource)
		if !ok {
			continue
		}
		key := strings.ToLower(r.Header.Name.String())
		if out == nil {
			out = map[string]srvEndpoint{}
		}
		if _, dup := out[key]; !dup {
			out[key] = srvEndpoint{target: srv.Target.String(), port: srv.Port}
		}
	}
	return out
}

// hostAddrs indexes every A/AAAA record by its (case-folded) owner host name.
// An IPv4 address wins over an IPv6 one for the same host — every consumer of
// a candidate address on this relay (the ECP controller, the pollers) dials
// plain ip:port on the LAN, and the lab fleet is v4 — while first-A-wins keeps
// duplicates deterministic.
func hostAddrs(resources []dnsmessage.Resource) map[string]string {
	var out map[string]string
	put := func(key, ip string, preferred bool) {
		if out == nil {
			out = map[string]string{}
		}
		if cur, ok := out[key]; ok && !(preferred && strings.Contains(cur, ":")) {
			return // keep the incumbent unless a v4 is displacing a v6
		}
		out[key] = ip
	}
	for _, r := range resources {
		key := strings.ToLower(r.Header.Name.String())
		switch body := r.Body.(type) {
		case *dnsmessage.AResource:
			put(key, netip.AddrFrom4(body.A).String(), true)
		case *dnsmessage.AAAAResource:
			put(key, netip.AddrFrom16(body.AAAA).String(), false)
		}
	}
	return out
}

// observePTRRecords Observes every PTR resource in resources whose owner
// name, normalized, matches a configured watch (REL-110/111) — the
// comparison folds case (byServiceType's own doc): DNS names are
// case-insensitive (RFC 1035 §2.3.3), so a device announcing
// "_Waiveo._TCP.local." must still hit a configured "_waiveo._tcp" pattern.
// A non-PTR resource or a non-matching owner name is ignored.
//
// The record's RDATA — the specific service INSTANCE the owner name enumerates
// (RFC 6763 §4.1) — is this lane's `native_id` (REL-110a). It is what
// distinguishes two devices of one service type on one LAN, and without it both
// would collapse into a single candidate that neither could be listed or
// addressed as (REL-111a, REL-153). A PTR carrying no instance name identifies
// no device and is skipped.
//
// A sighting is RESOLVED when the same datagram carries the instance's SRV
// (endpoint) and that target host's A/AAAA record — the shape RFC 6763 §12.1
// tells responders to bundle — yielding the Address every consumer needs to
// DRIVE the device and the human instance label as its Name. A thinner packet
// (PTR alone, or SRV with no address record) still observes the sighting,
// just unlocated: the store's orKeep merge fills the gap when a fuller
// announcement arrives, and never lets a thin one erase what a full one
// taught. Without any of this, an mDNS candidate carried no address at all —
// listable, adoptable, and undrivable forever.
func (l *Listener) observePTRRecords(resources []dnsmessage.Resource, atMs int64, srv map[string]srvEndpoint, addrs map[string]string) {
	for _, r := range resources {
		if r.Header.Type != dnsmessage.TypePTR {
			continue
		}
		serviceType := normalizeServiceType(r.Header.Name.String())
		w, ok := l.watchSet()[strings.ToLower(serviceType)]
		if !ok {
			continue
		}
		ptr, ok := r.Body.(*dnsmessage.PTRResource)
		if !ok {
			continue
		}
		wireInstance := ptr.PTR.String()
		instance := strings.TrimSuffix(wireInstance, ".")
		if instance == "" {
			continue
		}

		address := ""
		if ep, ok := srv[strings.ToLower(wireInstance)]; ok && ep.port != 0 {
			if ip, ok := addrs[strings.ToLower(ep.target)]; ok {
				address = net.JoinHostPort(ip, strconv.Itoa(int(ep.port)))
			}
		}

		l.store.Observe(deviceplane.Observation{
			Match:       w.Match,
			Provenance:  deviceplane.ProvenanceDiscovered,
			Driver:      w.Driver,
			NativeID:    instance,
			DeviceClass: w.DeviceClass,
			Name:        instanceLabel(wireInstance, r.Header.Name.String()),
			Address:     address,
			Entities:    w.Entities,
		}, atMs)
	}
}

// instanceLabel extracts the human display label from a full service-instance
// name: "Living Room TV._waiveo._tcp.local." → "Living Room TV". The service
// suffix is the PTR's own owner name, trimmed case-insensitively; a name that
// does not end with it (a nonconforming responder) yields no label rather
// than a wrong one. DNS presentation escapes (`\.` for a dot inside the
// label, `\\`, and `\DDD` byte escapes) are decoded so the operator reads
// the name the device announced, not its wire spelling — and a decode that
// yields invalid UTF-8 (the escapes admit arbitrary bytes, and the bytes are
// attacker-chosen multicast) yields no label instead: the store poisons the
// WHOLE observation on an invalid field, and a device must not lose its
// sighting to its own weird name.
func instanceLabel(instance, serviceOwner string) string {
	inst := strings.TrimSuffix(instance, ".")
	suffix := "." + strings.TrimSuffix(serviceOwner, ".")
	label := trimSuffixFold(inst, suffix)
	if label == inst || label == "" {
		return ""
	}
	decoded := unescapeDNSLabel(label)
	if !utf8.ValidString(decoded) {
		return ""
	}
	return decoded
}

// unescapeDNSLabel decodes RFC 1035 presentation-format escapes.
func unescapeDNSLabel(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		next := s[i+1]
		if next >= '0' && next <= '9' && i+3 < len(s) {
			if n, err := strconv.Atoi(s[i+1 : i+4]); err == nil && n <= 255 {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(next)
		i++
	}
	return b.String()
}

// normalizeServiceType converts a PTR record's owner name as unpacked off
// the wire (e.g. "_waiveo._tcp.local.", always root-dot-terminated once
// unpacked — see dnsmessage.Name.unpack) into MAN-071's mdns pattern form
// (e.g. "_waiveo._tcp"): the trailing root dot and the reserved ".local"
// pseudo-TLD (RFC 6762 §3) are both trimmed. The ".local" trim itself is
// case-insensitive, and the result is matched against configured patterns
// case-insensitively too (byServiceType's own doc, RFC 1035 §2.3.3) — that
// full-name fold happens at the byServiceType lookup site, not here, so this
// function's own return value stays whatever case the wire sent.
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
// (defaultListen — no third-party mDNS library).
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
// (REL-110/111's live listener), joined on every up, multicast-capable,
// IPv4-addressed network interface — not net.ListenMulticastUDP(nil), and
// deliberately so.
//
// net.ListenMulticastUDP's own doc calls passing a nil *net.Interface "not
// recommended because the assignment depends on platforms and sometimes it
// might require routing configuration": the OS picks one system-assigned
// interface to join the multicast group on, and on a multi-homed box (a
// waiveo-host Raspberry Pi appliance ships with both eth0 and wlan0) that
// pick can be the wrong NIC — the one that never actually carries LAN mDNS
// traffic. Run would still return nil and loop forever reading nothing,
// indistinguishable from "no traffic ever arrived" (see
// TestLiveMulticastListenSmoke's own doc), silently starving discovery with
// no error surfaced anywhere.
//
// This mirrors the sibling SSDP lane's own third-party dependency,
// go-ssdp@v0.9.1's internal/multicast.Listen(): one UDP socket bound to the
// group address, wrapped in golang.org/x/net/ipv4 so the multicast group can
// be joined on every qualifying interface individually (joinAllInterfaces)
// rather than left to the OS's single implicit choice.
func defaultListen() (packetSource, error) {
	addr, err := net.ResolveUDPAddr("udp4", mdnsAddress)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", mdnsAddress, err)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listen udp %s: %w", mdnsAddress, err)
	}
	if err := joinAllInterfaces(ipv4.NewPacketConn(conn), addr); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &udpPacketSource{conn: conn}, nil
}

// joinAllInterfaces joins pconn to the group multicast address on every
// up, multicast-capable, IPv4-addressed interface on the box (go-ssdp's own
// interfacesIPv4() filter, mirrored here so both discovery lanes in this
// codebase enumerate join candidates the same way). A single interface's
// join failure is skipped rather than fatal — a link that goes down between
// net.Interfaces() and the JoinGroup call, or one this process lacks
// privilege to join on, are both expected on a real box — but joining zero
// interfaces IS fatal: it reproduces exactly the "OS silently picked nothing
// usable" failure mode this function exists to avoid (see defaultListen's
// own doc), so that case is a reported error rather than a silent no-op
// listener.
func joinAllInterfaces(pconn *ipv4.PacketConn, group *net.UDPAddr) error {
	ifaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("mdns: listing network interfaces: %w", err)
	}

	joined := 0
	var lastErr error
	for i := range ifaces {
		ifi := &ifaces[i]
		if !multicastCapable(ifi) {
			continue
		}
		if err := pconn.JoinGroup(ifi, group); err != nil {
			lastErr = err
			continue
		}
		joined++
	}
	if joined > 0 {
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("mdns: joining multicast group %s on any interface: %w", group, lastErr)
	}
	return fmt.Errorf("mdns: no up, multicast-capable, IPv4 interface found to join %s on", group)
}

// multicastCapable reports whether ifi is up, multicast-capable, and has at
// least one non-unspecified IPv4 address — the same filter go-ssdp's
// internal/multicast/interface.go applies (hasLinkUp + hasMulticast +
// hasIPv4Address) before it will attempt to join a group on an interface.
func multicastCapable(ifi *net.Interface) bool {
	if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
		return false
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ip, _, err := net.ParseCIDR(a.String())
		if err != nil {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil && !ip.IsUnspecified() {
			return true
		}
	}
	return false
}
