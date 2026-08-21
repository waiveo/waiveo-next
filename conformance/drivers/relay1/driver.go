// Package relay1 is the executable relay/1 conformance driver: the §10
// differential oracle for the relay/1 contract's enrollment + desired-state
// surface. It replays the first-photon-applicable relay-1 corpus cases
// (conformance/corpora/relay-1) against a LIVE feeder and a pluggable
// RelayClient (the relay implementation under test), and diffs the client's
// actual behavior against each case's own declared `expected` block.
//
// The RelayClient is pluggable ON PURPOSE (§10 "drivers take ANY target"):
// the first-photon target is RealRelayClient (internal/relay/enroll +
// internal/relay/desiredstate), and the teeth meta-test plugs in a
// deliberately-broken client that skips signature verification to prove the
// REL-071 assertion can FAIL. The Feeder is likewise an abstraction over the
// live counterparty, rich enough to stage each case (mint a claim token,
// serve a validly-signed snapshot at a chosen generation, serve an
// impostor-signed one) — the feederBaseURL of the brief, expanded to what an
// oracle actually needs to reproduce a case.
//
// Applicability triage (§10 "no silent caps"): Run DRIVES every case in the
// frozen relay-1 corpus (REL-010 fresh enroll, REL-015 ahead-of-expiry
// renewal, REL-020/022/027
// Expired-certificate re-enrollment, REL-030 hello/negotiate, REL-051
// ahead-of-current since_generation drawing the full snapshot — refused at
// the relay's REL-052 gate, acked with the pull exchange's correlation id
// (REL-054), REL-056
// durable atomic generation-apply swap, REL-057 server-initiated
// state.changed nudge → relay-issued pull, REL-061 offline screen-program serve
// against internal/relay/desiredstate.ServedProgram, REL-070 idempotent
// reapply, REL-071 wrong-peer-key rejection, REL-090/094 telemetry buffer
// overflow / latest-only heartbeat supersession against
// internal/relay/telemetry, REL-110/112/113 device-candidate report +
// command resolve/dispatch against internal/relay/deviceplane, REL-121b's
// two-relay pairing-grant binding against internal/relay/playerserver, REL-123
// revocation enforced at token issuance and verification with the app peer torn
// down, REL-133
// bounded clock.hint, REL-136/137 cold-boot skew-tolerant/deferred connect)
// — the pending set is empty; every corpus case is driven.
package relay1

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"runtime"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// RelayClient is the pluggable relay/1 implementation under test: the two
// relay-side operations the first-photon corpus exercises — enrolling against
// a feeder, and pulling+verifying+applying its signed desired state. A
// different relay build (or a deliberately-broken one) implements the same
// two methods and is driven by the identical oracle.
type RelayClient interface {
	// Name identifies the implementation (e.g. "real-relay").
	Name() string
	// Enroll redeems a claim credential at feederBaseURL and persists the
	// resulting identity + desired-state verification key into store — the
	// HTTP bootstrap (REL-010), the one surface that cannot ride the
	// authenticated connection it exists to bootstrap.
	Enroll(feederBaseURL string, store *identity.Store) error
	// Pull opens one authenticated persistent connection to connectURL,
	// performs one state.pull, and verifies + applies the answered
	// snapshot, returning the applied state or a typed rejection error
	// (leaving last-applied untouched on rejection) and acknowledging the
	// outcome on the wire with a correlated state.ack (REL-050–054/072).
	Pull(connectURL string, store *identity.Store) (desiredstate.Applied, error)
	// Hello performs the relay/1 connection handshake (REL-030–041) against
	// connectURL over the persistent transport — the challenge → hello →
	// hello-ack exchange inside the dial — declaring decl and signing the
	// app peer's exporter-derived challenge nonce with the enrollment
	// identity Enroll persisted into store, returning the app peer's
	// hello-ack as observed on the wire or a typed refusal
	// (*relayconn.Refusal carrying the taxonomy code).
	Hello(connectURL string, store *identity.Store, decl hello.Declaration) (hello.HelloAck, error)
	// ReEnroll drives the Expired-certificate re-enrollment path
	// (REL-020/024) for store's already-persisted identity against
	// feederBaseURL: proving possession of its current certificate's own
	// private key over the app peer's bootstrap challenge (REL-026/027) and,
	// on success, persisting a fresh certificate under the SAME relay_id
	// (REL-014) plus a re-anchored desired_state_verification_key (REL-017).
	// A typed refusal (CERT_EXPIRED_INELIGIBLE / RE_ENROLL_POP_INVALID /
	// RE_ENROLL_RATE_LIMITED) is returned as a *reenroll.ReEnrollError.
	ReEnroll(feederBaseURL string, store *identity.Store) error
	// Renew drives PROACTIVE, ahead-of-expiry certificate renewal
	// (REL-015) for store's already-persisted identity against
	// feederBaseURL — over the bootstrap exchange (the contract's REL-015
	// draft-note's blessed transport until the in-band verb lands),
	// persisting a fresh certificate under the SAME relay_id (REL-014)
	// and, unlike ReEnroll, over the SAME keypair (the player-pinned SPKI
	// survives renewal). Typed refusals surface as *reenroll.ReEnrollError.
	Renew(feederBaseURL string, store *identity.Store) error
}

// Feeder is the LIVE counterparty the driver stages each case against: the
// enrollment endpoint (REL-010), the /relay/v1 persistent-connection server
// desired state now moves over exclusively, plus the ability to stage the
// snapshot that connection's next state.pull answers with — validly
// feeder-signed at a chosen generation (REL-070's gen-42→gen-43 reapply) or
// impostor-signed (REL-071). A concrete Feeder owns the feeder's signing
// identity, so the valid snapshots it serves verify against the exact key a
// relay enrolled against it learned.
type Feeder interface {
	// EnrollBaseURL is the feeder's enrollment base URL (/claim-token,
	// /enroll). The same listener carries /relay/v1, so this is also the
	// persistent-connection URL a driven client dials.
	EnrollBaseURL() string
	// CurrentClaimToken returns the feeder's currently-pending claim token
	// (minting one if none is pending) WITHOUT redeeming it — so the driver
	// knows which token a subsequent enrollment will consume, to later prove
	// its single-use refusal (REL-010).
	CurrentClaimToken() (string, error)
	// StageSnapshot installs the snapshot the connection's next state.pull
	// answers with, at the given generation over the SAME canonical sections
	// (same content, hence same hash, across generations — REL-070's
	// byte-identical reapply). foreignKey=true signs it with a key that is
	// NOT this feeder's own — REL-071's impostor snapshot.
	StageSnapshot(generation int64, foreignKey bool) error
	// StageRevokingSnapshot installs a validly-signed snapshot at generation
	// carrying exactly this `revocation_and_site.revoked` list (REL-066) and
	// this `pairing_grants` array (REL-067) over the same canonical base
	// sections — re-hashed and re-signed, since both ride `hash`/`signature`
	// (REL-053/075). It is what lets a case move a screen INTO and OUT OF the
	// revoked set across generations, which no feeder in this codebase can do
	// on its own: both snapshot builders emit an empty `revoked` unconditionally
	// (internal/feeder/snapshot), so the app peer's authoring of a revocation is
	// staged here rather than driven through a surface that does not yet exist.
	StageRevokingSnapshot(generation int64, revoked []string, grants []wire.PairingGrant) error
	// SetAppPeerReachable takes the app peer down (false) or brings it back
	// (true). Down means down: an already-established persistent connection is
	// dropped and a fresh dial fails, so a relay has nothing to consult. It is
	// what turns "the relay enforces this while disconnected" (REL-122/REL-123)
	// from a claim about code structure into an observation — the relay is
	// probed with no counterparty in existence. Reversible, so a case that
	// stages an outage leaves the feeder as it found it.
	SetAppPeerReachable(reachable bool)
	// LastStateAck returns the most recent wire state.ack the connection
	// server received from relayID (REL-054), and whether one has arrived —
	// the driver's window onto the WIRE acknowledgment a pull's apply
	// outcome produced.
	LastStateAck(relayID string) (wire.Frame, bool)
	// NotifyGenerationAdvance pushes REL-057's state.changed nudge, carrying
	// the currently-staged generation, to every live authenticated
	// connection — the server-initiated push driveREL057 stages.
	NotifyGenerationAdvance()
	// AppPeerLeafSPKI returns the DER-encoded SubjectPublicKeyInfo of the app
	// peer's own TLS leaf certificate — the enrollment-anchored `trust_pin`
	// (REL-011) a relay pins the connection against under REL-136/137. The
	// feeder serves this exact leaf on its TLS listener, so a relay dialing
	// EnrollBaseURL validates the real presented certificate against it.
	AppPeerLeafSPKI() ([]byte, error)
	// IsRevoked reports the feeder issuance record's revocation status for
	// serial under relayID — the driver's window onto what a renewal did
	// (and did NOT do) to the superseded certificate: REL-015 requires it
	// become superseded (REL-021/022) but never thereby revoked (REL-016).
	IsRevoked(relayID, serial string) bool
}

const contract = "relay/1"

// Run replays the first-photon-applicable relay/1 corpus cases against feeder,
// driving client, and returns the differential-oracle Report.
func Run(client RelayClient, feeder Feeder) report.Report {
	rep := report.Report{Driver: "relay1", Target: client.Name()}

	cases, err := corpus.LoadDir(corpusDir())
	if err != nil {
		rep.Fail("corpus", contract, fmt.Sprintf("load relay-1 corpus: %v", err))
		return rep
	}

	driveREL010(&rep, client, feeder, cases)
	driveREL015(&rep, client, feeder, cases)
	driveREL020(&rep, client, feeder, cases)
	driveREL022(&rep, client, feeder, cases)
	driveREL027(&rep, client, feeder, cases)
	driveREL030(&rep, client, feeder, cases)
	driveREL051(&rep, client, feeder, cases)
	driveREL056(&rep, cases)
	driveREL057(&rep, client, feeder, cases)
	driveREL061(&rep, cases)
	driveREL070(&rep, client, feeder, cases)
	driveREL071(&rep, client, feeder, cases)
	driveREL090(&rep, cases)
	driveREL090a(&rep, cases)
	driveREL094(&rep, cases)
	driveREL110(&rep, cases)
	driveREL110b(&rep, cases)
	driveREL121b(&rep, client, feeder, cases)
	driveREL123(&rep, client, feeder, cases)
	driveREL133(&rep, cases)
	driveREL136(&rep, client, feeder, cases)

	return rep
}

// driveREL010 drives fresh enrollment: a relay holding no certificate redeems
// a claim credential and receives a relay_id, certificate, and the app peer's
// desired_state_verification_key; a repeat enrollment is a stable-identity
// no-op; and a second redemption of the SAME claim token is refused.
func driveREL010(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-010")
	if !ok {
		rep.Fail("REL-010", contract, "case not found in frozen corpus")
		return
	}

	store, err := identity.Open(":memory:")
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("open identity store: %v", err))
		return
	}
	defer store.Close()

	// Capture the token the enrollment will consume, so the single-use refusal
	// below can re-present that exact token.
	token, err := feeder.CurrentClaimToken()
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("current claim token: %v", err))
		return
	}

	var diffs []report.Diff

	if err := client.Enroll(feeder.EnrollBaseURL(), store); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("fresh enroll failed: %v", err))
		return
	}

	relayID, ok, err := store.Identity()
	if err != nil || !ok || relayID.RelayID == "" {
		diffs = append(diffs, report.Diff{Field: "relay_id (enroll-ack.body.relay_id)", Expected: "issued+persisted", Actual: fmt.Sprintf("ok=%v id=%q err=%v", ok, relayID.RelayID, err)})
	}
	if len(relayID.CertPEM) == 0 {
		diffs = append(diffs, report.Diff{Field: "cert (enroll-ack.body cert)", Expected: "issued", Actual: "empty"})
	}
	if _, present, err := store.DesiredStateVerificationKey(); err != nil || !present {
		diffs = append(diffs, report.Diff{Field: "desired_state_verification_key", Expected: "present", Actual: fmt.Sprintf("present=%v err=%v", present, err)})
	}

	// relay_id_stable_across_attempts: a repeat enroll is an idempotent no-op
	// leaving the relay_id unchanged (REL-014).
	firstID := relayID.RelayID
	if err := client.Enroll(feeder.EnrollBaseURL(), store); err != nil {
		diffs = append(diffs, report.Diff{Field: "repeat-enroll idempotent", Expected: "no-op success", Actual: err.Error()})
	}
	if again, _, _ := store.Identity(); again.RelayID != firstID {
		diffs = append(diffs, report.Diff{Field: "relay_id_stable_across_attempts", Expected: firstID, Actual: again.RelayID})
	}

	// Second redemption of the SAME claim token is refused (REL-013): the
	// feeder checks the token before the CSR, so an empty CSR still surfaces
	// the CLAIM_TOKEN_INVALID reason.
	wantCode := c.ExpectString("responses.1.code")
	if wantCode == "" {
		wantCode = "CLAIM_TOKEN_INVALID"
	}
	gotCode, err := reEnrollRefusalCode(feeder.EnrollBaseURL(), token)
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "reuse-token probe", Expected: wantCode, Actual: err.Error()})
	} else if gotCode != wantCode {
		diffs = append(diffs, report.Diff{Field: "reuse-token refusal code", Expected: wantCode, Actual: gotCode})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "fresh enrollment diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"relay_id / cert / desired_state_verification_key values are runtime-issued, not the corpus fixtures — asserted present + stable + single-use, not byte-equal.",
		"not_before/not_after (enroll-ack.body): runtime validity window; not asserted byte-equal.")
}

// driveREL030 drives the relay/1 connection handshake (hello/negotiate,
// REL-030–039): after enrolling, the relay fetches the app peer's single-use
// challenge and signs it with its enrollment key as the channel binding
// (REL-032), declares the corpus's protocol_version/features/subnet_metadata/
// clock_state (REL-031), and the app peer verifies the binding, negotiates
// N-1 down to the relay's declared minor (REL-033/034), grants the shared
// feature subset — silently dropping the corpus's unrecognized flag
// (REL-035) — and answers with its own authoritative site_binding (REL-036).
func driveREL030(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-030")
	if !ok {
		rep.Fail("REL-030", contract, "case not found in frozen corpus")
		return
	}

	store, err := enrolledStore(client, feeder)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer store.Close()

	relayIdent, ok, err := store.Identity()
	if err != nil || !ok {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("read enrolled identity before hello: ok=%v err=%v", ok, err))
		return
	}

	var diffs []report.Diff

	// The corpus's own hello.body declaration: protocol_version one minor
	// behind the app peer's current, and a feature set that includes a flag
	// the app peer does not recognize — the exact input REL-035's silent-drop
	// assertion needs.
	decl := hello.Declaration{
		ProtocolVersion: "1.0",
		Features:        []string{"telemetry.latest_only_v1", "an-unrecognized-future-flag"},
		SubnetMetadata:  hello.SubnetMetadata{AdvertisedAddress: "192.0.2.12"},
		ClockState:      hello.ClockState{State: "trusted", Source: "ntp"},
	}

	ack, err := client.Hello(feeder.EnrollBaseURL(), store, decl)
	if err != nil {
		// channel_binding_verified=false / connection_refused=true: the corpus
		// declares the valid path, so any refusal or transport failure here IS
		// the divergence — a *relayconn.Refusal included verbatim.
		rep.Fail(c.CaseID, contract, fmt.Sprintf("hello handshake refused/failed (corpus expects channel_binding_verified=true, connection_refused=false): %v", err))
		return
	}

	if ack.RelayID != relayIdent.RelayID {
		diffs = append(diffs, report.Diff{Field: "hello_ack.relay_id", Expected: relayIdent.RelayID, Actual: ack.RelayID})
	}

	if wantVerified, present := c.Expect("channel_binding_verified"); !present {
		diffs = append(diffs, report.Diff{Field: "channel_binding_verified", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if wantVerified != true {
		diffs = append(diffs, report.Diff{Field: "channel_binding_verified", Expected: wantVerified, Actual: true})
	}

	if wantRefused, present := c.Expect("connection_refused"); !present {
		diffs = append(diffs, report.Diff{Field: "connection_refused", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if wantRefused != false {
		diffs = append(diffs, report.Diff{Field: "connection_refused", Expected: wantRefused, Actual: false})
	}

	wantNegotiated, present := c.Expect("hello_ack.body.negotiated_version")
	if !present {
		diffs = append(diffs, report.Diff{Field: "hello_ack.body.negotiated_version", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if wantNegotiated != ack.Body.NegotiatedVersion {
		diffs = append(diffs, report.Diff{Field: "hello_ack.body.negotiated_version (N-1, REL-033/034)", Expected: wantNegotiated, Actual: ack.Body.NegotiatedVersion})
	}

	wantFeatures, present := expectStringSlice(c, "hello_ack.body.features")
	if !present {
		diffs = append(diffs, report.Diff{Field: "hello_ack.body.features", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if !reflect.DeepEqual(wantFeatures, ack.Body.Features) {
		diffs = append(diffs, report.Diff{Field: "hello_ack.body.features (shared subset, REL-035)", Expected: wantFeatures, Actual: ack.Body.Features})
	}

	// unrecognized_feature_flag_dropped_silently: the relay's declared
	// "an-unrecognized-future-flag" must not survive into the negotiated
	// subset, and the connection must still succeed — a genuinely
	// unrecognized flag is dropped, never treated as a refusal reason.
	if wantDropped, present := c.Expect("unrecognized_feature_flag_dropped_silently"); !present {
		diffs = append(diffs, report.Diff{Field: "unrecognized_feature_flag_dropped_silently", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if wantDropped == true {
		for _, f := range ack.Body.Features {
			if f == "an-unrecognized-future-flag" {
				diffs = append(diffs, report.Diff{Field: "unrecognized_feature_flag_dropped_silently", Expected: true, Actual: "flag present in hello_ack.body.features"})
			}
		}
	}

	wantSite := hello.SiteBinding{
		ScopeNode: c.ExpectString("hello_ack.body.site_binding.scope_node"),
		TZ:        c.ExpectString("hello_ack.body.site_binding.tz"),
	}
	if lat, present := expectFloat(c, "hello_ack.body.site_binding.lat"); present {
		wantSite.Lat = lat
	} else {
		diffs = append(diffs, report.Diff{Field: "hello_ack.body.site_binding.lat", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	}
	if long, present := expectFloat(c, "hello_ack.body.site_binding.long"); present {
		wantSite.Long = long
	} else {
		diffs = append(diffs, report.Diff{Field: "hello_ack.body.site_binding.long", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	}
	if wantSite != ack.Body.SiteBinding {
		diffs = append(diffs, report.Diff{Field: "hello_ack.body.site_binding (authoritative, REL-036)", Expected: wantSite, Actual: ack.Body.SiteBinding})
	}

	if wantDeprecated, present := c.Expect("hello_ack.body.deprecated"); !present {
		diffs = append(diffs, report.Diff{Field: "hello_ack.body.deprecated", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if m, ok := wantDeprecated.(map[string]any); !ok || len(m) != len(ack.Body.Deprecated) {
		diffs = append(diffs, report.Diff{Field: "hello_ack.body.deprecated (REL-039)", Expected: wantDeprecated, Actual: ack.Body.Deprecated})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "hello/negotiate diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"relay_id / channel_binding_signature: runtime-issued (relay_id from REL-010 enrollment, the signature computed fresh over this connection's own single-use nonce) — asserted self-consistent (hello_ack.relay_id echoes the enrolling identity), not byte-equal to the corpus's static fixture values.",
		"challenge.body.nonce: derived per connection from the TLS exporter keying material by the live app peer (hello.ExporterChallengeNonce, REL-030/040); not the corpus's static nonce.",
	)
}

// driveREL070 drives idempotent generation reapply: after applying generation
// 42, a later snapshot at generation 43 with byte-identical sections (hence
// the same hash) must be a no-op — the applied generation advances but the
// persisted last-applied hash is unchanged and nothing re-runs.
func driveREL070(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-070")
	if !ok {
		rep.Fail("REL-070", contract, "case not found in frozen corpus")
		return
	}

	store, err := enrolledStore(client, feeder)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer store.Close()

	var diffs []report.Diff

	if err := feeder.StageSnapshot(42, false); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("stage gen-42 snapshot: %v", err))
		return
	}
	if _, err := client.Pull(feeder.EnrollBaseURL(), store); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("apply gen-42 snapshot: %v", err))
		return
	}
	gen1, hash1, _, _ := store.LastAppliedGeneration()
	if gen1 != 42 {
		diffs = append(diffs, report.Diff{Field: "last_applied after gen-42", Expected: int64(42), Actual: gen1})
	}

	if err := feeder.StageSnapshot(43, false); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("stage gen-43 snapshot: %v", err))
		return
	}
	if _, err := client.Pull(feeder.EnrollBaseURL(), store); err != nil {
		diffs = append(diffs, report.Diff{Field: "reapply gen-43 (treated_as_noop)", Expected: "no-op success", Actual: err.Error()})
	}
	gen2, hash2, _, _ := store.LastAppliedGeneration()

	// state_ack.body.applied_generation == 43, asserted against the WIRE
	// state.ack the app peer recorded (REL-054) — the pull's acknowledgment
	// as it actually crossed the connection, not a local field read. The
	// corpus case declares this field, so its absence is itself a failure —
	// not a silently-skipped assertion.
	relayIdent, identOK, identErr := store.Identity()
	if identErr != nil || !identOK {
		diffs = append(diffs, report.Diff{Field: "enrolled identity for wire-ack lookup", Expected: "present", Actual: fmt.Sprintf("ok=%v err=%v", identOK, identErr)})
	}
	if wantGen, present := expectInt(c, "state_ack.body.applied_generation"); !present {
		diffs = append(diffs, report.Diff{Field: "state_ack.body.applied_generation", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else {
		if gen2 != wantGen {
			diffs = append(diffs, report.Diff{Field: "persisted last_applied generation", Expected: wantGen, Actual: gen2})
		}
		if observed, ok := awaitWireAck(feeder, relayIdent.RelayID, func(body wire.StateAckBody) bool {
			return body.AppliedGeneration == wantGen && body.Error == nil
		}); !ok {
			diffs = append(diffs, report.Diff{Field: "state_ack.body.applied_generation (wire, REL-054)", Expected: wantGen, Actual: observed})
		}
	}
	// persisted_last_applied_hash_unchanged: the hash must not have moved even
	// though the generation number advanced.
	if hash2 != hash1 {
		diffs = append(diffs, report.Diff{Field: "persisted_last_applied_hash_unchanged", Expected: hash1, Actual: hash2})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "idempotent reapply diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"Observed here: the generation advances to 43, the WIRE state.ack reports it (REL-054), and the persisted hash does NOT move. "+
			"NOT observed here: apply_time_side_effects_rerun=false / in_flight_run_canceled=false. This driver exercises the "+
			"verify-and-persist path (desiredstate.VerifyAndApply), which is declarative and re-runs nothing by construction; the "+
			"apply-time effects REL-070 is about — cancelling in-flight schedule-resolve loops, re-installing served programs, "+
			"replacing pairing grants, reloading edge rules — live in the relay's live-pull seam (cmd/waiveo-relay/livepull.go) and "+
			"are not reachable from here. That seam fences on the section hash exactly as REL-070 states, including the "+
			"higher-generation/identical-hash case the requirement calls out; it is pinned by "+
			"TestRePullSameHashHigherGenerationIsANoOp and its apply-side control in cmd/waiveo-relay. "+
			"An earlier version of this rationale called those two fields VACUOUSLY satisfied because the apply path had no side "+
			"effects when it was written. That stopped being true as the seam grew, and the note outlived it — which is why it now "+
			"names the boundary instead of claiming the question away.")
}

// driveREL071 drives the wrong-peer-key security gate: a snapshot at
// generation 43 signed under a key that is NOT the relay's enrollment-anchored
// verification key MUST be rejected outright — no section applied, last-applied
// stays at 42.
func driveREL071(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-071")
	if !ok {
		rep.Fail("REL-071", contract, "case not found in frozen corpus")
		return
	}

	store, err := enrolledStore(client, feeder)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer store.Close()

	var diffs []report.Diff

	if err := feeder.StageSnapshot(42, false); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("stage gen-42 snapshot: %v", err))
		return
	}
	if _, err := client.Pull(feeder.EnrollBaseURL(), store); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("apply gen-42 snapshot: %v", err))
		return
	}
	genBefore, hashBefore, _, _ := store.LastAppliedGeneration()

	// The impostor snapshot at gen 43 signed with a foreign key.
	if err := feeder.StageSnapshot(43, true); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("stage wrong-key snapshot: %v", err))
		return
	}
	_, pullErr := client.Pull(feeder.EnrollBaseURL(), store)

	// signature_verifies=false: Pull must reject (a real client returns
	// ErrSnapshotSignatureInvalid; any non-nil rejection here is the
	// observable "did not accept").
	if pullErr == nil {
		diffs = append(diffs, report.Diff{Field: "signature_verifies (must reject)", Expected: false, Actual: "accepted"})
	}

	// state_ack.body: the WIRE acknowledgment for the rejected snapshot
	// (REL-054/072) — error carrying the taxonomy code, applied_generation
	// the UNADVANCED prior generation, as the app peer recorded it.
	if relayIdent, identOK, identErr := store.Identity(); identErr != nil || !identOK {
		diffs = append(diffs, report.Diff{Field: "enrolled identity for wire-ack lookup", Expected: "present", Actual: fmt.Sprintf("ok=%v err=%v", identOK, identErr)})
	} else {
		wantCode := c.ExpectString("state_ack.body.error.code")
		wantAckGen, genPresent := expectInt(c, "state_ack.body.applied_generation")
		if wantCode == "" || !genPresent {
			diffs = append(diffs, report.Diff{Field: "state_ack.body (error.code + applied_generation)", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
		} else if observed, ok := awaitWireAck(feeder, relayIdent.RelayID, func(body wire.StateAckBody) bool {
			return body.Error != nil && body.Error.Code == wantCode && body.AppliedGeneration == wantAckGen
		}); !ok {
			diffs = append(diffs, report.Diff{Field: "state_ack.body (wire, REL-054/072)", Expected: fmt.Sprintf("error.code=%s applied_generation=%d", wantCode, wantAckGen), Actual: observed})
		}
	}
	// sections_applied=false + persisted_last_applied_unchanged: last-applied
	// must still be exactly {42, hashBefore}.
	genAfter, hashAfter, _, _ := store.LastAppliedGeneration()
	// persisted_last_applied_unchanged.generation is declared by the corpus
	// case, so its absence is itself a failure — not a silently-skipped
	// assertion.
	if wantGen, present := expectInt(c, "persisted_last_applied_unchanged.generation"); !present {
		diffs = append(diffs, report.Diff{Field: "persisted_last_applied_unchanged.generation (sections_applied=false)", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if genAfter != wantGen {
		diffs = append(diffs, report.Diff{Field: "persisted_last_applied_unchanged.generation (sections_applied=false)", Expected: wantGen, Actual: genAfter})
	}
	if genAfter != genBefore || hashAfter != hashBefore {
		diffs = append(diffs, report.Diff{Field: "last_applied unchanged", Expected: fmt.Sprintf("{%d,%s}", genBefore, hashBefore), Actual: fmt.Sprintf("{%d,%s}", genAfter, hashAfter)})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "wrong-peer-key rejection diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		fmt.Sprintf("the rejection is typed both locally (ErrSnapshotSignatureInvalid, errors.Is-matched: %v) and on the wire (the state.ack error envelope diffed above).", errors.Is(pullErr, desiredstate.ErrSnapshotSignatureInvalid)))
}

// enrolledStore opens a fresh in-memory identity store and enrolls it against
// the feeder — the precondition for any desired-state pull (the store must
// hold the enrollment-anchored verification key).
func enrolledStore(client RelayClient, feeder Feeder) (*identity.Store, error) {
	return enrolledStoreAt(client, feeder, ":memory:")
}

// enrolledStoreAt is enrolledStore over a caller-chosen store path — what a
// stage that RESTARTS the relay needs, since a restart carries nothing but the
// on-disk file, and an in-memory store carries nothing across one at all.
func enrolledStoreAt(client RelayClient, feeder Feeder, path string) (*identity.Store, error) {
	store, err := identity.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open identity store: %w", err)
	}
	if err := client.Enroll(feeder.EnrollBaseURL(), store); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("enroll before pull: %w", err)
	}
	return store, nil
}

// reEnrollRefusalCode re-presents an already-redeemed claim token to the
// feeder's /enroll and returns the RFC-9457 problem `code` it refuses with.
// An empty CSR is deliberate: the feeder validates the (already-redeemed)
// token before ever parsing the CSR, so this surfaces the token reason.
func reEnrollRefusalCode(enrollBaseURL, token string) (string, error) {
	body, _ := json.Marshal(map[string]string{"claim_token": token, "csr": ""})
	resp, err := insecureClient().Post(enrollBaseURL+"/enroll", "application/json", bytesReader(body))
	if err != nil {
		return "", fmt.Errorf("POST /enroll: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "", fmt.Errorf("reuse of a redeemed claim token was ACCEPTED (status %d) — single-use not enforced", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var pb struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &pb) != nil || pb.Code == "" {
		return "", fmt.Errorf("refusal body carried no problem code: %s", string(raw))
	}
	return pb.Code, nil
}

// expectInt reads an integer expected field (JSON numbers decode as
// float64), also reporting whether the field was PRESENT — a case that
// declares the field must have it asserted, never silently skipped just
// because it happens to be missing from the corpus fixture.
func expectInt(c corpus.Case, path string) (n int64, present bool) {
	v, ok := c.Expect(path)
	if !ok {
		return 0, false
	}
	switch v := v.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}

// expectStringSlice reads a []string expected field (JSON arrays decode as
// []any of strings), also reporting whether the field was PRESENT — mirrors
// expectInt's presence-checking discipline for the hello-ack feature list.
func expectStringSlice(c corpus.Case, path string) (out []string, present bool) {
	v, ok := c.Expect(path)
	if !ok {
		return nil, false
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out = make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// expectFloat reads a float64 expected field, also reporting whether it was
// PRESENT — mirrors expectInt for the hello-ack site_binding's lat/long.
func expectFloat(c corpus.Case, path string) (f float64, present bool) {
	v, ok := c.Expect(path)
	if !ok {
		return 0, false
	}
	f, ok = v.(float64)
	return f, ok
}

func corpusDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "corpora", "relay-1")
}

// LoadCorpus loads every frozen relay-1 corpus case, keyed by case_id — the
// exact set Run itself reads. It is exported so the driver's own tests can
// independently verify every case_id present in the corpus DIRECTORY is
// accounted for by Run as either driven or pending (§10 "no silent caps"
// extended to a NEW case someone freezes later: it must be triaged, not
// silently uncovered).
func LoadCorpus() (map[string]corpus.Case, error) {
	return corpus.LoadDir(corpusDir())
}
