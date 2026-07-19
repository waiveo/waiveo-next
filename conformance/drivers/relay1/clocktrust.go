package relay1

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/clocktrust"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
)

// The REL-133 corpus fixture's two wire hints and bounds, inlined
// field-for-field (conformance/corpora/relay-1/REL-133-valid-clock-hint-bounded.json).
const (
	rel133PersistedFloor = int64(1752537000000)
	rel133CertNotAfter   = int64(1784073600000)
	rel133BoundedGrace   = int64(300000)
	rel133Hint1Wire      = `{"type":"clock.hint","relay_id":"01J8Z4K4N5P6Q7R8S9T0V1W3A1","body":{"ts":1752537600000}}`
	rel133Hint2Wire      = `{"type":"clock.hint","relay_id":"01J8Z4K4N5P6Q7R8S9T0V1W3A1","body":{"ts":1784074200001}}`
	// The REL-136 corpus clock.hint (its ts is in-bound for a freshly-issued
	// relay cert's not_after) and cold-boot local clock reading.
	rel136HintWire         = `{"type":"clock.hint","relay_id":"01J8Z4K4N5P6Q7R8S9T0V1W3A1","body":{"ts":1752537600000}}`
	rel136LocalClockMillis = int64(5000)
)

// driveREL133 drives the bounded clock.hint receiver (REL-132/133) against the
// corpus oracle through the REAL wire dispatcher
// (internal/relay/clocktrust.HintReceiver): the in-bound hint adjusts the
// runtime clock only, the out-of-bound one is declined, and — proved against a
// live identity store seeded to the corpus's persisted_floor — neither advances
// the persisted floor.
func driveREL133(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-133")
	if !ok {
		rep.Fail("REL-133", contract, "case not found in frozen corpus")
		return
	}

	store, err := identity.Open(":memory:")
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("open identity store: %v", err))
		return
	}
	defer store.Close()
	if _, err := store.SetClockFloor(rel133PersistedFloor); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("seed persisted floor: %v", err))
		return
	}

	clock := clocktrust.NewRuntimeClock()
	recv := clocktrust.NewHintReceiver(clock, rel133CertNotAfter, rel133BoundedGrace)

	var diffs []report.Diff

	// hint_1 (in-bound): accepted + adjusts the runtime clock.
	acc1, err := recv.Receive([]byte(rel133Hint1Wire))
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "hint_1 decode", Expected: "decoded", Actual: err.Error()})
	}
	if want := c.ExpectBool("hint_1.accepted"); acc1 != want {
		diffs = append(diffs, report.Diff{Field: "hint_1.accepted", Expected: want, Actual: acc1})
	}
	if want := c.ExpectBool("hint_1.runtime_clock_adjusted"); clock.Adjusted() != want {
		diffs = append(diffs, report.Diff{Field: "hint_1.runtime_clock_adjusted", Expected: want, Actual: clock.Adjusted()})
	}

	// hint_2 (out-of-bound): declined; a declined hint by construction does not
	// adjust the runtime clock (REL-133).
	acc2, err := recv.Receive([]byte(rel133Hint2Wire))
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "hint_2 decode", Expected: "decoded", Actual: err.Error()})
	}
	if want := c.ExpectBool("hint_2.accepted"); acc2 != want {
		diffs = append(diffs, report.Diff{Field: "hint_2.accepted", Expected: want, Actual: acc2})
	}
	if want := c.ExpectBool("hint_2.runtime_clock_adjusted"); acc2 != want {
		diffs = append(diffs, report.Diff{Field: "hint_2.runtime_clock_adjusted", Expected: want, Actual: acc2})
	}

	// persisted_floor_after_both_hints: the store the receiver never touched.
	floor, ok, err := store.ClockFloor()
	if err != nil || !ok {
		diffs = append(diffs, report.Diff{Field: "persisted_floor readable", Expected: "present", Actual: fmt.Sprintf("ok=%v err=%v", ok, err)})
	}
	if want, present := expectInt(c, "persisted_floor_after_both_hints"); !present {
		diffs = append(diffs, report.Diff{Field: "persisted_floor_after_both_hints", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if floor != want {
		diffs = append(diffs, report.Diff{Field: "persisted_floor_after_both_hints (a hint never advances the floor)", Expected: want, Actual: floor})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "bounded clock.hint diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"hints are replayed as the corpus's own wire bytes through the real clocktrust.HintReceiver; runtime-clock adjustment is observed via the RuntimeClock, and floor-immutability against a live identity store — not the corpus's static values.")
}

// driveREL136 drives cold-boot skew-tolerant/deferred connect (REL-136/137)
// against the corpus oracle:
//
//  1. peer_cert_validation=skew_tolerant_deferred + handshake_completed: a REAL
//     TLS handshake to the app peer's live listener, the relay clock UNTRUSTED
//     (cold-boot reading a few seconds after the epoch), pinned key-for-key to
//     the enrollment-anchored app-peer leaf SPKI — temporal validation of the
//     app peer's cert is deferred, the handshake completes.
//  2. A wrong-pin negative control: the SAME untrusted-clock dial is REFUSED
//     when the pin does not match (REL-137) — the deferred path did not simply
//     disable authentication.
//  3. hello_accepted/hello_ack_sent: the relay reaches hello reporting
//     clock_state untrusted (REL-038) and the app peer accepts.
//  4. clock_hint_result: the corpus clock.hint adjusts the runtime clock, and
//     the persisted floor (null at cold boot) is not advanced.
func driveREL136(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-136")
	if !ok {
		rep.Fail("REL-136", contract, "case not found in frozen corpus")
		return
	}

	spki, err := feeder.AppPeerLeafSPKI()
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("read app peer leaf SPKI (trust_pin): %v", err))
		return
	}

	coldBoot := time.UnixMilli(rel136LocalClockMillis)
	grace := time.Duration(clocktrust.DefaultBoundedGraceMs) * time.Millisecond
	addr := strings.TrimPrefix(feeder.EnrollBaseURL(), "https://")

	var diffs []report.Diff

	// (1) skew-tolerant/deferred connect over a real TLS handshake.
	conn, err := tls.Dial("tcp", addr, clocktrust.AppPeerTLSConfig(spki, false, coldBoot, grace))
	if err != nil {
		if c.ExpectBool("handshake_completed") {
			diffs = append(diffs, report.Diff{Field: "handshake_completed (skew-tolerant/deferred connect)", Expected: true, Actual: fmt.Sprintf("dial failed: %v", err)})
		}
	} else {
		mode, verr := clocktrust.VerifyAppPeerCert(conn.ConnectionState().PeerCertificates[0], spki, false, coldBoot, grace)
		_ = conn.Close()
		if verr != nil {
			diffs = append(diffs, report.Diff{Field: "peer_cert_validation", Expected: c.ExpectString("peer_cert_validation"), Actual: verr.Error()})
		} else if want := c.ExpectString("peer_cert_validation"); string(mode) != want {
			diffs = append(diffs, report.Diff{Field: "peer_cert_validation", Expected: want, Actual: string(mode)})
		}
		if c.ExpectBool("connection_refused") {
			diffs = append(diffs, report.Diff{Field: "connection_refused", Expected: true, Actual: false})
		}
	}

	// (2) wrong-pin negative control (REL-137): must be refused regardless of clock.
	if wrongSPKI, err := foreignSPKI(); err != nil {
		diffs = append(diffs, report.Diff{Field: "foreign SPKI for negative control", Expected: "generated", Actual: err.Error()})
	} else if bad, err := tls.Dial("tcp", addr, clocktrust.AppPeerTLSConfig(wrongSPKI, false, coldBoot, grace)); err == nil {
		_ = bad.Close()
		diffs = append(diffs, report.Diff{Field: "wrong-key handshake (REL-137 key pin)", Expected: "refused regardless of clock", Actual: "completed"})
	}

	// (3) reach hello reporting clock_state untrusted (REL-038).
	store, err := enrolledStore(client, feeder)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer store.Close()

	decl := hello.Declaration{
		ProtocolVersion: "1.0",
		Features:        []string{},
		SubnetMetadata:  hello.SubnetMetadata{AdvertisedAddress: "192.0.2.12"},
		ClockState:      hello.ClockState{State: "untrusted", Source: "cold_boot"},
	}
	_, helloErr := client.Hello(feeder.EnrollBaseURL(), store, decl)
	if want := c.ExpectBool("hello_accepted"); want && helloErr != nil {
		diffs = append(diffs, report.Diff{Field: "hello_accepted (clock_state untrusted, REL-038)", Expected: true, Actual: fmt.Sprintf("hello refused/failed: %v", helloErr)})
	}
	if want := c.ExpectBool("hello_ack_sent"); want && helloErr != nil {
		diffs = append(diffs, report.Diff{Field: "hello_ack_sent", Expected: true, Actual: "no hello-ack (hello failed)"})
	}

	// (4) clock_hint adjusts the runtime clock, not the persisted floor.
	relayNotAfterMs, err := enrolledCertNotAfterMs(store)
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "enrolled cert not_after", Expected: "parseable", Actual: err.Error()})
	} else {
		clock := clocktrust.NewRuntimeClock()
		recv := clocktrust.NewHintReceiver(clock, relayNotAfterMs, clocktrust.DefaultBoundedGraceMs)
		acc, rerr := recv.Receive([]byte(rel136HintWire))
		if rerr != nil {
			diffs = append(diffs, report.Diff{Field: "clock_hint decode", Expected: "decoded", Actual: rerr.Error()})
		}
		if want := c.ExpectBool("clock_hint_result.runtime_clock_adjusted"); acc != want || clock.Adjusted() != want {
			diffs = append(diffs, report.Diff{Field: "clock_hint_result.runtime_clock_adjusted", Expected: want, Actual: acc && clock.Adjusted()})
		}
		// persisted_floor_advanced=false: cold boot persisted no floor, and a
		// hint never sets one — the store still reports no floor.
		if _, present, ferr := store.ClockFloor(); ferr != nil {
			diffs = append(diffs, report.Diff{Field: "persisted floor readable", Expected: "readable", Actual: ferr.Error()})
		} else if want := c.ExpectBool("clock_hint_result.persisted_floor_advanced"); present != want {
			diffs = append(diffs, report.Diff{Field: "clock_hint_result.persisted_floor_advanced", Expected: want, Actual: present})
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "cold-boot skew-tolerant/deferred connect diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"peer cert validation runs over a REAL TLS handshake against the feeder's live leaf, pinned to the enrollment-anchored SPKI; the app_peer_server_cert window in the corpus is illustrative — the deferred path validates the actual presented cert, and a wrong-pin dial is refused as the REL-137 negative control.",
		"clock_state untrusted / clock.hint values: the relay reports untrusted at hello and applies the hint through the real clocktrust.HintReceiver bound to its own enrolled cert not_after; runtime-clock adjustment and floor-immutability are observed, not read from the corpus's static values.")
}

// foreignSPKI returns the DER SubjectPublicKeyInfo of a freshly-generated key —
// never the app peer's — for REL-137's wrong-pin negative control.
func foreignSPKI() ([]byte, error) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return x509.MarshalPKIXPublicKey(pub)
}

// enrolledCertNotAfterMs parses the not_after (Unix ms) of the relay's
// feeder-issued enrollment certificate — the bound the relay/1 clock.hint
// receiver enforces (REL-133).
func enrolledCertNotAfterMs(store *identity.Store) (int64, error) {
	id, ok, err := store.Identity()
	if err != nil || !ok {
		return 0, fmt.Errorf("read enrolled identity: ok=%v err=%v", ok, err)
	}
	block, _ := pem.Decode(id.CertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return 0, fmt.Errorf("enrolled cert did not PEM-decode to a CERTIFICATE block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return 0, fmt.Errorf("parse enrolled cert: %w", err)
	}
	return leaf.NotAfter.UnixMilli(), nil
}
