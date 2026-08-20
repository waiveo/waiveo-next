package mdns

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
)

// mustMatch parses raw into a deviceplane.Match via ParseMatch (match.go's
// documented construction path), failing the test on a parse error —
// byte-identical to discovery_test.go's own helper of the same name.
func mustMatch(t *testing.T, raw string) deviceplane.Match {
	t.Helper()
	m, err := deviceplane.ParseMatch(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("parse match %s: %v", raw, err)
	}
	return m
}

// buildPTRPacket hand-builds a raw mDNS response datagram carrying one PTR
// answer whose owner name is ownerName (the service type matching keys on) and
// whose RDATA is target — the specific service INSTANCE, which is the device's
// native_id (REL-110a).
func buildPTRPacket(t *testing.T, ownerName, target string) []byte {
	t.Helper()
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{Response: true},
		Answers: []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{
					Name:  dnsmessage.MustNewName(ownerName),
					Type:  dnsmessage.TypePTR,
					Class: dnsmessage.ClassINET,
					TTL:   120,
				},
				Body: &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(target)},
			},
		},
	}
	data, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack PTR message: %v", err)
	}
	return data
}

// buildAPacket hand-builds a raw mDNS response datagram carrying one A
// record answer — a non-PTR record type observePTRRecords must ignore.
func buildAPacket(t *testing.T, ownerName string) []byte {
	t.Helper()
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{Response: true},
		Answers: []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{
					Name:  dnsmessage.MustNewName(ownerName),
					Type:  dnsmessage.TypeA,
					Class: dnsmessage.ClassINET,
					TTL:   120,
				},
				Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}},
			},
		},
	}
	data, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack A message: %v", err)
	}
	return data
}

// watchFor wraps a bare match in the declaration-side facts a Watch carries
// (REL-110a) — the driver, class and entity handle a device found by this
// pattern is reported under. Constant across this file's cases; what varies is
// the PTR instance name a packet supplies as the device's native_id.
func watchFor(m deviceplane.Match) Watch {
	return Watch{
		Match:       m,
		Driver:      "mdns",
		DeviceClass: "media-player",
		Entities:    []deviceplane.CandidateEntity{{Key: "main", DeviceClass: "media-player"}},
	}
}

func TestNewValidation(t *testing.T) {
	waiveoWatch := []Watch{watchFor(mustMatch(t, `{"mdns":"_waiveo._tcp"}`))}
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }

	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "nil Store",
			cfg:  Config{Watches: waiveoWatch, Store: nil, NowMillis: now},
		},
		{
			name: "nil NowMillis",
			cfg:  Config{Watches: waiveoWatch, Store: store, NowMillis: nil},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Fatal("New() error = nil, want error")
			}
		})
	}
}

// An empty (or mDNS-free) initial watch set is LEGAL: watches follow the
// signed desired state (REL-064), so a lane may start before the first pack
// pattern exists. It observes nothing until SetWatches installs one — and a
// later full replace with an empty set forgets a removed pack's watch rather
// than leaking it.
func TestWatchesFollowSetWatches(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }

	l, err := New(Config{Watches: nil, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() with no initial watches must construct, got %v", err)
	}
	if got := l.WatchCount(); got != 0 {
		t.Fatalf("WatchCount() = %d before any SetWatches, want 0", got)
	}

	n := l.SetWatches([]Watch{
		watchFor(mustMatch(t, `{"mdns":"_waiveo._tcp"}`)),
		// An SSDP-form watch is unusable on this lane and must not count.
		watchFor(mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)),
	})
	if n != 1 || l.WatchCount() != 1 {
		t.Fatalf("SetWatches installed %d (count %d), want exactly the one mDNS watch", n, l.WatchCount())
	}

	if n := l.SetWatches(nil); n != 0 || l.WatchCount() != 0 {
		t.Fatalf("a full replace with no watches must forget the old set; got %d live", l.WatchCount())
	}
}

func TestNewOK(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pat := []Watch{watchFor(mustMatch(t, `{"mdns":"_waiveo._tcp"}`))}

	l, err := New(Config{Watches: pat, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if got := l.WatchCount(); got != 1 {
		t.Fatalf("WatchCount() = %d, want 1", got)
	}
	if _, ok := l.watchSet()["_waiveo._tcp"]; !ok {
		t.Errorf("watch set missing _waiveo._tcp: %+v", l.watchSet())
	}
}

// TestHandlePacketObservesMatchingPTR asserts a PTR answer whose owner name,
// normalized, exactly matches the configured pattern is Observed into the
// Store (REL-110/111).
func TestHandlePacketObservesMatchingPTR(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	data := buildPTRPacket(t, "_waiveo._tcp.local.", "TheHanger._waiveo._tcp.local.")
	l.handlePacket(data)

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(cands), cands)
	}
	if cands[0].Match != pattern {
		t.Errorf("candidate match = %+v, want %+v", cands[0].Match, pattern)
	}
	if cands[0].Provenance != deviceplane.ProvenanceDiscovered {
		t.Errorf("provenance = %q, want %q", cands[0].Provenance, deviceplane.ProvenanceDiscovered)
	}
	if cands[0].FirstSeen != 1000 || cands[0].LastSeen != 1000 {
		t.Errorf("first/last seen = %d/%d, want 1000/1000", cands[0].FirstSeen, cands[0].LastSeen)
	}
}

// TestHandlePacketObservesMatchingPTRCaseInsensitive asserts a PTR answer
// whose owner name differs from a configured pattern only by case (anywhere
// in the name, not just the ".local" suffix normalizeServiceType already
// folds) still matches: DNS names are case-insensitive (RFC 1035 §2.3.3),
// so a device announcing "_Waiveo._TCP.local." must hit a configured
// "_waiveo._tcp" pattern exactly as a byte-identical announcement would.
func TestHandlePacketObservesMatchingPTRCaseInsensitive(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	data := buildPTRPacket(t, "_Waiveo._TCP.LOCAL.", "TheHanger._Waiveo._TCP.local.")
	l.handlePacket(data)

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1 (case-insensitive match): %+v", len(cands), cands)
	}
	if cands[0].Match != pattern {
		t.Errorf("candidate match = %+v, want %+v", cands[0].Match, pattern)
	}
}

// TestHandlePacketObservesMatchingPTRCaseInsensitivePattern is the mirror of
// TestHandlePacketObservesMatchingPTRCaseInsensitive from the other side: a
// mixed-case configured pattern must still fold at construction (New) so a
// byte-exact lower-case wire announcement matches it.
func TestHandlePacketObservesMatchingPTRCaseInsensitivePattern(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"mdns":"_Waiveo._TCP"}`)

	l, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	data := buildPTRPacket(t, "_waiveo._tcp.local.", "TheHanger._waiveo._tcp.local.")
	l.handlePacket(data)

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1 (case-insensitive match): %+v", len(cands), cands)
	}
	if cands[0].Match != pattern {
		t.Errorf("candidate match = %+v, want %+v", cands[0].Match, pattern)
	}
}

// TestHandlePacketIgnoresNonMatchingPTR asserts a PTR answer whose owner name
// does not match any configured pattern is never Observed.
func TestHandlePacketIgnoresNonMatchingPTR(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	data := buildPTRPacket(t, "_googlecast._tcp.local.", "Chromecast._googlecast._tcp.local.")
	l.handlePacket(data)

	if cands := store.Report().Body.Candidates; len(cands) != 0 {
		t.Fatalf("got %d candidates, want 0: %+v", len(cands), cands)
	}
}

// TestHandlePacketIgnoresNonPTRRecord asserts a non-PTR answer (e.g. an A
// record glue answer) is never Observed even when its owner name happens to
// match a configured pattern string.
func TestHandlePacketIgnoresNonPTRRecord(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	data := buildAPacket(t, "_waiveo._tcp.local.")
	l.handlePacket(data)

	if cands := store.Report().Body.Candidates; len(cands) != 0 {
		t.Fatalf("got %d candidates, want 0 (A record must be ignored): %+v", len(cands), cands)
	}
}

// TestHandlePacketMalformedIsSkippedWithoutPanic asserts a malformed packet —
// too short to even carry a DNS header, or one whose declared section counts
// don't match its actual bytes — is silently skipped: no Observe, no panic.
func TestHandlePacketMalformedIsSkippedWithoutPanic(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	malformed := [][]byte{
		nil,
		{},
		{0x00, 0x01, 0x02}, // shorter than a 12-byte DNS header
		func() []byte {
			// A well-formed 12-byte header claiming 1 answer, but with no
			// resource-record bytes following it at all.
			good := buildPTRPacket(t, "_waiveo._tcp.local.", "x._waiveo._tcp.local.")
			return good[:12] // header only, truncated before the question/answer bytes
		}(),
	}

	for i, data := range malformed {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d: handlePacket panicked: %v", i, r)
				}
			}()
			l.handlePacket(data)
		}()
	}

	if cands := store.Report().Body.Candidates; len(cands) != 0 {
		t.Fatalf("got %d candidates after malformed packets, want 0: %+v", len(cands), cands)
	}
}

// TestHandlePacketRepeatedHitsBumpLastSeenNotDuplicate asserts two packets
// for the same pattern — within one call and across two — dedup to a single
// candidate with last_seen bumped and first_seen left alone (Store.Observe's
// own device-identity dedup, REL-110/111a).
func TestHandlePacketRepeatedHitsBumpLastSeenNotDuplicate(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	nowVal := int64(1000)
	now := func() int64 { return nowVal }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	data := buildPTRPacket(t, "_waiveo._tcp.local.", "TheHanger._waiveo._tcp.local.")
	l.handlePacket(data)
	nowVal = 2000
	l.handlePacket(data)

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1 (dedup by device identity): %+v", len(cands), cands)
	}
	if cands[0].FirstSeen != 1000 {
		t.Errorf("first_seen = %d, want 1000 (must not move)", cands[0].FirstSeen)
	}
	if cands[0].LastSeen != 2000 {
		t.Errorf("last_seen = %d, want 2000 (must bump on re-observe)", cands[0].LastSeen)
	}
}

// TestNormalizeServiceType pins the wire-form -> MAN-071-pattern-form
// conversion: trailing root dot and the ".local" pseudo-TLD trimmed,
// case-insensitively for ".local".
func TestNormalizeServiceType(t *testing.T) {
	tests := []struct{ in, want string }{
		{"_waiveo._tcp.local.", "_waiveo._tcp"},
		{"_waiveo._tcp.LOCAL.", "_waiveo._tcp"},
		{"_waiveo._tcp", "_waiveo._tcp"}, // already normalized, no-op
		{"_waiveo._tcp.", "_waiveo._tcp"},
	}
	for _, tt := range tests {
		if got := normalizeServiceType(tt.in); got != tt.want {
			t.Errorf("normalizeServiceType(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// fakePacketSource is a packetSource test double that never touches a real
// socket: ReadPacket blocks on a channel of queued packets or on the fake
// being closed, mirroring discovery_test.go's fakeMonitor / ssdpresponder's
// fakeAdvertiser pattern for this package's own third-party-free boundary.
type fakePacketSource struct {
	mu         sync.Mutex
	packets    chan []byte
	closed     chan struct{}
	closeOnce  sync.Once
	closeCalls int
}

func newFakePacketSource() *fakePacketSource {
	return &fakePacketSource{packets: make(chan []byte, 8), closed: make(chan struct{})}
}

func (f *fakePacketSource) ReadPacket() ([]byte, error) {
	select {
	case p := <-f.packets:
		return p, nil
	case <-f.closed:
		return nil, errors.New("fakePacketSource: closed")
	}
}

func (f *fakePacketSource) push(p []byte) { f.packets <- p }

func (f *fakePacketSource) Close() error {
	f.mu.Lock()
	f.closeCalls++
	f.mu.Unlock()
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *fakePacketSource) closeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCalls
}

// TestRunObservesDeliveredPacketsThenStopsPromptlyOnContextCancel proves the
// full Run wiring end to end over a fake packetSource: a delivered matching
// packet is Observed, and ctx cancellation makes Run return ctx.Err()
// promptly (closing the source, which is what unblocks a real blocked
// ReadPacket — C1, mirroring discovery's own prompt-cancellation concern).
func TestRunObservesDeliveredPacketsThenStopsPromptlyOnContextCancel(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	fake := newFakePacketSource()
	l.listen = func() (packetSource, error) { return fake, nil }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	fake.push(buildPTRPacket(t, "_waiveo._tcp.local.", "TheHanger._waiveo._tcp.local."))

	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(store.Report().Body.Candidates) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run() never Observed the delivered packet within 2s")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return promptly after ctx cancel")
	}

	// Exactly once: the ctx-cancellation watcher goroutine and Run's own
	// deferred cleanup both reach a close call on this path, but sync.Once
	// inside Run must collapse them to a single real Close (Run's own doc).
	if got := fake.closeCallCount(); got != 1 {
		t.Errorf("packetSource.Close() called %d times on ctx cancel, want exactly 1", got)
	}
}

// TestRunStopsPromptlyOnContextCancelWithNoTraffic mirrors discovery_test.go's
// TestRunStopsPromptlyOnContextCancel: Run must return promptly on ctx
// cancellation even when no packet has ever arrived (ReadPacket is blocked
// indefinitely on the fake, exactly like a real idle multicast socket).
func TestRunStopsPromptlyOnContextCancelWithNoTraffic(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	fake := newFakePacketSource()
	l.listen = func() (packetSource, error) { return fake, nil }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return promptly after ctx cancel")
	}

	if cands := store.Report().Body.Candidates; len(cands) != 0 {
		t.Fatalf("got %d candidates, want 0 (no traffic ever arrived)", len(cands))
	}
}

// TestRunReturnsListenError asserts a failure opening the packetSource
// (e.g. the multicast bind failing) is returned from Run, wrapped.
func TestRunReturnsListenError(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	boom := errors.New("bind failed")
	l.listen = func() (packetSource, error) { return nil, boom }

	if err := l.Run(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Run() error = %v, want wrapping %v", err, boom)
	}
}

// TestLiveMulticastListenSmoke is an optional, env-gated integration check
// that defaultListen can actually bind the real mDNS multicast socket and
// that Run tears down cleanly on cancellation against it — skipped by
// default so CI and ordinary `go test` runs never touch multicast; set
// WAIVEO_HW_LAN=1 to run it, mirroring discovery_test.go's TestLiveLANSearch
// and ssdpresponder_test.go's TestLiveAdvertiseSmoke gates. It does not
// assert any candidate was observed — real mDNS announcements are sent
// rarely (mostly on network join), so asserting one arrives within a short
// window would be flaky; this only proves the real socket path is sound.
func TestLiveMulticastListenSmoke(t *testing.T) {
	if os.Getenv("WAIVEO_HW_LAN") != "1" {
		t.Skip("set WAIVEO_HW_LAN=1 to run against real multicast")
	}

	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return time.Now().UnixMilli() }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() against the real multicast socket: %v", err)
	}
}

// TestHandlePacketReportsInstanceIdentity is REL-110a at the mDNS lane: the
// PTR record's RDATA — the service INSTANCE the owner name enumerates
// (RFC 6763 §4.1) — is the device's native_id. Two instances of one service
// type must produce two candidates: matching on the owner name alone would
// report both boxes as one, and neither could then be listed or addressed.
func TestHandlePacketReportsInstanceIdentity(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)
	l, err := New(Config{Watches: []Watch{watchFor(pattern)}, Store: store, NowMillis: func() int64 { return 1000 }})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	l.handlePacket(buildPTRPacket(t, "_waiveo._tcp.local.", "TheHanger._waiveo._tcp.local."))
	l.handlePacket(buildPTRPacket(t, "_waiveo._tcp.local.", "BackOffice._waiveo._tcp.local."))

	cands := store.Report().Body.Candidates
	if len(cands) != 2 {
		t.Fatalf("got %d candidate(s), want 2 — two instances of one service type are two devices: %+v", len(cands), cands)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.NativeID] = true
		if c.Driver != "mdns" || c.DeviceClass != "media-player" {
			t.Errorf("candidate %q = driver %q class %q, want the watch's declared mdns/media-player", c.NativeID, c.Driver, c.DeviceClass)
		}
	}
	for _, want := range []string{"TheHanger._waiveo._tcp", "BackOffice._waiveo._tcp"} {
		if !got[want+".local"] {
			t.Errorf("no candidate carries native_id %q.local; got %v", want, got)
		}
	}
}

// --- G3: same-datagram PTR→SRV→A resolution -----------------------------------

// packMsg packs one mDNS response with the given answer/additional sections.
func packMsg(t *testing.T, answers, additionals []dnsmessage.Resource) []byte {
	t.Helper()
	msg := dnsmessage.Message{
		Header:      dnsmessage.Header{Response: true},
		Answers:     answers,
		Additionals: additionals,
	}
	data, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack message: %v", err)
	}
	return data
}

func ptrRec(owner, target string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(owner), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET, TTL: 120},
		Body:   &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(target)},
	}
}

func srvRec(owner, target string, port uint16) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(owner), Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET, TTL: 120},
		Body:   &dnsmessage.SRVResource{Target: dnsmessage.MustNewName(target), Port: port},
	}
}

func aRec(owner string, ip [4]byte) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(owner), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 120},
		Body:   &dnsmessage.AResource{A: ip},
	}
}

func aaaaRec(owner string, ip [16]byte) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(owner), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: 120},
		Body:   &dnsmessage.AAAAResource{AAAA: ip},
	}
}

func newResolvingListener(t *testing.T) (*Listener, *deviceplane.Store) {
	t.Helper()
	store := deviceplane.NewStore("relay-1")
	l, err := New(Config{
		Watches:   []Watch{watchFor(mustMatch(t, `{"mdns":"_waiveo._tcp"}`))},
		Store:     store,
		NowMillis: func() int64 { return 1000 },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l, store
}

func soleCandidate(t *testing.T, store *deviceplane.Store) deviceplane.Candidate {
	t.Helper()
	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	return cands[0]
}

// The RFC 6763 §12.1 bundle — PTR in answers, SRV+A in additionals — resolves
// to a candidate an operator can read (Name) and this relay can DRIVE
// (Address). Before this, an mDNS candidate carried no address at all:
// listable, adoptable, undrivable forever.
func TestAnnouncementResolvesAddressAndName(t *testing.T) {
	l, store := newResolvingListener(t)
	l.handlePacket(packMsg(t,
		[]dnsmessage.Resource{ptrRec("_waiveo._tcp.local.", "Living Room._waiveo._tcp.local.")},
		[]dnsmessage.Resource{
			srvRec("Living Room._waiveo._tcp.local.", "tv.local.", 7423),
			aRec("tv.local.", [4]byte{192, 168, 0, 9}),
		},
	))
	c := soleCandidate(t, store)
	if c.Address != "192.168.0.9:7423" {
		t.Errorf("Address = %q, want the SRV+A resolution", c.Address)
	}
	if c.Name != "Living Room" {
		t.Errorf("Name = %q, want the instance label", c.Name)
	}
	if c.NativeID != "Living Room._waiveo._tcp.local" {
		t.Errorf("NativeID = %q, want the full instance name", c.NativeID)
	}
}

// Correlation folds case at BOTH hops (RFC 1035 §2.3.3): the SRV owner and
// the A owner may be spelled in any case relative to the names that point at
// them.
func TestResolutionFoldsCaseAcrossHops(t *testing.T) {
	l, store := newResolvingListener(t)
	l.handlePacket(packMsg(t,
		[]dnsmessage.Resource{ptrRec("_waiveo._tcp.local.", "TV._waiveo._tcp.local.")},
		[]dnsmessage.Resource{
			srvRec("tv._WAIVEO._tcp.LOCAL.", "Host.Local.", 8080),
			aRec("HOST.local.", [4]byte{192, 168, 0, 7}),
		},
	))
	if c := soleCandidate(t, store); c.Address != "192.168.0.7:8080" {
		t.Errorf("Address = %q, want case-folded correlation to resolve", c.Address)
	}
}

// A PTR-only packet still observes the sighting — unlocated, named from the
// instance — and a LATER full announcement fills the address in. The reverse
// order must not lose it: a thin re-sighting keeps the learned address
// (the store's orKeep, exercised here through the lane end to end).
func TestThinAndFullPacketsConverge(t *testing.T) {
	l, store := newResolvingListener(t)
	thin := buildPTRPacket(t, "_waiveo._tcp.local.", "TV._waiveo._tcp.local.")
	full := packMsg(t,
		[]dnsmessage.Resource{ptrRec("_waiveo._tcp.local.", "TV._waiveo._tcp.local.")},
		[]dnsmessage.Resource{
			srvRec("TV._waiveo._tcp.local.", "tv.local.", 9),
			aRec("tv.local.", [4]byte{192, 168, 0, 5}),
		},
	)

	l.handlePacket(thin)
	if c := soleCandidate(t, store); c.Address != "" || c.Name != "TV" {
		t.Fatalf("thin sighting = {addr %q, name %q}, want unlocated but named", c.Address, c.Name)
	}
	l.handlePacket(full)
	if c := soleCandidate(t, store); c.Address != "192.168.0.5:9" {
		t.Fatalf("full announcement did not fill the address in: %q", c.Address)
	}
	l.handlePacket(thin)
	if c := soleCandidate(t, store); c.Address != "192.168.0.5:9" {
		t.Fatalf("a thin re-sighting erased the learned address: %q", c.Address)
	}
}

// Missing links yield no address rather than a guessed one: an SRV whose
// target has no address record, and an SRV port of zero (RFC 2782's
// "service decidedly not available"), both leave the sighting unlocated.
func TestUnresolvableAnnouncementsStayUnlocated(t *testing.T) {
	cases := []struct {
		name string
		pkt  func(t *testing.T) []byte
	}{
		{"srv without address record", func(t *testing.T) []byte {
			return packMsg(t,
				[]dnsmessage.Resource{ptrRec("_waiveo._tcp.local.", "TV._waiveo._tcp.local.")},
				[]dnsmessage.Resource{srvRec("TV._waiveo._tcp.local.", "tv.local.", 9)},
			)
		}},
		{"port zero", func(t *testing.T) []byte {
			return packMsg(t,
				[]dnsmessage.Resource{ptrRec("_waiveo._tcp.local.", "TV._waiveo._tcp.local.")},
				[]dnsmessage.Resource{
					srvRec("TV._waiveo._tcp.local.", "tv.local.", 0),
					aRec("tv.local.", [4]byte{192, 168, 0, 5}),
				},
			)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, store := newResolvingListener(t)
			l.handlePacket(tc.pkt(t))
			if c := soleCandidate(t, store); c.Address != "" {
				t.Errorf("Address = %q, want unlocated", c.Address)
			}
		})
	}
}

// IPv4 wins over IPv6 for one host — every consumer dials plain ip:port on a
// v4 lab LAN — but a v6-only host still resolves (bracketed, per
// net.JoinHostPort), so a v6-only device is not silently unlocatable.
func TestIPv4PreferredIPv6Fallback(t *testing.T) {
	v6 := [16]byte{0xfd, 0} // fd00::… — ULA, private, dialable
	v6[15] = 9

	l, store := newResolvingListener(t)
	l.handlePacket(packMsg(t,
		[]dnsmessage.Resource{ptrRec("_waiveo._tcp.local.", "TV._waiveo._tcp.local.")},
		[]dnsmessage.Resource{
			srvRec("TV._waiveo._tcp.local.", "tv.local.", 9),
			aaaaRec("tv.local.", v6),
			aRec("tv.local.", [4]byte{192, 168, 0, 5}),
		},
	))
	if c := soleCandidate(t, store); c.Address != "192.168.0.5:9" {
		t.Errorf("Address = %q, want the v4 endpoint preferred over v6", c.Address)
	}

	l2, store2 := newResolvingListener(t)
	l2.handlePacket(packMsg(t,
		[]dnsmessage.Resource{ptrRec("_waiveo._tcp.local.", "TV._waiveo._tcp.local.")},
		[]dnsmessage.Resource{
			srvRec("TV._waiveo._tcp.local.", "tv.local.", 9),
			aaaaRec("tv.local.", v6),
		},
	))
	if c := soleCandidate(t, store2); c.Address != "[fd00::9]:9" {
		t.Errorf("Address = %q, want the bracketed v6 fallback", c.Address)
	}
}

// instanceLabel and unescapeDNSLabel are pure and carry the sharp edges:
// suffix mismatch yields NO label (never a wrong one), presentation escapes
// decode, and a decode yielding invalid UTF-8 is refused so the store's
// poison rule can never drop the whole sighting over a name.
func TestInstanceLabel(t *testing.T) {
	cases := []struct {
		instance, owner, want string
	}{
		{"Living Room._waiveo._tcp.local.", "_waiveo._tcp.local.", "Living Room"},
		{"TV\\.2._waiveo._tcp.local.", "_waiveo._tcp.local.", "TV.2"},
		{"Caf\\195\\169._waiveo._tcp.local.", "_waiveo._tcp.local.", "Café"},
		{"TV._other._tcp.local.", "_waiveo._tcp.local.", ""},
		{"Bad\\255Name._waiveo._tcp.local.", "_waiveo._tcp.local.", ""},
		{"_waiveo._tcp.local.", "_waiveo._tcp.local.", ""},
	}
	for _, tc := range cases {
		if got := instanceLabel(tc.instance, tc.owner); got != tc.want {
			t.Errorf("instanceLabel(%q, %q) = %q, want %q", tc.instance, tc.owner, got, tc.want)
		}
	}
}

// The declared mDNS lane's own half of the same statement (#204). Asserted on
// the candidate this lane observes rather than on a merge outcome, because the
// defect it guards is that the value is never SET — which every merge test would
// pass through unchanged.
func TestTheDeclaredMDNSLaneStatesProductAuthorityForItsClass(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	l, err := New(Config{
		Watches:   []Watch{watchFor(mustMatch(t, `{"mdns":"_waiveo._tcp"}`))},
		Store:     store,
		NowMillis: func() int64 { return 1000 },
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	l.handlePacket(buildPTRPacket(t, "_waiveo._tcp.local.", "TheHanger._waiveo._tcp.local."))

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("the store holds %d candidates, want 1", len(cands))
	}
	if cands[0].ClassRank != deviceplane.ClassRankProduct {
		t.Fatalf("class_rank = %d, want %d (product) — a watch names one service type a pack asked for BY NAME, so the declaration is the evidence; left at the ladder's zero value this lane reports a class it is sure of as unranked",
			cands[0].ClassRank, deviceplane.ClassRankProduct)
	}
}
