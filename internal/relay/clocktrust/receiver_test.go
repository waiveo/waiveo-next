package clocktrust

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
)

// The REL-133 corpus fixture's wire bytes, verbatim, so the receiver is driven
// by an actual clock.hint envelope rather than a hand-built struct.
const (
	rel133Hint1Wire = `{"type":"clock.hint","relay_id":"01J8Z4K4N5P6Q7R8S9T0V1W3A1","body":{"ts":1752537600000}}`
	rel133Hint2Wire = `{"type":"clock.hint","relay_id":"01J8Z4K4N5P6Q7R8S9T0V1W3A1","body":{"ts":1784074200001}}`
)

// The wire receiver decodes and applies the in-bound corpus hint (hint_1),
// adjusting the runtime clock; the out-of-bound one (hint_2) is decoded but
// declined.
func TestHintReceiver_Receive_CorpusHints(t *testing.T) {
	const nowMono = int64(5000)
	rc := newTestClock(nowMono)
	recv := NewHintReceiver(rc, rel133CertNotAfter, rel133BoundedGrace)

	accepted, err := recv.Receive([]byte(rel133Hint1Wire))
	if err != nil {
		t.Fatalf("receive in-bound hint: %v", err)
	}
	if !accepted {
		t.Fatal("in-bound corpus hint_1 not accepted by the wire receiver")
	}
	if !rc.Adjusted() {
		t.Error("runtime clock not adjusted after accepting hint_1")
	}
	if got := rc.WallMillis(); got != rel133Hint1TS {
		t.Errorf("WallMillis() = %d, want %d", got, rel133Hint1TS)
	}

	accepted, err = recv.Receive([]byte(rel133Hint2Wire))
	if err != nil {
		t.Fatalf("receive out-of-bound hint: %v", err)
	}
	if accepted {
		t.Error("out-of-bound corpus hint_2 accepted by the wire receiver, want declined")
	}
	// The declined hint left the earlier adjustment intact.
	if got := rc.WallMillis(); got != rel133Hint1TS {
		t.Errorf("WallMillis() after declined hint_2 = %d, want %d", got, rel133Hint1TS)
	}
}

func TestHintReceiver_Receive_Malformed(t *testing.T) {
	rc := newTestClock(5000)
	recv := NewHintReceiver(rc, rel133CertNotAfter, rel133BoundedGrace)

	if _, err := recv.Receive([]byte(`{not json`)); err == nil {
		t.Error("malformed body: want decode error")
	}
	if _, err := recv.Receive([]byte(`{"type":"hello","body":{"ts":1}}`)); err == nil {
		t.Error("wrong message type: want error, not a silent no-op")
	}
	if rc.Adjusted() {
		t.Error("malformed/wrong-type inputs adjusted the clock")
	}
}

// ServeHTTP is the end-to-end connection-layer path: a POSTed clock.hint
// reaches AcceptHint through the receiver, and the persisted floor never moves.
func TestHintReceiver_ServeHTTP_EndToEnd_NeverAdvancesFloor(t *testing.T) {
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("open identity store: %v", err)
	}
	defer store.Close()
	if _, err := store.SetClockFloor(rel133PersistedFloor); err != nil {
		t.Fatalf("seed clock floor: %v", err)
	}

	const nowMono = int64(5000)
	rc := newTestClock(nowMono)
	recv := NewHintReceiver(rc, rel133CertNotAfter, rel133BoundedGrace)

	srv := httptest.NewServer(http.HandlerFunc(recv.ServeHTTP))
	defer srv.Close()

	// In-bound hint over the wire.
	resp, err := http.Post(srv.URL, "application/json", bytes.NewBufferString(rel133Hint1Wire))
	if err != nil {
		t.Fatalf("POST clock.hint: %v", err)
	}
	var body struct {
		Accepted      bool  `json:"accepted"`
		RuntimeWallMs int64 `json:"runtime_wall_ms"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if !body.Accepted {
		t.Error("POSTed in-bound hint not accepted")
	}
	if body.RuntimeWallMs != rel133Hint1TS {
		t.Errorf("runtime_wall_ms = %d, want %d", body.RuntimeWallMs, rel133Hint1TS)
	}

	// Out-of-bound hint over the wire: 200 with accepted=false.
	resp2, err := http.Post(srv.URL, "application/json", bytes.NewBufferString(rel133Hint2Wire))
	if err != nil {
		t.Fatalf("POST out-of-bound clock.hint: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("out-of-bound hint status = %d, want 200 (a declined hint is not a protocol error)", resp2.StatusCode)
	}
	resp2.Body.Close()

	// GET is rejected.
	resp3, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp3.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp3.StatusCode)
	}
	resp3.Body.Close()

	// The persisted floor is exactly what it was seeded to — a hint over the
	// wire never advanced it (REL-132).
	floor, ok, err := store.ClockFloor()
	if err != nil || !ok {
		t.Fatalf("read clock floor: ok=%v err=%v", ok, err)
	}
	if floor != rel133PersistedFloor {
		t.Errorf("persisted floor after wire hints = %d, want %d (a hint never advances the floor)", floor, rel133PersistedFloor)
	}
}
