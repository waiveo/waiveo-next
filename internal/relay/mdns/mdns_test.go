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
// answer whose owner name is ownerName and whose RDATA (the specific service
// instance, never consulted by matching) is target.
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

func TestNewValidation(t *testing.T) {
	waiveoPattern := []deviceplane.Match{mustMatch(t, `{"mdns":"_waiveo._tcp"}`)}
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }

	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "nil Store",
			cfg:  Config{Patterns: waiveoPattern, Store: nil, NowMillis: now},
		},
		{
			name: "nil NowMillis",
			cfg:  Config{Patterns: waiveoPattern, Store: store, NowMillis: nil},
		},
		{
			name: "no patterns at all",
			cfg:  Config{Patterns: nil, Store: store, NowMillis: now},
		},
		{
			name: "only non-MDNS patterns",
			cfg: Config{
				Patterns:  []deviceplane.Match{mustMatch(t, `{"ssdp":"urn:roku-com:device:player:1"}`)},
				Store:     store,
				NowMillis: now,
			},
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

func TestNewOK(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pat := []deviceplane.Match{mustMatch(t, `{"mdns":"_waiveo._tcp"}`)}

	l, err := New(Config{Patterns: pat, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if len(l.byServiceType) != 1 {
		t.Fatalf("byServiceType has %d entries, want 1", len(l.byServiceType))
	}
	if _, ok := l.byServiceType["_waiveo._tcp"]; !ok {
		t.Errorf("byServiceType missing _waiveo._tcp: %+v", l.byServiceType)
	}
}

// TestHandlePacketObservesMatchingPTR asserts a PTR answer whose owner name,
// normalized, exactly matches the configured pattern is Observed into the
// Store (REL-110/111).
func TestHandlePacketObservesMatchingPTR(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Patterns: []deviceplane.Match{pattern}, Store: store, NowMillis: now})
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

// TestHandlePacketIgnoresNonMatchingPTR asserts a PTR answer whose owner name
// does not match any configured pattern is never Observed.
func TestHandlePacketIgnoresNonMatchingPTR(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Patterns: []deviceplane.Match{pattern}, Store: store, NowMillis: now})
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

	l, err := New(Config{Patterns: []deviceplane.Match{pattern}, Store: store, NowMillis: now})
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

	l, err := New(Config{Patterns: []deviceplane.Match{pattern}, Store: store, NowMillis: now})
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
// own Match.Key() dedup, REL-110/111).
func TestHandlePacketRepeatedHitsBumpLastSeenNotDuplicate(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	nowVal := int64(1000)
	now := func() int64 { return nowVal }
	pattern := mustMatch(t, `{"mdns":"_waiveo._tcp"}`)

	l, err := New(Config{Patterns: []deviceplane.Match{pattern}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	data := buildPTRPacket(t, "_waiveo._tcp.local.", "TheHanger._waiveo._tcp.local.")
	l.handlePacket(data)
	nowVal = 2000
	l.handlePacket(data)

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1 (dedup by Match.Key()): %+v", len(cands), cands)
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

	l, err := New(Config{Patterns: []deviceplane.Match{pattern}, Store: store, NowMillis: now})
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

	if fake.closeCallCount() < 1 {
		t.Error("Run() did not close the packetSource on ctx cancel")
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

	l, err := New(Config{Patterns: []deviceplane.Match{pattern}, Store: store, NowMillis: now})
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

	l, err := New(Config{Patterns: []deviceplane.Match{pattern}, Store: store, NowMillis: now})
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

	l, err := New(Config{Patterns: []deviceplane.Match{pattern}, Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() against the real multicast socket: %v", err)
	}
}
