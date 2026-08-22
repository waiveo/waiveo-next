package wire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// screenstatus_contract_test.go gives REL-119/119a/119b teeth.
//
// The frame shipped before relay/1 said anything about it — the file's own
// header noted that screen liveness had "no contract clause yet" and named
// itself the shape to reconcile. The clause now exists, and these are the rules
// it states that nothing was checking.
//
// They are asserted at the WIRE level because that is where each of them is a
// property of the bytes rather than of a component: whether an empty view
// survives encoding, whether a never-observed age is distinguishable from a
// recent one, and what an OLDER relay's report decodes to on a newer app peer.

// TestAnEmptyScreenViewSurvivesEncodingAsAnArray pins REL-119's empty-array
// rule at the boundary the rule is about.
//
// The empty report is the one that CLEARS the app peer's view of a relay that
// no longer knows of any screen. Encoded as `null` it stops being that: the app
// peer decodes a nil slice and cannot tell "this relay has forgotten its
// screens" from "this relay said nothing about screens", so a console keeps
// showing screens that are gone.
func TestAnEmptyScreenViewSurvivesEncodingAsAnArray(t *testing.T) {
	raw, err := json.Marshal(ScreenStatusBody{Screens: []ScreenStatusEntry{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"screens":[]`) {
		t.Errorf("an empty view encoded as %s, want `\"screens\":[]` — an empty array is the value that clears a stale view (REL-119)", raw)
	}

	// And the failure mode stated from the other side: a nil slice does NOT
	// encode as an array, which is precisely why the producer normalizes before
	// building the frame rather than trusting its caller.
	nilRaw, err := json.Marshal(ScreenStatusBody{})
	if err != nil {
		t.Fatalf("marshal nil: %v", err)
	}
	if !strings.Contains(string(nilRaw), `"screens":null`) {
		t.Fatalf("a nil Screens encoded as %s; this test's premise — that nil and empty differ on the wire, so normalizing is load-bearing — no longer holds and REL-119's producer rule needs re-reading", nilRaw)
	}
}

// TestNeverObservedIsDistinguishableFromJustNow pins REL-119a's sentinel.
//
// Zero is a real answer — "just now" — so it cannot also mean "never". A
// consumer that treated the sentinel as an ordinary age would rank a screen
// that has NEVER pulled as the most recently active one on the page, which is
// the exact inversion an operator would act on first.
func TestNeverObservedIsDistinguishableFromJustNow(t *testing.T) {
	var never, justNow ScreenStatusEntry
	if err := json.Unmarshal([]byte(`{"screen_id":"a","last_pull_age_ms":-1}`), &never); err != nil {
		t.Fatalf("unmarshal never: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"screen_id":"b","last_pull_age_ms":0}`), &justNow); err != nil {
		t.Fatalf("unmarshal just-now: %v", err)
	}
	if never.LastPullAgeMs != -1 {
		t.Errorf("never-observed decoded as %d, want -1 (REL-119a)", never.LastPullAgeMs)
	}
	if justNow.LastPullAgeMs != 0 {
		t.Errorf("just-now decoded as %d, want 0 (REL-119a)", justNow.LastPullAgeMs)
	}
	if never.LastPullAgeMs == justNow.LastPullAgeMs {
		t.Error("never-observed and just-now decoded to the same value — the two states an operator must act on differently are indistinguishable")
	}
}

// TestAnOlderRelaysReportMakesNoRefusalClaim pins REL-119b's compatibility
// rule, which is the reason `rejected` is a boolean rather than an age.
//
// A relay is expected to be upgraded at or before its app peer, so an app peer
// decoding a report from a relay that predates this member is the ORDINARY case
// (REL-004). An absent numeric would decode to 0, and 0 as an age reads as
// "refused just now" — every screen behind a pre-upgrade relay would appear to
// be refusing its program, on a fleet where nothing is wrong. An absent boolean
// decodes to false: no claim, which is the only honest reading.
func TestAnOlderRelaysReportMakesNoRefusalClaim(t *testing.T) {
	var entry ScreenStatusEntry
	// A body from a relay that has never heard of these members at all.
	if err := json.Unmarshal([]byte(`{"screen_id":"a","paired":true,"last_pull_age_ms":1200}`), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if entry.Rejected {
		t.Error("a report omitting `rejected` decoded as a refusal — every screen behind a pre-upgrade relay would read as refusing its program (REL-119b)")
	}
	if entry.RejectReason != "" || entry.RejectedProgramRevision != "" {
		t.Errorf("a report omitting the refusal members invented one: reason=%q revision=%q", entry.RejectReason, entry.RejectedProgramRevision)
	}

	// The decode above is nearly tautological on a bool — Go's zero value does
	// the work — so it is documentary, not defensive. The member's TYPE is the
	// real requirement, and the honest note is that the COMPILER is its primary
	// guard: changing `Rejected` to a numeric fails to build, because producers
	// assign a bool to it. A mutation proves that rather than proving this test.
	//
	// What this check adds is the case the compiler waves through — someone
	// changing the type DELIBERATELY and updating every producer with it. That
	// compiles, and it is exactly the change REL-119b forbids: an absent numeric
	// decodes to 0, which as an age reads "refused just now" on every screen
	// behind a relay that predates the member.
	field, ok := reflect.TypeOf(ScreenStatusEntry{}).FieldByName("Rejected")
	if !ok {
		t.Fatal("ScreenStatusEntry has no Rejected member")
	}
	if field.Type.Kind() != reflect.Bool {
		t.Errorf("`rejected` is %s, want bool — a numeric absent value decodes to 0, and 0 as an age is a refusal claim no relay made (REL-119b)", field.Type.Kind())
	}
}

// TestIntentAndConfirmationAreSeparateMembers pins the shape REL-119b's
// separation depends on.
//
// The rule is about how a CONSUMER reads them, which a wire test cannot
// enforce. What it can enforce is that the two remain distinct members, so a
// future change cannot quietly collapse them into one and leave every consumer
// correct-by-accident until a player refuses something.
func TestIntentAndConfirmationAreSeparateMembers(t *testing.T) {
	raw := `{"screen_id":"a",` +
		`"program_revision":"rev-handed","content_count":3,` +
		`"acked_program_revision":"rev-accepted","acked_content_count":1}`
	var entry ScreenStatusEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if entry.ProgramRevision == entry.AckedProgramRevision {
		t.Fatal("intent and acknowledged revisions decoded to one value — a console could not tell what a wall was HANDED from what it ACCEPTED")
	}
	if entry.ProgramRevision != "rev-handed" || entry.AckedProgramRevision != "rev-accepted" {
		t.Errorf("intent/ack mismatch: handed=%q accepted=%q", entry.ProgramRevision, entry.AckedProgramRevision)
	}
	if entry.ContentCount == entry.AckedContentCount {
		t.Error("intent and acknowledged content counts decoded to one value")
	}
}
