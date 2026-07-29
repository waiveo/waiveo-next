// pairingredeemed_e2e_test.go proves relay/1 REL-124's upstream redemption
// report end to end over a REAL authenticated connection: the relay's own
// SendPairingRedeemed, across the mTLS + challenge/hello/hello-ack WS the
// enrollment established, into the app peer's real read loop and out to a
// wired RedemptionSink — and back as the `pairing.redeemed_ack` the relay may
// not retire an owed report without (REL-124a/REL-124d).
//
// The report is the one verb on this connection whose whole point is to change
// app-peer state on a RELAY's say-so, so the untrusted-input assertions are the
// substance here: attribution comes from the mTLS identity and nothing else,
// and a relay reaching for a grant bound to another relay is refused with the
// taxonomy's own code rather than acknowledged.
package relayconn_test

import (
	"errors"
	"sync"
	"testing"

	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	relayclient "github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// recordingRedemptionSink records what the connection layer handed it, and can
// be told to refuse — standing in for an app peer whose grant is bound
// elsewhere (ErrGrantBoundElsewhere) or whose store is failing (any other
// error).
type recordingRedemptionSink struct {
	mu       sync.Mutex
	applied  []appliedRedemption
	refuseAs error
}

type appliedRedemption struct {
	RelayID string
	Body    wire.PairingRedeemedBody
}

func (s *recordingRedemptionSink) ApplyRedemption(relayID string, body wire.PairingRedeemedBody) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, appliedRedemption{RelayID: relayID, Body: body})
	return s.refuseAs
}

func (s *recordingRedemptionSink) records() []appliedRedemption {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]appliedRedemption, len(s.applied))
	copy(out, s.applied)
	return out
}

func (s *recordingRedemptionSink) refuse(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseAs = err
}

// TestPairingRedeemedReportReachesTheSinkAndIsAcknowledged: the happy path, end
// to end. The relay's report crosses the real connection, the sink receives it
// attributed to the relay's ENROLLED identity (never anything the frame
// asserts), and SendPairingRedeemed returns only after the ack — which is the
// signal REL-124d lets a relay retire its owed report on.
func TestPairingRedeemedReportReachesTheSinkAndIsAcknowledged(t *testing.T) {
	sink := &recordingRedemptionSink{}
	h := newHarness(t, feederrelayconn.WithRedemptionSink(sink))
	store := enrolledRelay(t, h)
	id, _, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	client, _ := dialClient(t, h, store, nil)
	defer client.Close()

	body := wire.PairingRedeemedBody{GrantID: "grant-e2e-0123456789abcdef", RedeemedAt: 1752537600000}
	if err := client.SendPairingRedeemed(body); err != nil {
		t.Fatalf("SendPairingRedeemed: %v", err)
	}

	got := sink.records()
	if len(got) != 1 {
		t.Fatalf("sink received %d report(s), want 1", len(got))
	}
	if got[0].RelayID != id.RelayID {
		t.Errorf("report attributed to %q, want the connection's enrolled identity %q (REL-124b)", got[0].RelayID, id.RelayID)
	}
	if got[0].Body != body {
		t.Errorf("sink received %+v, want %+v carried unmodified", got[0].Body, body)
	}
}

// TestPairingRedeemedIsAttributedToTheConnectionNotTheFrame is the
// untrusted-input assertion. The report retires a grant, so a relay able to
// name ANOTHER relay here could cancel a pairing in progress at its sibling.
// REL-124b forbids honouring an asserted identity, and this drives the actual
// wire: a frame stamped with a foreign relay_id still lands attributed to the
// mTLS identity that authenticated the connection.
//
// The forged frame is emitted with the RAW client, because the production
// dialer stamps the connection's own identity and would never send this — the
// same arrangement TestAForgedRelayIdInTheFrameCannotReplaceAnotherRelaysView
// uses for `device.candidates`.
//
// Guard-disabled check: passing f.RelayID instead of the handshake's relayID
// into ApplyRedemption in handlePairingRedeemed makes the sink see relay A's
// identity on relay B's report, and this fails.
func TestPairingRedeemedIsAttributedToTheConnectionNotTheFrame(t *testing.T) {
	sink := &recordingRedemptionSink{}
	h := newHarness(t, feederrelayconn.WithRedemptionSink(sink))

	aStore := enrolledRelay(t, h)
	aID, _, err := aStore.Identity()
	if err != nil {
		t.Fatalf("store.Identity (relay A): %v", err)
	}
	bStore := enrolledRelay(t, h)
	bID, _, err := bStore.Identity()
	if err != nil {
		t.Fatalf("store.Identity (relay B): %v", err)
	}
	if aID.RelayID == bID.RelayID {
		t.Fatal("the two relays enrolled under one identity — this case cannot separate them")
	}

	ws, err := rawDial(t, h, bStore, bID.CertPEM, []string{wire.Subprotocol})
	if err != nil {
		t.Fatalf("rawDial (relay B): %v", err)
	}
	defer ws.CloseNow()
	rawHandshake(t, ws, bStore)

	forged, err := wire.NewFrame(wire.FrameTypePairingRedeemed, "forged-report-1",
		aID.RelayID, // the lie: relay B stamping relay A's identity
		wire.PairingRedeemedBody{GrantID: "grant-forged-0123456789abcd", RedeemedAt: 1752537600000})
	if err != nil {
		t.Fatalf("NewFrame(pairing.redeemed): %v", err)
	}
	if err := wsSend(t, ws, forged); err != nil {
		t.Fatalf("send forged report: %v", err)
	}
	var reply wire.Frame
	if err := wsRecv(t, ws, &reply); err != nil {
		t.Fatalf("read reply to the forged report: %v", err)
	}

	got := sink.records()
	if len(got) != 1 {
		t.Fatalf("sink received %d report(s), want 1", len(got))
	}
	if got[0].RelayID == aID.RelayID {
		t.Fatalf("the app peer attributed the report to the relay_id the FRAME asserted (%q) — relay B could retire relay A's grant (REL-124b)", aID.RelayID)
	}
	if got[0].RelayID != bID.RelayID {
		t.Errorf("report attributed to %q, want relay B's own enrolled identity %q", got[0].RelayID, bID.RelayID)
	}
}

// TestPairingRedeemedBoundElsewhereIsRefusedNotAcknowledged: the sink's
// bound-elsewhere refusal must reach the relay as the taxonomy's own
// PAIRING_REPORT_UNAUTHORIZED, never as an ack — an acknowledged report is one
// REL-124d lets the relay retire, and retiring a report the app peer refused
// would lose it. The connection stays up: a bad report is not a protocol
// violation.
func TestPairingRedeemedBoundElsewhereIsRefusedNotAcknowledged(t *testing.T) {
	sink := &recordingRedemptionSink{}
	sink.refuse(feederrelayconn.ErrGrantBoundElsewhere)
	h := newHarness(t, feederrelayconn.WithRedemptionSink(sink))
	store := enrolledRelay(t, h)
	client, _ := dialClient(t, h, store, nil)
	defer client.Close()

	err := client.SendPairingRedeemed(wire.PairingRedeemedBody{
		GrantID: "grant-elsewhere-0123456789ab", RedeemedAt: 1752537600000,
	})
	var refusal *relayclient.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("SendPairingRedeemed = %v, want a typed *Refusal", err)
	}
	if refusal.Code != "PAIRING_REPORT_UNAUTHORIZED" {
		t.Errorf("refusal code = %q, want PAIRING_REPORT_UNAUTHORIZED (REL-124b)", refusal.Code)
	}

	// The connection survives the refusal, and a subsequent well-formed
	// exchange still works.
	sink.refuse(nil)
	if err := client.SendPairingRedeemed(wire.PairingRedeemedBody{
		GrantID: "grant-after-refusal-01234567", RedeemedAt: 1752537600000,
	}); err != nil {
		t.Fatalf("the connection did not survive a refused report: %v", err)
	}
}

// TestPairingRedeemedRecordFailureIsNotAcknowledged: an app peer that cannot
// record the report answers a typed error rather than an ack, so the relay
// keeps the redemption owed and re-sends it (REL-124b/REL-124d). Acknowledging
// a report nothing recorded is exactly the silent loss the ack discipline
// exists to prevent — the same reason REL-092 gates the telemetry buffer on
// ack_through_seq.
func TestPairingRedeemedRecordFailureIsNotAcknowledged(t *testing.T) {
	sink := &recordingRedemptionSink{}
	sink.refuse(errors.New("the store is unavailable"))
	h := newHarness(t, feederrelayconn.WithRedemptionSink(sink))
	store := enrolledRelay(t, h)
	client, _ := dialClient(t, h, store, nil)
	defer client.Close()

	err := client.SendPairingRedeemed(wire.PairingRedeemedBody{
		GrantID: "grant-unrecordable-012345678", RedeemedAt: 1752537600000,
	})
	var refusal *relayclient.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("SendPairingRedeemed = %v, want a typed *Refusal so the report stays owed", err)
	}
	if refusal.Code != "INTERNAL" {
		t.Errorf("refusal code = %q, want INTERNAL — the grant selector was fine, this side's record was not", refusal.Code)
	}
}

// TestPairingRedeemedWithNoSinkIsStillAcknowledged: REL-124's obligation is the
// RELAY's (to report), and the audit record's own shape is explicitly out of
// relay/1's scope — so a deployment that keeps no such record is conformant and
// must still acknowledge, or every relay talking to it would re-send its whole
// ledger forever (REL-124d).
func TestPairingRedeemedWithNoSinkIsStillAcknowledged(t *testing.T) {
	h := newHarness(t) // no RedemptionSink wired
	store := enrolledRelay(t, h)
	client, _ := dialClient(t, h, store, nil)
	defer client.Close()

	if err := client.SendPairingRedeemed(wire.PairingRedeemedBody{
		GrantID: "grant-no-sink-0123456789abc", RedeemedAt: 1752537600000,
	}); err != nil {
		t.Fatalf("SendPairingRedeemed with no sink wired: %v", err)
	}
}
