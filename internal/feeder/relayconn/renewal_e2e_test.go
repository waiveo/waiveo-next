// renewal_e2e_test.go proves proactive certificate renewal (REL-015) end to
// end, both sides real: the feeder's enrollment + persistent-connection
// servers on one mTLS listener (the e2e harness), the relay's real identity
// store, dialer, reconnect supervisor, and renewal client. All expiry math
// rides injected clocks — the supervisor predicate's evaluation instant and
// the feeder's issuance clock (enroll.SetNowFunc) — never a wall-clock sleep.
package relayconn_test

import (
	"crypto/x509"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
	relayclient "github.com/maaxton/waiveo-next/internal/relay/relayconn"
)

// certNotAfterFromPEM reads a PEM-encoded certificate's NotAfter.
func certNotAfterFromPEM(t *testing.T, certPEM []byte) time.Time {
	t.Helper()
	der, _ := pemDecode(certPEM)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert.NotAfter
}

// TestProactiveRenewalMidSessionThenRedialWithFreshLeaf is the REL-015
// choreography proof: enroll → connect → the injected clock crosses
// NotAfter−window → the supervisor's mid-session renewal ticker fires →
// the store holds a FRESH serial under the same relay_id and keypair while
// the LIVE connection — authenticated with the now-superseded leaf — keeps
// working untouched (the contract's cutover sentence: superseded is not
// revoked, REL-016 gates on revocation only). When that connection later
// dies, the redial presents the fresh leaf: proven server-side by revoking
// the OLD serial first — a redial still presenting it would draw a typed
// CERT_REVOKED refusal and END supervision, so the reconnect that follows
// is only possible with the renewed certificate.
func TestProactiveRenewalMidSessionThenRedialWithFreshLeaf(t *testing.T) {
	h := newHarness(t)
	store := enrolledRelay(t, h)
	relayID, serialA := certSerial(t, store)

	idBefore, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity before renewal: ok=%v err=%v", ok, err)
	}
	notAfter := certNotAfterFromPEM(t, idBefore.CertPEM)

	// The injected evaluation clock the renewal predicate reads — real now
	// until the test advances it into the renewal window.
	var clockMs atomic.Int64
	clockMs.Store(time.Now().UnixMilli())

	const window = 30 * 24 * time.Hour

	var permanent atomic.Pointer[relayclient.Refusal]
	sup := relayclient.StartSupervisor(relayclient.SupervisorConfig{
		NeedsRenewal: func() bool {
			id, ok, err := store.Identity()
			if err != nil || !ok {
				return false
			}
			due, err := reenroll.ExpiresWithin(id.CertPEM, time.UnixMilli(clockMs.Load()), window)
			return err == nil && due
		},
		Renew: func() error { return reenroll.Renew(h.ts.URL, store) },
		Connect: func() (*relayclient.Client, error) {
			return relayclient.Dial(relayclient.Config{URL: h.ts.URL, Store: store, Declaration: testDeclaration})
		},
		OnPermanentRefusal:   func(r *relayclient.Refusal) { permanent.Store(r) },
		RenewalCheckInterval: 20 * time.Millisecond,
		InitialBackoff:       5 * time.Millisecond,
		MaxBackoff:           100 * time.Millisecond,
		MinStableUptime:      10 * time.Millisecond,
	})
	defer func() {
		sup.Stop()
		<-sup.Done()
	}()

	waitFor(t, 10*time.Second, func() bool { return sup.Client() != nil }, "supervisor never connected")
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() == 1 }, "connection never registered server-side")
	firstClient := sup.Client()

	// Advance the INJECTED clock into the renewal window; the mid-session
	// ticker notices on its next 20ms beat.
	clockMs.Store(notAfter.Add(-window + time.Hour).UnixMilli())

	waitFor(t, 10*time.Second, func() bool {
		_, serial := certSerial(t, store)
		return serial != serialA
	}, "renewal never persisted a fresh certificate")

	relayIDAfter, serialB := certSerial(t, store)
	if relayIDAfter != relayID {
		t.Fatalf("relay_id changed across renewal: %q -> %q (REL-014)", relayID, relayIDAfter)
	}
	idAfter, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity after renewal: ok=%v err=%v", ok, err)
	}
	if !idBefore.PrivateKey.Equal(idAfter.PrivateKey) {
		t.Error("private key rotated on proactive renewal — the player-pinned SPKI must survive (key reuse)")
	}
	// The feeder's issuance record reflects the contract: the new serial is
	// most-recently-issued; the old one is SUPERSEDED, never revoked.
	if head, _, ok := h.enrollSrv.MostRecentSerial(relayID); !ok || head != serialB {
		t.Errorf("feeder MostRecentSerial = %q ok=%v, want renewed serial %q", head, ok, serialB)
	}
	if h.enrollSrv.IsRevoked(relayID, serialA) {
		t.Error("renewal revoked the superseded serial — REL-015: superseded is not thereby revoked")
	}

	// The LIVE connection was authenticated with serial A and must be
	// untouched by the renewal: same client, not dead, and still serving
	// frames end-to-end (the per-frame gate checks revocation only).
	if got := sup.Client(); got != firstClient {
		t.Fatalf("supervisor swapped the live client on renewal — a renewal must not tear down the connection")
	}
	select {
	case <-firstClient.Done():
		t.Fatal("live connection died across the renewal — REL-015 forbids renewal tearing it down")
	default:
	}
	if _, err := firstClient.Pull("trace-post-renewal", nil); err != nil {
		t.Fatalf("Pull on the superseded-leaf connection after renewal: %v — the session must remain serviceable", err)
	}

	// Redial proof: revoke the OLD serial, then kill the live connection.
	// If the redial presented serial A it would draw typed CERT_REVOKED —
	// a PERMANENT refusal that ends supervision — so a successful reconnect
	// is only possible presenting the renewed leaf B.
	if !h.enrollSrv.Revoke(relayID, serialA) {
		t.Fatalf("Revoke(%s, %s) found no issuance on record", relayID, serialA)
	}
	_ = firstClient.Close()

	waitFor(t, 10*time.Second, func() bool {
		c := sup.Client()
		return c != nil && c != firstClient
	}, "supervisor never reconnected after the old-leaf session closed")
	if r := permanent.Load(); r != nil {
		t.Fatalf("redial drew a permanent refusal %v — it presented the revoked OLD leaf instead of the renewed one", r)
	}
	if _, err := sup.Client().Pull("trace-post-redial", nil); err != nil {
		t.Fatalf("Pull on the renewed-leaf connection: %v", err)
	}
}

// TestExpiredLeafHandshakeFallbackRenewsThenRedials proves the clock-skew
// belt-and-suspenders half of expiry classification with a GENUINELY
// expired certificate presented at a real mTLS handshake: the leaf is
// issued already-expired under the feeder's real CA (enroll.SetNowFunc —
// the injectable issuance clock), the renewal predicate deliberately
// reports not-due (a skewed relay clock believing the leaf valid), so the
// dial reaches the peer's TLS stack and dies on its expired-certificate
// alert — a transport-level error, not a typed refusal. The supervisor
// must classify that as renew-then-redial, not retry-forever: bounded here
// by requiring a live, freshly-authenticated connection within the test
// budget.
func TestExpiredLeafHandshakeFallbackRenewsThenRedials(t *testing.T) {
	h := newHarness(t)

	// Enroll with the feeder's issuance clock wound two years back: the
	// issued leaf's NotAfter (issuance + 365d) is a year in the real past —
	// genuinely expired, signed by the CA the listener trusts.
	h.enrollSrv.SetNowFunc(func() time.Time { return time.Now().Add(-2 * 365 * 24 * time.Hour) })
	store := enrolledRelay(t, h)
	h.enrollSrv.SetNowFunc(nil)

	relayID, serialA := certSerial(t, store)

	// Pin the classification premise: a direct dial dies at the handshake
	// with the TLS stack's expired-certificate alert — surfaced as a plain
	// transport error (no typed *Refusal), exactly what the supervisor's
	// string-match fallback keys on.
	_, err := relayclient.Dial(relayclient.Config{URL: h.ts.URL, Store: store, Declaration: testDeclaration})
	if err == nil {
		t.Fatal("Dial with a genuinely expired leaf succeeded, want a handshake failure")
	}
	var refusal *relayclient.Refusal
	if errors.As(err, &refusal) {
		t.Fatalf("expired-leaf dial drew a typed refusal %v, want a transport-level TLS handshake error", refusal)
	}

	var renews atomic.Int64
	sup := relayclient.StartSupervisor(relayclient.SupervisorConfig{
		// The skewed relay clock: never "due" — only the handshake fallback
		// can trigger renewal here.
		NeedsRenewal: func() bool { return false },
		Renew: func() error {
			renews.Add(1)
			return reenroll.Renew(h.ts.URL, store)
		},
		Connect: func() (*relayclient.Client, error) {
			return relayclient.Dial(relayclient.Config{URL: h.ts.URL, Store: store, Declaration: testDeclaration})
		},
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	})
	defer func() {
		sup.Stop()
		<-sup.Done()
	}()

	// Renewal-then-redial, bounded: the supervisor must end up CONNECTED —
	// endless expired-handshake retries never get here.
	waitFor(t, 15*time.Second, func() bool { return sup.Client() != nil },
		"supervisor never recovered from the expired leaf — expected renew-then-redial, not retry-forever")

	if got := renews.Load(); got < 1 {
		t.Fatalf("renewals = %d, want >= 1 (the handshake fallback must have driven the renewal)", got)
	}
	relayIDAfter, serialB := certSerial(t, store)
	if relayIDAfter != relayID {
		t.Fatalf("relay_id changed across expired-leaf renewal: %q -> %q (REL-014)", relayID, relayIDAfter)
	}
	if serialB == serialA {
		t.Fatal("serial unchanged — the store still holds the expired leaf")
	}
	// The renewed leaf is genuinely valid now.
	id, ok, err := store.Identity()
	if err != nil || !ok {
		t.Fatalf("Identity after renewal: ok=%v err=%v", ok, err)
	}
	expired, err := reenroll.ExpiresWithin(id.CertPEM, time.Now(), 0)
	if err != nil || expired {
		t.Fatalf("renewed leaf expired=%v err=%v, want a currently-valid certificate", expired, err)
	}
	if head, _, ok := h.enrollSrv.MostRecentSerial(relayID); !ok || head != serialB {
		t.Errorf("feeder MostRecentSerial = %q ok=%v, want renewed serial %q", head, ok, serialB)
	}
	if _, err := sup.Client().Pull("trace-after-expired-recovery", nil); err != nil {
		t.Fatalf("Pull on the recovered connection: %v", err)
	}
}
