package eval

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/model"
)

// signage_test.go covers RunActions' dispatch of the three RUL-234/235 signage
// actions: that each reaches the sink with the members the author declared, that
// a malformed one performs no work AND is reported as failed rather than
// silently skipped, and that the outcome is recorded in RUL-236's shape.

// recordingSignage is a SignageSink that records what it was asked to do and
// answers with a scripted per-screen result set.
type recordingSignage struct {
	calls   []signageCall
	screens []ScreenResult
}

type signageCall struct {
	action  string
	ref     ScreenRef
	castID  string
	message string
	ttl     int
}

func (r *recordingSignage) PlayCast(ref ScreenRef, castID string) SignageOutcome {
	r.calls = append(r.calls, signageCall{action: "play_cast", ref: ref, castID: castID})
	return SignageOutcomeFor("play_cast", r.screens)
}

func (r *recordingSignage) ShowAlert(ref ScreenRef, castID, message string, ttl int) SignageOutcome {
	r.calls = append(r.calls, signageCall{action: "show_alert", ref: ref, castID: castID, message: message, ttl: ttl})
	return SignageOutcomeFor("show_alert", r.screens)
}

func (r *recordingSignage) DismissAlert(ref ScreenRef) SignageOutcome {
	r.calls = append(r.calls, signageCall{action: "dismiss_alert", ref: ref})
	return SignageOutcomeFor("dismiss_alert", r.screens)
}

// signageAction decodes one action literal into the model.Member RunActions takes.
func signageAction(t *testing.T, raw string) model.Member {
	t.Helper()
	r, err := model.ParseRule([]byte(`{"id":"r","mode":"single","triggers":[],"conditions":[],"actions":[` + raw + `]}`))
	if err != nil {
		t.Fatalf("parse action %s: %v", raw, err)
	}
	return r.Actions[0]
}

func TestSignageActionsReachTheSink(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want signageCall
	}{
		{
			"play_cast by screen_id",
			`{"type":"play_cast","screen_id":"SCR1","cast_id":"CAST1"}`,
			signageCall{action: "play_cast", ref: ScreenRef{ScreenID: "SCR1"}, castID: "CAST1"},
		},
		{
			// A selector on a SIGNAGE action selects screens, not entities —
			// which is why it is decoded into a ScreenRef rather than reused
			// from the member's EntityRef.
			"play_cast by selector",
			`{"type":"play_cast","selector":"zone=lobby","cast_id":"CAST1"}`,
			signageCall{action: "play_cast", ref: ScreenRef{Selector: "zone=lobby"}, castID: "CAST1"},
		},
		{
			"show_alert with a literal message and a ttl",
			`{"type":"show_alert","screen_id":"SCR1","message":"Kitchen closed","ttl_seconds":90}`,
			signageCall{action: "show_alert", ref: ScreenRef{ScreenID: "SCR1"}, message: "Kitchen closed", ttl: 90},
		},
		{
			"show_alert naming a cast",
			`{"type":"show_alert","screen_id":"SCR1","cast_id":"CAST1"}`,
			signageCall{action: "show_alert", ref: ScreenRef{ScreenID: "SCR1"}, castID: "CAST1"},
		},
		{
			"dismiss_alert",
			`{"type":"dismiss_alert","selector":"zone=lobby"}`,
			signageCall{action: "dismiss_alert", ref: ScreenRef{Selector: "zone=lobby"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSignage{screens: []ScreenResult{{ScreenID: "SCR1", OK: true}}}
			var outcomes []SignageOutcome
			ctx := ActionContext{Signage: sink, SignageOutcomes: &outcomes}
			if err := RunActions(ctx, []model.Member{signageAction(t, tc.raw)}); err != nil {
				t.Fatalf("RunActions: %v", err)
			}
			if len(sink.calls) != 1 || sink.calls[0] != tc.want {
				t.Fatalf("sink saw %+v, want %+v", sink.calls, tc.want)
			}
			if len(outcomes) != 1 || outcomes[0].Action != tc.want.action || outcomes[0].Outcome != "complete" {
				t.Fatalf("recorded outcome = %+v, want one complete %s", outcomes, tc.want.action)
			}
		})
	}
}

// TestMalformedSignageActionPerformsNoWorkAndSaysSo: an action whose own
// required members do not hold is not dispatched with a guessed value — AND it
// is reported as failed rather than skipped silently.
//
// Both halves are load-bearing and only the first used to hold. Each of these
// shapes is refused by the compile gate (compile.validateSignageContent), so
// reaching the evaluator means something upstream is already wrong: a row
// written before that gate existed, a hand-edited store. The fail-closed answer
// is to do nothing, never to pick one of the two things the author asked for —
// but doing nothing SILENTLY is what made this a defect. The run answered
// `ran` with an empty `signage` report and an unchanged screen, which is
// indistinguishable from a run that worked, and left an operator with no
// diagnostic anywhere in the system.
func TestMalformedSignageActionPerformsNoWorkAndSaysSo(t *testing.T) {
	cases := []struct {
		raw    string
		action string
	}{
		{`{"type":"play_cast","screen_id":"SCR1"}`, "play_cast"},                                  // RUL-234: no cast_id
		{`{"type":"show_alert","screen_id":"SCR1"}`, "show_alert"},                                // RUL-235: neither cast_id nor message
		{`{"type":"show_alert","screen_id":"SCR1","cast_id":"C","message":"both"}`, "show_alert"}, // RUL-235: both
	}
	for _, tc := range cases {
		sink := &recordingSignage{}
		var outcomes []SignageOutcome
		ctx := ActionContext{Signage: sink, SignageOutcomes: &outcomes}
		if err := RunActions(ctx, []model.Member{signageAction(t, tc.raw)}); err != nil {
			t.Fatalf("RunActions: %v", err)
		}
		if len(sink.calls) != 0 {
			t.Fatalf("%s reached the sink as %+v; a malformed signage action must perform no work", tc.raw, sink.calls)
		}
		if len(outcomes) != 1 {
			t.Fatalf("%s recorded %d outcome(s), want exactly 1 — a silent skip reports the run as having done nothing wrong", tc.raw, len(outcomes))
		}
		got := outcomes[0]
		if got.Action != tc.action || got.Outcome != "failed" {
			t.Errorf("%s recorded %+v, want a failed %s", tc.raw, got, tc.action)
		}
		if got.Error == "" {
			t.Errorf("%s recorded no reason; an operator reading the report has nothing to act on", tc.raw)
		}
		if len(got.Screens) != 0 {
			t.Errorf("%s reported screens %+v; no screen was attempted, and a fabricated entry would carry an empty screen_id", tc.raw, got.Screens)
		}
	}
}

// TestSignageWithNoSinkIsANoOp: every EDGE evaluation context leaves Signage
// nil, because these actions are app-class and the relay's engine never loads a
// rule carrying one. Reaching one there must be a silent skip, exactly as
// notify/variable_write already are — not a panic and not an error that would
// halt the rest of an unrelated action sequence.
func TestSignageWithNoSinkIsANoOp(t *testing.T) {
	var logs []LogEntry
	ctx := ActionContext{Logs: &logs}
	actions := []model.Member{
		signageAction(t, `{"type":"play_cast","screen_id":"SCR1","cast_id":"CAST1"}`),
		signageAction(t, `{"type":"log","message":"after"}`),
	}
	if err := RunActions(ctx, actions); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(logs) != 1 || logs[0].Message != "after" {
		t.Fatalf("logs = %+v; the action after a skipped signage action must still run", logs)
	}
}

// TestSignageOutcomeForReusesThePresetBatchClassification pins RUL-236's reuse
// of RUL-172's three-value outcome. An EMPTY screen list is `complete`, not
// `failed`: RUL-233 makes a selector matching no screen a legal no-op, and
// reporting it as a failure would make "no lobby screens are placed here" look
// like a refused write.
func TestSignageOutcomeForReusesThePresetBatchClassification(t *testing.T) {
	cases := []struct {
		name    string
		screens []ScreenResult
		want    string
	}{
		{"no targets", nil, "complete"},
		{"all written", []ScreenResult{{ScreenID: "a", OK: true}, {ScreenID: "b", OK: true}}, "complete"},
		{"one refused", []ScreenResult{{ScreenID: "a", OK: true}, {ScreenID: "b", Error: "nope"}}, "partial"},
		{"none written", []ScreenResult{{ScreenID: "a", Error: "nope"}}, "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SignageOutcomeFor("play_cast", tc.screens)
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", got.Outcome, tc.want)
			}
			if len(got.Screens) != len(tc.screens) {
				t.Fatalf("per-screen list = %+v, want the %d input result(s)", got.Screens, len(tc.screens))
			}
		})
	}
}

// TestRejectedRecordsAPreDispatchRefusal: a single-target device_command that
// never reaches the sink (RUL-161 gives it no outcome channel) is visible to a
// caller that supplies ctx.Rejected, and remains invisible to one that does not.
// Both halves matter — the edge engine relies on the silence.
func TestRejectedRecordsAPreDispatchRefusal(t *testing.T) {
	action := signageAction(t, `{"type":"device_command","entity_id":"ENT1","command":"launch"}`)

	var rejected []CommandResult
	// No Sink wired: the dispatch is refused before it could go anywhere.
	if err := RunActions(ActionContext{Rejected: &rejected}, []model.Member{action}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(rejected) != 1 || rejected[0].Target != "ENT1" || rejected[0].Command != "launch" || rejected[0].OK {
		t.Fatalf("rejected = %+v, want one named refusal for ENT1/launch", rejected)
	}

	// Without a recorder the refusal is absorbed, unchanged from before.
	if err := RunActions(ActionContext{}, []model.Member{action}); err != nil {
		t.Fatalf("RunActions with no recorder: %v", err)
	}
}

// TestSignageActionRawIsDecodedIndependentlyOfEntityRef guards a decoding trap:
// model.Member puts a `selector` into EntityRef, so a signage action carrying
// one arrives with an EntityRef set that means something else entirely. The
// dispatch must read the ScreenRef out of the action's own raw JSON.
func TestSignageActionRawIsDecodedIndependentlyOfEntityRef(t *testing.T) {
	m := signageAction(t, `{"type":"play_cast","selector":"zone=lobby","cast_id":"CAST1"}`)
	if m.EntityRef == nil || m.EntityRef.Selector != "zone=lobby" {
		t.Fatalf("fixture: the decoder no longer places a selector on EntityRef (%+v); this test's premise is gone", m.EntityRef)
	}
	var probe struct {
		ScreenID string `json:"screen_id"`
	}
	if err := json.Unmarshal(m.Raw, &probe); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	sink := &recordingSignage{}
	if err := RunActions(ActionContext{Signage: sink}, []model.Member{m}); err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(sink.calls) != 1 || sink.calls[0].ref.Selector != "zone=lobby" || sink.calls[0].ref.ScreenID != "" {
		t.Fatalf("ScreenRef = %+v, want the selector form", sink.calls[0].ref)
	}
}
