package relaystatus

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
)

// enrolledStore builds a store shaped like a relay that has enrolled, applied a
// generation, paired a screen and buffered telemetry — using the SAME writers
// the relay itself uses, so the reader is proved against the real schema rather
// than against a fixture that agrees with it by construction.
func enrolledStore(t *testing.T, notAfter time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.db")
	st, err := identity.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(4242),
		Subject:      pkix.Name{CommonName: "relay-under-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := st.SetIdentity("relay-under-test", certPEM, priv); err != nil {
		t.Fatal(err)
	}
	dsPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDesiredStateVerificationKey(dsPub); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAppPeerTrustPin([]byte("spki-bytes")); err != nil {
		t.Fatal(err)
	}

	programs, _ := json.Marshal([]map[string]any{
		{"screen_id": "screen-a", "program_revision": "r1", "priority": "schedule", "display": "image",
			"content": []map[string]any{{"asset_ref": "sha256:aa", "url": "https://x/aa"}}},
	})
	revoked, _ := json.Marshal([]string{"screen-z"})
	inventory, _ := json.Marshal(map[string]any{
		"devices":             []json.RawMessage{json.RawMessage(`{"entity_id":"roku.one"}`)},
		"pack_match_patterns": []json.RawMessage{},
	})
	if err := st.ApplyGeneration(19, "hash-nineteen-abcdef", programs, revoked, inventory); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetClockFloor(time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	// A DURABLE-class schema: the store refuses to persist a latest-only one
	// (REL-094), so a `box.vitals` entry here would leave the queue empty and this
	// fixture would silently be testing zero rows.
	if err := st.AppendTelemetry(telemetry.Entry{Seq: 5, Schema: telemetry.SchemaContentPlayed, Payload: json.RawMessage(`{}`), RecordedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSeqHighWater(9); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).UnixMilli()
	if err := st.SetPlayerSession(identity.HashToken("tok-live"), "screen-a", future); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPlayerSession(identity.HashToken("tok-stale"), "screen-b", time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRedemption("grant-1", time.Now().UnixMilli(), true); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadReportsWhatTheRelayPersisted(t *testing.T) {
	path := enrolledStore(t, time.Now().Add(30*24*time.Hour))
	rep, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if rep.Identity == nil {
		t.Fatalf("no identity read; problems: %v", rep.Problems)
	}
	if rep.Identity.RelayID != "relay-under-test" {
		t.Errorf("relay_id = %q", rep.Identity.RelayID)
	}
	if rep.Identity.Expired {
		t.Error("a certificate valid for 30 more days was reported expired")
	}
	if rep.Identity.DaysRemaining < 28 || rep.Identity.DaysRemaining > 30 {
		t.Errorf("days_remaining = %d, want ~30", rep.Identity.DaysRemaining)
	}
	if rep.Identity.SPKISHA256 == "" {
		t.Error("no SPKI digest")
	}
	if !rep.Trust.DesiredStateKey || !rep.Trust.AppPeerPin {
		t.Errorf("trust material not reported: %+v", rep.Trust)
	}
	if rep.Applied == nil || rep.Applied.Generation != 19 {
		t.Fatalf("applied generation not read: %+v", rep.Applied)
	}
	if rep.Applied.ScreenPrograms != 1 || rep.Applied.ContentRefs != 1 ||
		rep.Applied.RevokedScreens != 1 || rep.Applied.AdoptedDevices != 1 {
		t.Errorf("applied counts wrong: %+v", rep.Applied)
	}
	if len(rep.Applied.ScreenIDs) != 1 || rep.Applied.ScreenIDs[0] != "screen-a" {
		t.Errorf("screen ids = %v", rep.Applied.ScreenIDs)
	}
	if rep.Clock == nil {
		t.Error("no clock floor read")
	}
	if rep.Telemetry.Queued != 1 || rep.Telemetry.HighWaterSeq != 9 {
		t.Errorf("telemetry = %+v", rep.Telemetry)
	}
	if rep.Sessions.Screens != 2 || rep.Sessions.Live != 1 || rep.Sessions.Expired != 1 {
		t.Errorf("sessions = %+v", rep.Sessions)
	}
	if rep.Sessions.RedeemedGrants != 1 {
		t.Errorf("redeemed grants = %d", rep.Sessions.RedeemedGrants)
	}
	if len(rep.Problems) != 0 {
		t.Errorf("unexpected problems: %v", rep.Problems)
	}
}

// TestAnExpiredCertificateIsReportedAsExpired: a lapsed enrollment leaf is the
// condition where the relay stops being able to connect at all, and it is
// invisible in any count of rows.
func TestAnExpiredCertificateIsReportedAsExpired(t *testing.T) {
	path := enrolledStore(t, time.Now().Add(-time.Hour))
	rep, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Identity == nil || !rep.Identity.Expired {
		t.Fatalf("an expired certificate was not reported: %+v", rep.Identity)
	}
	if rep.Identity.DaysRemaining > 0 {
		t.Errorf("days_remaining = %d for an expired leaf", rep.Identity.DaysRemaining)
	}
}

// TestReadingDoesNotWriteTheStore is the load-bearing property: this runs against
// a LIVE relay's file. identity.Open would create tables and run migrations, and
// pointing that at a running relay would have a diagnostic mutate what it
// inspects.
func TestReadingDoesNotWriteTheStore(t *testing.T) {
	path := enrolledStore(t, time.Now().Add(time.Hour))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || string(beforeBytes) != string(afterBytes) {
		t.Error("reading the store changed it")
	}
}

// TestAnAbsentStoreIsAnAnswerNotAnError: a relay that has never enrolled has no
// file, and that IS the diagnosis a first-boot operator needs.
func TestAnAbsentStoreIsAnAnswerNotAnError(t *testing.T) {
	rep, err := Read(filepath.Join(t.TempDir(), "nothing-here.db"))
	if err != nil {
		t.Fatalf("an absent store was raised as an error: %v", err)
	}
	if rep.Identity != nil {
		t.Error("an identity was reported for a store that does not exist")
	}
	if len(rep.Problems) == 0 || !strings.Contains(rep.Problems[0], "never enrolled") {
		t.Errorf("the absence was not explained: %v", rep.Problems)
	}
}

// TestTheReportNamesWhatItCannotSee: a diagnostic that showed green lines and
// stayed silent about the connection state it never checked would have the
// operator conclude the relay was connected.
func TestTheReportNamesWhatItCannotSee(t *testing.T) {
	rep, err := Read(enrolledStore(t, time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(rep.Blind, "\n")
	for _, want := range []string{"connection state", "clock TRUST", "liveness"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the blind-spot list does not mention %q:\n%s", want, joined)
		}
	}
}

// TestNoCredentialMaterialIsRendered: the store holds a private key and channel
// token hashes. A diagnostic that printed either would turn filesystem access
// into protocol access.
func TestNoCredentialMaterialIsRendered(t *testing.T) {
	path := enrolledStore(t, time.Now().Add(time.Hour))
	rep, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"PRIVATE KEY", identity.HashToken("tok-live"), "tok-live"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the report renders %q", forbidden)
		}
	}
}

func TestHealthzReportsWhatTheRelayAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"component":"waiveo-relay","status":"ok","vitals":{"uptime_s":42},"vitals_unavailable":["disk"]}`))
	}))
	t.Cleanup(srv.Close)

	got := Healthz(context.Background(), srv.URL+"/healthz", &tls.Config{MinVersion: tls.VersionTLS12})
	if got.Error != "" || got.Status != 200 {
		t.Fatalf("healthz probe failed: %+v", got)
	}
	if got.Component != "waiveo-relay" || got.Reported != "ok" {
		t.Errorf("healthz payload lost: %+v", got)
	}
	if got.Vitals["uptime_s"] != float64(42) || len(got.VitalsUnavailable) != 1 {
		t.Errorf("vitals lost: %+v", got)
	}
}

func TestHealthzReportsUnreachableRatherThanPanicking(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	got := Healthz(context.Background(), url+"/healthz", &tls.Config{MinVersion: tls.VersionTLS12})
	if got.Error == "" {
		t.Error("a dead relay was reported reachable")
	}
}
