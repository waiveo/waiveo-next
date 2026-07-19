package hello

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

// rel030Corpus is the frozen relay/1 differential-oracle fixture Task 1's
// wire types are checked against field-for-field (contracts/relay-1.md
// REL-030/031, and REL-033-039 for the shape of hello-ack). Task 3+ reuse
// this same fixture for negotiation/feature-subset behavior; this file only
// asserts the wire shapes.
const rel030Corpus = "../../../conformance/corpora/relay-1/REL-030-valid-hello-negotiate-channel-binding.json"

// rel030Doc decodes exactly the sub-documents this file's tests replay:
// the challenge and hello the corpus hands the relay/feeder, the app
// peer's own declared implemented-minors set, and the hello-ack plus the
// three scalar verdict fields (channel_binding_verified,
// unrecognized_feature_flag_dropped_silently, connection_refused) it
// expects back. Kept as json.RawMessage for the message shapes (so each
// test can unmarshal, and separately round-trip re-marshal, them against
// this package's own typed structs rather than a hand-copied literal); the
// scalar verdict fields are decoded directly since TestREL030EndToEnd
// asserts against them by value, not by shape.
type rel030Doc struct {
	Input struct {
		AppPeerImplementedMinors []string        `json:"app_peer_implemented_minors"`
		Challenge                json.RawMessage `json:"challenge"`
		Hello                    json.RawMessage `json:"hello"`
	} `json:"input"`
	Expected struct {
		ChannelBindingVerified                 bool            `json:"channel_binding_verified"`
		HelloAck                               json.RawMessage `json:"hello_ack"`
		UnrecognizedFeatureFlagDroppedSilently bool            `json:"unrecognized_feature_flag_dropped_silently"`
		ConnectionRefused                      bool            `json:"connection_refused"`
	} `json:"expected"`
}

func loadRel030(t *testing.T) rel030Doc {
	t.Helper()
	raw, err := os.ReadFile(rel030Corpus)
	if err != nil {
		t.Fatalf("read corpus %s: %v", rel030Corpus, err)
	}
	var doc rel030Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal corpus %s: %v", rel030Corpus, err)
	}
	return doc
}

// asAny decodes raw into a generic interface{} tree (map[string]interface{},
// []interface{}, float64, string, bool, nil) — the natural comparison form
// for "does this JSON carry the same fields/values" regardless of Go struct
// field order or map key order.
func asAny(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal into any: %v (%s)", err, raw)
	}
	return v
}

// TestChallengeMatchesCorpusFieldNames asserts Challenge unmarshals the
// REL-030 corpus's `challenge` fixture and re-marshals to the exact same
// field names/values (REL-030): {type:"challenge", body:{nonce}}.
func TestChallengeMatchesCorpusFieldNames(t *testing.T) {
	doc := loadRel030(t)

	var c Challenge
	if err := json.Unmarshal(doc.Input.Challenge, &c); err != nil {
		t.Fatalf("unmarshal Challenge: %v", err)
	}

	if c.Type != "challenge" {
		t.Errorf("Type = %q, want %q", c.Type, "challenge")
	}
	if c.Body.Nonce != "b3f1c2a9d4e5f60718293a4b5c6d7e8f" {
		t.Errorf("Body.Nonce = %q, want the corpus fixture's nonce", c.Body.Nonce)
	}

	got, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal Challenge: %v", err)
	}

	want := asAny(t, doc.Input.Challenge)
	gotAny := asAny(t, got)
	if !reflect.DeepEqual(gotAny, want) {
		t.Errorf("Challenge round-trip mismatch:\n got  %s\n want %s", got, doc.Input.Challenge)
	}
}

// TestHelloMatchesCorpusFieldNames asserts Hello unmarshals the REL-030
// corpus's `hello` fixture and re-marshals to the exact same field names
// (REL-031): {type, relay_id, body:{protocol_version, features,
// site_binding{scope_node,tz,lat,long}, subnet_metadata{advertised_address},
// clock_state{state,source}, channel_binding_signature}}.
func TestHelloMatchesCorpusFieldNames(t *testing.T) {
	doc := loadRel030(t)

	var h Hello
	if err := json.Unmarshal(doc.Input.Hello, &h); err != nil {
		t.Fatalf("unmarshal Hello: %v", err)
	}

	if h.Type != "hello" {
		t.Errorf("Type = %q, want %q", h.Type, "hello")
	}
	if h.RelayID != "01J8Z4K4N5P6Q7R8S9T0V1W3A1" {
		t.Errorf("RelayID = %q, want the corpus fixture's relay_id", h.RelayID)
	}
	if h.Body.ProtocolVersion != "1.0" {
		t.Errorf("Body.ProtocolVersion = %q, want %q", h.Body.ProtocolVersion, "1.0")
	}
	wantFeatures := []string{"telemetry.latest_only_v1", "an-unrecognized-future-flag"}
	if !reflect.DeepEqual(h.Body.Features, wantFeatures) {
		t.Errorf("Body.Features = %v, want %v", h.Body.Features, wantFeatures)
	}
	wantSiteBinding := SiteBinding{
		ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
		TZ:        "America/Chicago",
		Lat:       41.8781,
		Long:      -87.6298,
	}
	if h.Body.SiteBinding != wantSiteBinding {
		t.Errorf("Body.SiteBinding = %+v, want %+v", h.Body.SiteBinding, wantSiteBinding)
	}
	if h.Body.SubnetMetadata.AdvertisedAddress != "192.0.2.12" {
		t.Errorf("Body.SubnetMetadata.AdvertisedAddress = %q, want %q", h.Body.SubnetMetadata.AdvertisedAddress, "192.0.2.12")
	}
	wantClockState := ClockState{State: "trusted", Source: "ntp"}
	if h.Body.ClockState != wantClockState {
		t.Errorf("Body.ClockState = %+v, want %+v", h.Body.ClockState, wantClockState)
	}
	if h.Body.ChannelBindingSignature != "ed25519-sig-over-nonce-b3f1c2a9d4e5f60718293a4b5c6d7e8f-signed-with-relay-enrollment-key" {
		t.Errorf("Body.ChannelBindingSignature = %q, want the corpus fixture's signature", h.Body.ChannelBindingSignature)
	}

	got, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal Hello: %v", err)
	}

	want := asAny(t, doc.Input.Hello)
	gotAny := asAny(t, got)
	if !reflect.DeepEqual(gotAny, want) {
		t.Errorf("Hello round-trip mismatch:\n got  %s\n want %s", got, doc.Input.Hello)
	}
}

// TestHelloAckMatchesCorpusFieldNames asserts HelloAck unmarshals the
// REL-030 corpus's expected `hello_ack` fixture and re-marshals to the exact
// same field names (REL-039): {type, relay_id, body:{negotiated_version,
// features, site_binding, deprecated}} — including an empty (not omitted,
// not null) `deprecated` object.
func TestHelloAckMatchesCorpusFieldNames(t *testing.T) {
	doc := loadRel030(t)

	var ack HelloAck
	if err := json.Unmarshal(doc.Expected.HelloAck, &ack); err != nil {
		t.Fatalf("unmarshal HelloAck: %v", err)
	}

	if ack.Type != "hello-ack" {
		t.Errorf("Type = %q, want %q", ack.Type, "hello-ack")
	}
	if ack.RelayID != "01J8Z4K4N5P6Q7R8S9T0V1W3A1" {
		t.Errorf("RelayID = %q, want the corpus fixture's relay_id", ack.RelayID)
	}
	if ack.Body.NegotiatedVersion != "1.0" {
		t.Errorf("Body.NegotiatedVersion = %q, want %q", ack.Body.NegotiatedVersion, "1.0")
	}
	wantFeatures := []string{"telemetry.latest_only_v1"}
	if !reflect.DeepEqual(ack.Body.Features, wantFeatures) {
		t.Errorf("Body.Features = %v, want %v (unrecognized flag must NOT reappear here, REL-035)", ack.Body.Features, wantFeatures)
	}
	wantSiteBinding := SiteBinding{
		ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
		TZ:        "America/Chicago",
		Lat:       41.8781,
		Long:      -87.6298,
	}
	if ack.Body.SiteBinding != wantSiteBinding {
		t.Errorf("Body.SiteBinding = %+v, want %+v", ack.Body.SiteBinding, wantSiteBinding)
	}
	if ack.Body.Deprecated == nil {
		t.Errorf("Body.Deprecated = nil, want a non-nil (empty) map so it marshals as {} not null")
	}
	if len(ack.Body.Deprecated) != 0 {
		t.Errorf("Body.Deprecated = %v, want empty", ack.Body.Deprecated)
	}

	got, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal HelloAck: %v", err)
	}

	want := asAny(t, doc.Expected.HelloAck)
	gotAny := asAny(t, got)
	if !reflect.DeepEqual(gotAny, want) {
		t.Errorf("HelloAck round-trip mismatch:\n got  %s\n want %s", got, doc.Expected.HelloAck)
	}
}

// TestHelloAckDeprecatedNoticeFieldNames asserts DeprecationNotice's own
// field names match REL-039's {deprecated_in, removed_in, message} shape —
// the REL-030 corpus's own deprecated map happens to be empty, so this
// exercises a populated entry directly rather than relying on the corpus.
func TestHelloAckDeprecatedNoticeFieldNames(t *testing.T) {
	ack := HelloAck{
		Type:    "hello-ack",
		RelayID: "01J8Z4K4N5P6Q7R8S9T0V1W3A1",
		Body: HelloAckBody{
			NegotiatedVersion: "1.0",
			Features:          []string{},
			SiteBinding:       SiteBinding{ScopeNode: "x", TZ: "UTC", Lat: 0, Long: 0},
			Deprecated: map[string]DeprecationNotice{
				"state.pull": {
					DeprecatedIn: "1.0",
					RemovedIn:    "2.0",
					Message:      "use state.pull2 instead",
				},
			},
		},
	}

	raw, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	body := got["body"].(map[string]any)
	deprecated := body["deprecated"].(map[string]any)
	notice := deprecated["state.pull"].(map[string]any)
	for _, k := range []string{"deprecated_in", "removed_in", "message"} {
		if _, ok := notice[k]; !ok {
			t.Errorf("DeprecationNotice JSON missing field %q; got %s", k, raw)
		}
	}
}

// TestREL030EndToEndChannelBindingVerifiedAndNegotiated drives the REL-030
// corpus case through the actual negotiation/verification logic
// (BuildHelloAck), rather than round-tripping a pre-computed literal —
// closing the gap where TestHelloAckMatchesCorpusFieldNames alone would
// pass identically whether Negotiate/IntersectFeatures/VerifyChannelBinding
// were correct, wrong, or entirely absent.
//
// The corpus's own `channel_binding_signature` string
// ("ed25519-sig-over-nonce-...-signed-with-relay-enrollment-key") is a
// human-readable description, not a real signature — the same convention
// the REL-027 and REL-071 corpus fixtures use for their own `pop_signature`/
// `signature` fields (relay-1's frozen corpus never embeds a working
// keypair). This test substitutes a REAL ed25519 signature, computed with
// SignChannelBinding over the corpus's own challenge nonce, for that
// placeholder — everything else (protocol_version, features,
// app_peer_implemented_minors, site_binding) is exactly the corpus's own
// data — and asserts the corpus's declared verdict fields
// (channel_binding_verified, unrecognized_feature_flag_dropped_silently,
// connection_refused) against BuildHelloAck's actual behavior.
func TestREL030EndToEndChannelBindingVerifiedAndNegotiated(t *testing.T) {
	doc := loadRel030(t)

	var c Challenge
	if err := json.Unmarshal(doc.Input.Challenge, &c); err != nil {
		t.Fatalf("unmarshal Challenge: %v", err)
	}
	var h Hello
	if err := json.Unmarshal(doc.Input.Hello, &h); err != nil {
		t.Fatalf("unmarshal Hello: %v", err)
	}
	var wantAck HelloAck
	if err := json.Unmarshal(doc.Expected.HelloAck, &wantAck); err != nil {
		t.Fatalf("unmarshal expected HelloAck: %v", err)
	}

	relayPub, relayPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	realSig, err := SignChannelBinding(relayPriv, c.Body.Nonce)
	if err != nil {
		t.Fatalf("SignChannelBinding: %v", err)
	}
	h.Body.ChannelBindingSignature = realSig

	// appPeerRecognizedFeatures: the app peer recognizes exactly
	// "telemetry.latest_only_v1" — the corpus's expected hello-ack features
	// set — and nothing else, so "an-unrecognized-future-flag" is dropped
	// for being unrecognized, not for any other reason.
	appPeerRecognizedFeatures := []string{"telemetry.latest_only_v1"}

	ack, err := BuildHelloAck(
		h,
		c.Body.Nonce,
		relayPub,
		doc.Input.AppPeerImplementedMinors,
		appPeerRecognizedFeatures,
		wantAck.Body.SiteBinding,
		wantAck.Body.Deprecated,
	)

	gotRefused := err != nil
	if gotRefused != doc.Expected.ConnectionRefused {
		t.Fatalf("BuildHelloAck refused = %v (err=%v), want connection_refused = %v", gotRefused, err, doc.Expected.ConnectionRefused)
	}
	if err != nil {
		// connection_refused is expected true in this branch; nothing further
		// to assert against hello-ack since none is produced.
		return
	}

	if !doc.Expected.ChannelBindingVerified {
		t.Fatal("BuildHelloAck accepted the connection, but the corpus declares channel_binding_verified = false")
	}

	if !reflect.DeepEqual(ack, wantAck) {
		t.Errorf("BuildHelloAck result:\n got  %+v\n want %+v", ack, wantAck)
	}

	droppedUnrecognized := !containsString(ack.Body.Features, "an-unrecognized-future-flag")
	if droppedUnrecognized != doc.Expected.UnrecognizedFeatureFlagDroppedSilently {
		t.Errorf("unrecognized flag dropped = %v, want %v (features = %v)", droppedUnrecognized, doc.Expected.UnrecognizedFeatureFlagDroppedSilently, ack.Body.Features)
	}
}

// TestREL030EndToEndRefusesWrongChannelBindingSignature is the negative
// counterpart to TestREL030EndToEndChannelBindingVerifiedAndNegotiated: the
// exact same corpus-derived hello, but signed with a DIFFERENT relay's
// private key than the one the app peer looks up (simulating an impostor,
// or a relay presenting a signature computed with the wrong key). REL-032
// mandates refusal (CHANNEL_BINDING_INVALID) here EVEN THOUGH every other
// field — protocol_version, features, site_binding — is identical to the
// accepted case, and even though (per REL-032's own wording) this MUST hold
// regardless of whether the connection's own mutual-TLS handshake already
// succeeded.
func TestREL030EndToEndRefusesWrongChannelBindingSignature(t *testing.T) {
	doc := loadRel030(t)

	var c Challenge
	if err := json.Unmarshal(doc.Input.Challenge, &c); err != nil {
		t.Fatalf("unmarshal Challenge: %v", err)
	}
	var h Hello
	if err := json.Unmarshal(doc.Input.Hello, &h); err != nil {
		t.Fatalf("unmarshal Hello: %v", err)
	}
	var wantAck HelloAck
	if err := json.Unmarshal(doc.Expected.HelloAck, &wantAck); err != nil {
		t.Fatalf("unmarshal expected HelloAck: %v", err)
	}

	// The app peer's enrollment-learned key for this relay ...
	relayPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	// ... but the hello is signed with an UNRELATED key.
	_, wrongPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	sig, err := SignChannelBinding(wrongPriv, c.Body.Nonce)
	if err != nil {
		t.Fatalf("SignChannelBinding: %v", err)
	}
	h.Body.ChannelBindingSignature = sig

	_, err = BuildHelloAck(
		h,
		c.Body.Nonce,
		relayPub,
		doc.Input.AppPeerImplementedMinors,
		[]string{"telemetry.latest_only_v1"},
		wantAck.Body.SiteBinding,
		wantAck.Body.Deprecated,
	)
	if !errors.Is(err, ErrChannelBindingInvalid) {
		t.Fatalf("BuildHelloAck(wrong-key signature) = %v, want ErrChannelBindingInvalid — the connection MUST be refused even though mutual-TLS is assumed to have already succeeded (REL-032)", err)
	}
}

// TestREL030EndToEndRefusesAbsentChannelBindingSignature is a second
// negative counterpart: the same corpus-derived hello with
// channel_binding_signature simply blanked out, as a relay that never
// signed at all would send. REL-032 requires refusal here exactly as for a
// wrong signature — an absent signature is not treated as "unauthenticated
// but otherwise fine".
func TestREL030EndToEndRefusesAbsentChannelBindingSignature(t *testing.T) {
	doc := loadRel030(t)

	var h Hello
	if err := json.Unmarshal(doc.Input.Hello, &h); err != nil {
		t.Fatalf("unmarshal Hello: %v", err)
	}
	var c Challenge
	if err := json.Unmarshal(doc.Input.Challenge, &c); err != nil {
		t.Fatalf("unmarshal Challenge: %v", err)
	}
	var wantAck HelloAck
	if err := json.Unmarshal(doc.Expected.HelloAck, &wantAck); err != nil {
		t.Fatalf("unmarshal expected HelloAck: %v", err)
	}

	relayPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	h.Body.ChannelBindingSignature = ""

	_, err = BuildHelloAck(
		h,
		c.Body.Nonce,
		relayPub,
		doc.Input.AppPeerImplementedMinors,
		[]string{"telemetry.latest_only_v1"},
		wantAck.Body.SiteBinding,
		wantAck.Body.Deprecated,
	)
	if !errors.Is(err, ErrChannelBindingInvalid) {
		t.Fatalf("BuildHelloAck(absent signature) = %v, want ErrChannelBindingInvalid", err)
	}
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
