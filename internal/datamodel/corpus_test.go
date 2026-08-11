package datamodel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/rules/schedule"
)

// This file is the data-model/1 conformance driver for every requirement this
// package's reference ENGINE can answer: it replays those cases under
// conformance/corpora/data-model-1/*.json and asserts each case's own exact
// expected shape. The scheduling-core cases (cascade, precedence, layering,
// holding, fallback, terminal, preset rising-edge, misfire) exercise
// Resolve/PresetTransition/BatchOutcome; the tree/tz/ownership/overlap cases
// exercise BuildScopeTree/EffectiveGeo/ValidateRows/ValidateNoOverlap. A case
// with no handler fails the run, so the corpus and the engine cannot drift
// apart silently.
//
// # The cases this driver does NOT own
//
// A case whose `input` carries a `request` block is a REQUEST being refused, not
// a row set being evaluated: this engine has no delete operation, no HTTP status
// and no Problem body, so there is nothing here for such a case to run against.
// Those are driven against the mounted handler by
// internal/app/api/datamodel_corpus_test.go — a package this file cannot import
// (it imports this one) and which cannot import this file's handlers either, so
// the split is decided by BOTH sides reading the same property off the same
// frozen data rather than by either holding a list of the other's case ids.
//
// "Carries a `request` block" means present AND non-null on both sides — see
// isRequestShaped. Agreeing on ONE predicate is what the no-double-drive and
// no-drop claims rest on, and the two are pinned against each other by
// TestRequestShapedMatchesTheHTTPDriversDecode.

const corpusDir = "../../conformance/corpora/data-model-1"

// corpusCase is the envelope every corpus file shares; input/expected are held
// raw so each handler decodes only the fields its case defines.
type corpusCase struct {
	CaseID   string          `json:"case_id"`
	Input    json.RawMessage `json:"input"`
	Expected json.RawMessage `json:"expected"`
}

// corpusHandlers maps case_id → its assertion. Every file MUST have an entry.
var corpusHandlers = map[string]func(*testing.T, json.RawMessage, json.RawMessage){
	"DAT-001-valid-scope-node-tree":                                     checkScopeTree001,
	"DAT-004a-valid-screen-and-device-identity-rows":                    checkIdentityRows004a,
	"DAT-005b-invalid-identity-rows-missing-baseline":                   checkIdentityBaseline005b,
	"DAT-010-valid-org-account-states":                                  checkAccountStates010,
	"DAT-033-valid-screen-tz-override":                                  checkEffectiveGeo033,
	"DAT-051-valid-site-schedule-cascades-to-screen":                    checkSingleWinner,
	"DAT-053-valid-equal-priority-same-node-lowest-id-wins":             checkSingleWinner,
	"DAT-053-valid-equal-priority-specificity-nearer-node-wins":         checkSingleWinner,
	"DAT-053-valid-screen-schedule-precedence-over-site":                checkSingleWinner,
	"DAT-071-invalid-daypart-time-out-of-range":                         checkDaypartWriteRefusal071,
	"DAT-071-invalid-daypart-time-24-boundary":                          checkDaypartWriteRefusal071,
	"DAT-071-invalid-daypart-phantom-weekday":                           checkDaypartWriteRefusal071,
	"DAT-071-invalid-daypart-empty-days":                                checkDaypartWriteRefusal071,
	"DAT-073-invalid-within-schedule-daypart-overlap-rejected":          checkOverlapReject073,
	"DAT-073-invalid-midnight-wrap-collides-next-weekday":               checkOverlapReject073,
	"DAT-074-valid-daypart-display-power-off":                           checkDisplayPowerOff074,
	"DAT-075-valid-apply-carries-baseline-until-the-bound-rows-change":  checkCarryAcrossApply075,
	"DAT-075-valid-fall-back-boundary-refires-preset":                   checkPresetFallBack075,
	"DAT-075-valid-masked-preset-fires-on-fall-through":                 checkPresetMasked075,
	"DAT-092-valid-preset-batch-partial-failure":                        checkBatchOutcome092,
	"DAT-101-invalid-pack-owned-scheduler-rejected":                     checkPackOwned101,
	"DAT-111-valid-layered-daypart-holiday-over-base":                   checkLayered111,
	"DAT-117-valid-layered-fallback-through-precedence":                 checkFallback117,
	"DAT-118-valid-terminal-default-blank":                              checkTerminal118,
	"DAT-119-valid-dst-fall-back-window-holds-through-repeat":           checkHoldSequence119,
	"DAT-119-valid-dst-spring-forward-window-begins-first-real-instant": checkSpringForward119,
	"DAT-121-valid-misfire-catchup-vs-skip-by-kind":                     checkMisfire121,
}

// TestDataModel1Corpus replays every engine-shaped data-model/1 case and fails on
// any of them without a handler, so a newly added corpus case cannot pass
// unexercised. Request-shaped cases are skipped here and counted separately —
// see requestShapedCaseIDs and this file's header.
func TestDataModel1Corpus(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.json"))
	if err != nil {
		t.Fatalf("glob corpus: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no corpus files found under %s", corpusDir)
	}
	seen := map[string]bool{}
	engineCases := 0
	for _, f := range files {
		c := loadCase(t, f)
		if isRequestShaped(t, c) {
			if _, ok := corpusHandlers[c.CaseID]; ok {
				t.Errorf("corpus case %q is request-shaped AND has an engine handler — exactly one driver owns a case", c.CaseID)
			}
			continue
		}
		engineCases++
		seen[c.CaseID] = true
		h, ok := corpusHandlers[c.CaseID]
		if !ok {
			t.Errorf("no handler registered for corpus case %q (%s)", c.CaseID, filepath.Base(f))
			continue
		}
		t.Run(c.CaseID, func(t *testing.T) { h(t, c.Input, c.Expected) })
	}
	for id := range corpusHandlers {
		if !seen[id] {
			t.Errorf("handler registered for %q but no corpus file present", id)
		}
	}
	if engineCases != len(corpusHandlers) {
		t.Fatalf("engine-shaped corpus files = %d, handlers = %d — every case must map 1:1", engineCases, len(corpusHandlers))
	}
}

// isRequestShaped reports whether a case's `input` carries a `request` block —
// the single property that decides which driver owns a data-model/1 case. The
// HTTP driver reads the SAME property off the same files (see the header), so
// neither side can claim a case the other is also driving, or neither is.
//
// "Carries a `request` block" is present AND NON-NULL, and the second half is
// load-bearing rather than defensive. The HTTP driver decodes the member into a
// `*struct` and drives the case only when that pointer is non-nil, and
// encoding/json leaves a pointer nil for an explicit JSON null. A key-presence
// test here would therefore call `"request": null` request-shaped while the HTTP
// driver called it absent, and the case would be driven by NEITHER — silently,
// because each side's skip is the other side's job. That is not the failure the
// double-drive guard below catches; it is its mirror, and it was reachable
// before this predicate rejected null. See TestRequestShapedMatchesTheHTTPDrivers
// Decode, which pins the two spellings against each other.
func isRequestShaped(t *testing.T, c corpusCase) bool {
	t.Helper()
	var input map[string]json.RawMessage
	if err := json.Unmarshal(c.Input, &input); err != nil {
		t.Fatalf("decode input of %s: %v", c.CaseID, err)
	}
	raw, ok := input["request"]
	if !ok {
		return false
	}
	return !isJSONNull(t, c.CaseID, raw)
}

// isJSONNull reports whether raw is the JSON literal null, decided the same way
// the HTTP driver decides it: by decoding into a POINTER and asking whether
// encoding/json left it nil. Spelling it as a decode rather than as a byte
// comparison against "null" is what keeps the two sides on one rule — whatever
// encoding/json treats as null for a `*struct` field is what is treated as null
// here, including any leading or trailing whitespace the raw member carries.
func isJSONNull(t *testing.T, caseID string, raw json.RawMessage) bool {
	t.Helper()
	var probe *json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("decode the `request` member of %s: %v", caseID, err)
	}
	return probe == nil
}

// httpDriverOwnsCase decides ownership the way internal/app/api/
// datamodel_corpus_test.go decides it: the `request` member is decoded into a
// POINTER field, and that driver drives the case only when encoding/json leaves
// the pointer non-nil.
//
// It is the other driver's mechanism restated in the only form this package can
// execute — internal/datamodel cannot import internal/app/api, which imports it,
// and the real predicate lives in a _test.go file no package can import at all.
// Restating it is therefore not duplication for its own sake: it is the only way
// a test HERE can compare the two rules, and the api side pins its own half
// against the same written rule (TestHTTPDriverOwnershipIsPresentAndNonNull).
func httpDriverOwnsCase(t *testing.T, caseID string, input json.RawMessage) bool {
	t.Helper()
	var envelope struct {
		Request *struct {
			Method  string            `json:"method"`
			Path    string            `json:"path"`
			Headers map[string]string `json:"headers"`
			Body    json.RawMessage   `json:"body"`
		} `json:"request"`
	}
	if err := json.Unmarshal(input, &envelope); err != nil {
		t.Fatalf("decode input of %s: %v", caseID, err)
	}
	return envelope.Request != nil
}

// TestRequestShapedMatchesTheHTTPDriversDecode pins the two drivers to ONE
// predicate, which is the whole basis of the claim that a case cannot be driven
// twice or not at all.
//
// The double-drive direction is already caught out loud: TestDataModel1Corpus
// errors when a request-shaped case also has an engine handler. The DROP
// direction had no check, and was reachable — a case carrying `"request": null`
// was request-shaped to a key-presence test (so the engine skipped it) and nil to
// the HTTP driver's pointer decode (so it skipped too), while every manifest
// assertion still counted it as driven, because each side counts the OTHER side's
// share from the corpus rather than from that driver. Nothing failed; the case
// simply never ran. This is the check that makes that impossible.
func TestRequestShapedMatchesTheHTTPDriversDecode(t *testing.T) {
	// Every spelling of the member, including the ones no frozen case carries
	// today — a corpus-only check would go green again the moment the offending
	// case were deleted, which is how the gap got in.
	t.Run("every spelling of the request member", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			input string
			want  bool
		}{
			{"absent", `{"scope_nodes":[]}`, false},
			{"explicit null", `{"request":null}`, false},
			{"null with surrounding whitespace", "{\"request\":\n    null\n}", false},
			{"a request object", `{"request":{"method":"DELETE","path":"/api/v1/scope-nodes/x"}}`, true},
			{"an empty request object", `{"request":{}}`, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := isRequestShaped(t, corpusCase{CaseID: tc.name, Input: json.RawMessage(tc.input)})
				if got != tc.want {
					t.Errorf("isRequestShaped(%s) = %v, want %v — a `request` member counts only when it is "+
						"present AND non-null", tc.input, got, tc.want)
				}
				if http := httpDriverOwnsCase(t, tc.name, json.RawMessage(tc.input)); http != got {
					t.Errorf("the two drivers disagree on %s: engine says request-shaped=%v, the HTTP driver's "+
						"pointer decode says owned=%v — one of them would drive this case twice, or neither would "+
						"drive it at all", tc.input, got, http)
				}
			})
		}
	})

	// And on the real data, so a frozen case that manages to split the two is
	// caught even if it is spelled in a way the table above did not anticipate.
	t.Run("every frozen corpus case", func(t *testing.T) {
		files, err := filepath.Glob(filepath.Join(corpusDir, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("glob corpus: %v (%d file(s))", err, len(files))
		}
		for _, f := range files {
			c := loadCase(t, f)
			engine := isRequestShaped(t, c)
			if http := httpDriverOwnsCase(t, c.CaseID, c.Input); http != engine {
				t.Errorf("%s: engine says request-shaped=%v, the HTTP driver says owned=%v — exactly one driver "+
					"must own every case", c.CaseID, engine, http)
			}
		}
	})
}

// requestShapedCaseIDs are the case ids the HTTP driver owns, read off the frozen
// corpus rather than listed — the input to the driven-manifest union in
// driven_manifest_test.go.
func requestShapedCaseIDs(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.json"))
	if err != nil {
		t.Fatalf("glob corpus: %v", err)
	}
	var out []string
	for _, f := range files {
		if c := loadCase(t, f); isRequestShaped(t, c) {
			out = append(out, c.CaseID)
		}
	}
	return out
}

func loadCase(t *testing.T, path string) corpusCase {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var c corpusCase
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return c
}

// --- shared decode + instant helpers -------------------------------------------

// resolveInput is the union of fields the scheduling-core resolution cases carry;
// each handler reads only the subset its case populates.
type resolveInput struct {
	ScopeNodes       []ScopeNode      `json:"scope_nodes"`
	Schedules        []Schedule       `json:"schedules"`
	ValidityWindows  []ValidityWindow `json:"validity_windows"`
	Dayparts         []Daypart        `json:"dayparts"`
	Fallbacks        []Fallback       `json:"fallbacks"`
	PresetBatches    []PresetBatch    `json:"preset_batches"`
	ResolveFor       string           `json:"resolve_for_scope_node"`
	EvaluatedAtLocal string           `json:"evaluated_at_local"`
	EvaluatedAt      *int64           `json:"evaluated_at"`
	Evaluations      []corpusEval     `json:"evaluations"`
}

type corpusEval struct {
	AtUTC   string `json:"at_utc"`
	AtLocal string `json:"at_local"`
	Label   string `json:"label"`
}

func mustInput(t *testing.T, raw json.RawMessage) resolveInput {
	t.Helper()
	var in resolveInput
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("decode input: %v", err)
	}
	return in
}

// storeFromInput builds a RowStore from a resolution case's rows. It tolerates
// BuildScopeTree structural findings (e.g. a case that attaches a screen directly
// under an org to isolate a DST-holding scenario, DAT-119): those cases test
// resolution, not the DAT-003 kind table, and Resolve reports its own errors.
func storeFromInput(in resolveInput) RowStore {
	tree, _ := BuildScopeTree(in.ScopeNodes)
	return RowStore{
		Tree: tree,
		Rows: RowSet{
			Schedules:       in.Schedules,
			ValidityWindows: in.ValidityWindows,
			Dayparts:        in.Dayparts,
			Fallbacks:       in.Fallbacks,
			PresetBatches:   in.PresetBatches,
		},
	}
}

func effTZName(t *testing.T, store RowStore, node string) string {
	t.Helper()
	tz, _, _, err := EffectiveTZ(store.Tree, node)
	if err != nil {
		t.Fatalf("EffectiveTZ(%s): %v", node, err)
	}
	return tz
}

func utcMs(t *testing.T, s string) int64 {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse utc %q: %v", s, err)
	}
	return tm.UnixMilli()
}

func localMs(t *testing.T, tz, s string) int64 {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load %q: %v", tz, err)
	}
	tm, err := time.ParseInLocation("2006-01-02T15:04:05", s, loc)
	if err != nil {
		t.Fatalf("parse local %q: %v", s, err)
	}
	return tm.UnixMilli()
}

// evalMs converts one evaluation to an absolute instant: at_utc directly, else
// at_local in the resolve node's effective tz (the same tz the engine resolves).
func evalMs(t *testing.T, store RowStore, node string, e corpusEval) int64 {
	t.Helper()
	if e.AtUTC != "" {
		return utcMs(t, e.AtUTC)
	}
	return localMs(t, effTZName(t, store, node), e.AtLocal)
}

// singleMs converts a single-instant case's clock to an absolute instant.
func singleMs(t *testing.T, store RowStore, in resolveInput) int64 {
	t.Helper()
	if in.EvaluatedAt != nil {
		return *in.EvaluatedAt
	}
	return localMs(t, effTZName(t, store, in.ResolveFor), in.EvaluatedAtLocal)
}

func daypartID(st EffectiveState) string {
	if st.Daypart == nil {
		return ""
	}
	return st.Daypart.ID
}

func strp(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func assertLease(t *testing.T, st EffectiveState, display, contentSource string) {
	t.Helper()
	if st.Display != display {
		t.Fatalf("display = %q, want %q", st.Display, display)
	}
	if display == "content" {
		if got := "playlist:" + st.PlaylistID; got != contentSource {
			t.Fatalf("content_source = %q, want %q", got, contentSource)
		}
	}
}

// --- DAT-001: scope-node tree structural validity + any-kind attachment --------

func checkScopeTree001(t *testing.T, rawIn, rawExp json.RawMessage) {
	var in struct {
		ScopeNodes []ScopeNode `json:"scope_nodes"`
		Device     struct {
			ScopeNode string `json:"scope_node"`
		} `json:"device"`
		ScreenResource struct {
			ScopeNode string `json:"scope_node"`
		} `json:"screen_resource"`
	}
	if err := json.Unmarshal(rawIn, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var exp struct {
		Valid                        bool   `json:"valid"`
		DeviceAttachmentKind         string `json:"device_attachment_kind"`
		ScreenResourceAttachmentKind string `json:"screen_resource_attachment_kind"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	tree, errs := BuildScopeTree(in.ScopeNodes)
	if exp.Valid && len(errs) != 0 {
		t.Fatalf("BuildScopeTree errors on a valid tree: %+v", errs)
	}
	if k, _ := tree.KindOf(in.Device.ScopeNode); k != exp.DeviceAttachmentKind {
		t.Fatalf("device attachment kind = %q, want %q", k, exp.DeviceAttachmentKind)
	}
	if k, _ := tree.KindOf(in.ScreenResource.ScopeNode); k != exp.ScreenResourceAttachmentKind {
		t.Fatalf("screen-resource attachment kind = %q, want %q", k, exp.ScreenResourceAttachmentKind)
	}
}

// --- DAT-010: org account_state round-trip (vocabulary is api/1's) -------------

func checkAccountStates010(t *testing.T, rawIn, rawExp json.RawMessage) {
	var in struct {
		OrgRows []ScopeNode `json:"org_rows"`
	}
	if err := json.Unmarshal(rawIn, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var exp struct {
		AccountStateByRow map[string]string `json:"account_state_by_row"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	got := map[string]string{}
	for _, o := range in.OrgRows {
		if o.Kind != "org" {
			t.Fatalf("row %s kind = %q, want org", o.ID, o.Kind)
		}
		got[o.ID] = o.AccountState
	}
	for id, want := range exp.AccountStateByRow {
		if got[id] != want {
			t.Fatalf("account_state[%s] = %q, want %q", id, got[id], want)
		}
	}
}

// --- DAT-033: effective tz/lat/long as one unit, own vs ancestor-site walk -----

func checkEffectiveGeo033(t *testing.T, rawIn, rawExp json.RawMessage) {
	in := mustInput(t, rawIn)
	tree, errs := BuildScopeTree(in.ScopeNodes)
	if len(errs) != 0 {
		t.Fatalf("BuildScopeTree: %+v", errs)
	}
	var exp struct {
		EffectiveGeo map[string]struct {
			TZ     string  `json:"tz"`
			Lat    float64 `json:"lat"`
			Long   float64 `json:"long"`
			Source string  `json:"source"`
		} `json:"effective_geo"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	for id, want := range exp.EffectiveGeo {
		g, e := tree.EffectiveGeo(id)
		if e != nil {
			t.Fatalf("EffectiveGeo(%s): %v", id, e)
		}
		if g.TZ != want.TZ || g.Lat != want.Lat || g.Long != want.Long || g.Source != want.Source {
			t.Fatalf("geo[%s] = %+v, want %+v", id, g, want)
		}
	}
}

// --- DAT-051 / DAT-053: single-instant precedence winner + lease --------------

func checkSingleWinner(t *testing.T, rawIn, rawExp json.RawMessage) {
	in := mustInput(t, rawIn)
	store := storeFromInput(in)
	var exp struct {
		ApplicableIDs      []string `json:"applicable_schedule_ids"`
		ApplicableByPrec   []string `json:"applicable_schedule_ids_by_precedence"`
		EffectiveDaypartID *string  `json:"effective_daypart_id"`
		WinningScheduleID  string   `json:"winning_schedule_id"`
		LeaseProjection    struct {
			Display       string `json:"display"`
			ContentSource string `json:"content_source"`
		} `json:"lease_projection"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	tMs := singleMs(t, store, in)

	wantOrder := exp.ApplicableByPrec
	if wantOrder == nil {
		wantOrder = exp.ApplicableIDs
	}
	if got := scheduleIDs(ApplicableSchedules(store, in.ResolveFor, tMs)); !eqStrs(got, wantOrder) {
		t.Fatalf("applicable = %v, want %v", got, wantOrder)
	}

	st, err := Resolve(store, in.ResolveFor, tMs)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if daypartID(st) != strp(exp.EffectiveDaypartID) {
		t.Fatalf("effective daypart = %q, want %q", daypartID(st), strp(exp.EffectiveDaypartID))
	}
	if exp.WinningScheduleID != "" && st.ScheduleID != exp.WinningScheduleID {
		t.Fatalf("winning schedule = %q, want %q", st.ScheduleID, exp.WinningScheduleID)
	}
	assertLease(t, st, exp.LeaseProjection.Display, exp.LeaseProjection.ContentSource)
}

// --- DAT-073: within-schedule daypart overlap rejection -----------------------

// checkDaypartWriteRefusal071 drives a DAT-071 write-time refusal — a daypart
// time-of-day (DAYPART_TIME_INVALID) or days_of_week (DAYPART_DAYS_INVALID)
// violation — through ValidateRows, the same authoring gate the store's write
// path calls, never through the parser alone. The time cases split that
// clause's two halves (well-formed-but-out-of-range, and the 24:00:00
// boundary); the days cases split the OPPOSITE failure directions (the phantom
// weekday that fails open via the wrap tail, and the empty array that fails
// silent). Field, code, AND message are compared byte-exact against the frozen
// expectation: the message is the operator's remedy text, and a silent drift
// there is a corpus edit that should be flagged, not absorbed.
func checkDaypartWriteRefusal071(t *testing.T, rawIn, rawExp json.RawMessage) {
	var in struct {
		Schedule json.RawMessage   `json:"schedule"`
		Dayparts []json.RawMessage `json:"dayparts"`
	}
	if err := json.Unmarshal(rawIn, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var exp struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Field   string `json:"field"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	_, got := ValidateRows(RawRows{Schedules: []json.RawMessage{in.Schedule}, Dayparts: in.Dayparts})
	if len(got) != len(exp.Errors) {
		t.Fatalf("ValidateRows returned %d error(s) %v, corpus expects exactly %d", len(got), got, len(exp.Errors))
	}
	for i, want := range exp.Errors {
		if got[i].Field != want.Field || got[i].Code != want.Code || got[i].Message != want.Message {
			t.Fatalf("error[%d] = {%s, %s, %q}, want {%s, %s, %q}",
				i, got[i].Field, got[i].Code, got[i].Message, want.Field, want.Code, want.Message)
		}
	}
}

func checkOverlapReject073(t *testing.T, rawIn, rawExp json.RawMessage) {
	var in struct {
		Dayparts []Daypart `json:"dayparts"`
	}
	if err := json.Unmarshal(rawIn, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var exp struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Field string `json:"field"`
			Code  string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	got := ValidateNoOverlap(in.Dayparts)
	if got == nil {
		t.Fatalf("expected DAYPART_OVERLAP, got nil")
	}
	if got.Code != exp.Errors[0].Code || got.Field != exp.Errors[0].Field {
		t.Fatalf("error = {%s,%s}, want {%s,%s}", got.Field, got.Code, exp.Errors[0].Field, exp.Errors[0].Code)
	}
}

// --- DAT-074: display_power off projects blank + carries power-off preset ------

func checkDisplayPowerOff074(t *testing.T, rawIn, rawExp json.RawMessage) {
	var in struct {
		Schedule         Schedule  `json:"schedule"`
		Dayparts         []Daypart `json:"dayparts"`
		EvaluatedAtLocal string    `json:"evaluated_at_local"`
	}
	if err := json.Unmarshal(rawIn, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var exp struct {
		DaypartsOverlap    bool   `json:"dayparts_overlap"`
		EffectiveDaypartID string `json:"effective_daypart_id"`
		LeaseProjection    struct {
			ForOvernight struct {
				Display           string `json:"display"`
				DevicePowerOffVia string `json:"device_power_off_via"`
			} `json:"for_overnight_daypart"`
		} `json:"lease_projection"`
		EffectiveMisfireBoth string `json:"effective_misfire_both_dayparts"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	// The case's rows attach at a group with no tree; place that group under a
	// Denver site so the effective tz resolves by ancestor walk (DAT-033).
	grp := in.Schedule.ScopeNode
	nodes := []ScopeNode{
		{ID: "01J0RGNDE1PG2X7R5JQC42EJ00", Kind: "org", Name: "Denver Org"},
		{ID: "01JS1TENDE1EWH0AVNH9DN6R00", Kind: "site", ParentID: ptrStr("01J0RGNDE1PG2X7R5JQC42EJ00"), Name: "Denver Site", TZ: ptrStr("America/Denver"), Lat: ptrF64(39.7392), Long: ptrF64(-104.9903)},
		{ID: grp, Kind: "group", ParentID: ptrStr("01JS1TENDE1EWH0AVNH9DN6R00"), Name: "Effective Group"},
	}
	tree, errs := BuildScopeTree(nodes)
	if len(errs) != 0 {
		t.Fatalf("BuildScopeTree: %+v", errs)
	}
	store := RowStore{Tree: tree, Rows: RowSet{Schedules: []Schedule{in.Schedule}, Dayparts: in.Dayparts}}

	if (ValidateNoOverlap(in.Dayparts) != nil) != exp.DaypartsOverlap {
		t.Fatalf("dayparts_overlap mismatch")
	}
	tMs := localMs(t, "America/Denver", in.EvaluatedAtLocal)
	st, err := Resolve(store, grp, tMs)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if daypartID(st) != exp.EffectiveDaypartID {
		t.Fatalf("effective daypart = %q, want %q", daypartID(st), exp.EffectiveDaypartID)
	}
	if st.Display != exp.LeaseProjection.ForOvernight.Display || st.PlaylistID != "" {
		t.Fatalf("overnight lease = {%s,%s}, want {%s,}", st.Display, st.PlaylistID, exp.LeaseProjection.ForOvernight.Display)
	}
	if want := "preset_batch:" + st.Daypart.PresetBatchID; want != exp.LeaseProjection.ForOvernight.DevicePowerOffVia {
		t.Fatalf("device_power_off_via = %q, want %q", want, exp.LeaseProjection.ForOvernight.DevicePowerOffVia)
	}
	for _, d := range in.Dayparts {
		if got := d.EffectiveMisfire(in.Schedule); got != exp.EffectiveMisfireBoth {
			t.Fatalf("daypart %s misfire = %q, want %q", d.ID, got, exp.EffectiveMisfireBoth)
		}
	}
}

// --- DAT-075: fall-back boundary re-fires the preset on every rising edge ------

func checkPresetFallBack075(t *testing.T, rawIn, rawExp json.RawMessage) {
	in := mustInput(t, rawIn)
	store := storeFromInput(in)
	var exp struct {
		ApplicableByPrec []string  `json:"applicable_schedule_ids_by_precedence"`
		IdentitySequence []*string `json:"effective_daypart_identity_sequence"`
		Results          []struct {
			Label            string  `json:"label"`
			Holds            bool    `json:"holds"`
			EffectiveDaypart *string `json:"effective_daypart_id"`
			RisingEdge       bool    `json:"rising_edge"`
			PresetFired      *string `json:"preset_fired"`
		} `json:"results"`
		Invocations []struct {
			AtUTC         string `json:"at_utc"`
			PresetBatchID string `json:"preset_batch_id"`
		} `json:"preset_invocations_in_order"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}

	// Precedence order over the evaluation window (schedules do not change).
	firstMs := evalMs(t, store, in.ResolveFor, in.Evaluations[0])
	if got := scheduleIDs(ApplicableSchedules(store, in.ResolveFor, firstMs)); !eqStrs(got, exp.ApplicableByPrec) {
		t.Fatalf("applicable = %v, want %v", got, exp.ApplicableByPrec)
	}

	var prev *EffectiveState
	var gotSeq []*string
	type inv struct{ at, batch string }
	var gotInv []inv
	for i, e := range in.Evaluations {
		tMs := evalMs(t, store, in.ResolveFor, e)
		st, err := Resolve(store, in.ResolveFor, tMs)
		if err != nil {
			t.Fatalf("Resolve %s: %v", e.Label, err)
		}
		curID := daypartID(st)
		gotSeq = append(gotSeq, idPtr(curID))

		fire := PresetTransition(prev, &st)
		risingEdge := curID != "" && curID != daypartID(deref(prev))

		r := exp.Results[i]
		if r.Label != e.Label {
			t.Fatalf("eval %d label = %q, expected %q", i, e.Label, r.Label)
		}
		if (st.Daypart != nil) != r.Holds {
			t.Fatalf("%s holds = %v, want %v", e.Label, st.Daypart != nil, r.Holds)
		}
		if curID != strp(r.EffectiveDaypart) {
			t.Fatalf("%s effective = %q, want %q", e.Label, curID, strp(r.EffectiveDaypart))
		}
		if risingEdge != r.RisingEdge {
			t.Fatalf("%s rising_edge = %v, want %v", e.Label, risingEdge, r.RisingEdge)
		}
		firedID := ""
		if fire != nil {
			firedID = fire.PresetBatchID
		}
		if firedID != strp(r.PresetFired) {
			t.Fatalf("%s preset_fired = %q, want %q", e.Label, firedID, strp(r.PresetFired))
		}
		if fire != nil {
			gotInv = append(gotInv, inv{at: e.AtUTC, batch: fire.PresetBatchID})
		}
		cur := st
		prev = &cur
	}

	if !eqStrPtrs(gotSeq, exp.IdentitySequence) {
		t.Fatalf("identity sequence = %v, want %v", ptrSeq(gotSeq), ptrSeq(exp.IdentitySequence))
	}
	if len(gotInv) != len(exp.Invocations) {
		t.Fatalf("invocations = %d, want %d", len(gotInv), len(exp.Invocations))
	}
	for i, w := range exp.Invocations {
		if gotInv[i].batch != w.PresetBatchID || gotInv[i].at != w.AtUTC {
			t.Fatalf("invocation %d = {%s,%s}, want {%s,%s}", i, gotInv[i].at, gotInv[i].batch, w.AtUTC, w.PresetBatchID)
		}
	}
}

// --- DAT-075: a generation apply carries the baseline until the bound rows change ---

// checkCarryAcrossApply075 replays a SEQUENCE OF APPLIES over one continuously
// resolving node, which is the only shape in which DAT-075's carry clause is
// observable: each apply installs its generation's rows, the consumer decides
// whether it may carry the baseline it had reached (MayCarryAcrossApply), and
// the invocation is whatever PresetTransition then reports from that baseline.
//
// The case's dayparting is deliberately inert — one all-day daypart, effective
// at every instant in the case — so the identity sequence is constant and every
// difference between legs comes from the rows, not from the clock. A driver that
// only replayed instants could not tell these legs apart at all.
func checkCarryAcrossApply075(t *testing.T, rawIn, rawExp json.RawMessage) {
	base := mustInput(t, rawIn)
	var in struct {
		Applies []struct {
			Label    string `json:"label"`
			AtLocal  string `json:"at_local"`
			Authored struct {
				Schedules     []Schedule    `json:"schedules"`
				Dayparts      []Daypart     `json:"dayparts"`
				PresetBatches []PresetBatch `json:"preset_batches"`
			} `json:"authored"`
		} `json:"applies"`
	}
	if err := json.Unmarshal(rawIn, &in); err != nil {
		t.Fatalf("decode applies: %v", err)
	}
	var exp struct {
		IdentitySequence []*string `json:"effective_daypart_identity_sequence"`
		Results          []struct {
			Label            string  `json:"label"`
			MayCarry         bool    `json:"may_carry_baseline"`
			EffectiveDaypart *string `json:"effective_daypart_id"`
			PresetFired      *string `json:"preset_fired"`
		} `json:"results"`
		Invocations []struct {
			AtLocal       string `json:"at_local"`
			PresetBatchID string `json:"preset_batch_id"`
		} `json:"preset_invocations_in_order"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	if len(in.Applies) != len(exp.Results) {
		t.Fatalf("applies = %d, expected results = %d", len(in.Applies), len(exp.Results))
	}

	rows := base // the generation currently applied; each leg's `authored` rows replace theirs by id
	var prev *EffectiveState
	var prevBatch *PresetBatch
	var gotSeq []*string
	type inv struct{ at, batch string }
	var gotInv []inv

	for i, a := range in.Applies {
		rows.Schedules = replaceSchedules(rows.Schedules, a.Authored.Schedules)
		rows.Dayparts = replaceDayparts(rows.Dayparts, a.Authored.Dayparts)
		rows.PresetBatches = replacePresetBatches(rows.PresetBatches, a.Authored.PresetBatches)
		store := storeFromInput(rows)

		// The consumer's decision, then the edge that follows from it.
		mayCarry := MayCarryAcrossApply(prev, prevBatch, store)
		baseline := prev
		if !mayCarry {
			baseline = nil
		}
		st, err := Resolve(store, base.ResolveFor, localMs(t, effTZName(t, store, base.ResolveFor), a.AtLocal))
		if err != nil {
			t.Fatalf("Resolve %s: %v", a.Label, err)
		}
		fire := PresetTransition(baseline, &st)

		r := exp.Results[i]
		if r.Label != a.Label {
			t.Fatalf("apply %d label = %q, expected %q", i, a.Label, r.Label)
		}
		if mayCarry != r.MayCarry {
			t.Fatalf("%s may_carry_baseline = %v, want %v", a.Label, mayCarry, r.MayCarry)
		}
		if daypartID(st) != strp(r.EffectiveDaypart) {
			t.Fatalf("%s effective = %q, want %q", a.Label, daypartID(st), strp(r.EffectiveDaypart))
		}
		firedID := ""
		if fire != nil {
			firedID = fire.PresetBatchID
		}
		if firedID != strp(r.PresetFired) {
			t.Fatalf("%s preset_fired = %q, want %q", a.Label, firedID, strp(r.PresetFired))
		}
		if fire != nil {
			gotInv = append(gotInv, inv{at: a.AtLocal, batch: fire.PresetBatchID})
		}
		gotSeq = append(gotSeq, idPtr(daypartID(st)))

		// What this consumer hands its own replacement at the NEXT apply: the
		// baseline it just reached, and the batch that baseline's daypart binds
		// in the generation it was resolved against.
		cur := st
		prev = &cur
		prevBatch = nil
		if cur.Daypart != nil && cur.Daypart.PresetBatchID != "" {
			prevBatch = store.presetBatch(cur.Daypart.PresetBatchID)
		}
	}

	if !eqStrPtrs(gotSeq, exp.IdentitySequence) {
		t.Fatalf("identity sequence = %v, want %v", ptrSeq(gotSeq), ptrSeq(exp.IdentitySequence))
	}
	if len(gotInv) != len(exp.Invocations) {
		t.Fatalf("invocations = %d, want %d", len(gotInv), len(exp.Invocations))
	}
	for i, w := range exp.Invocations {
		if gotInv[i].batch != w.PresetBatchID || gotInv[i].at != w.AtLocal {
			t.Fatalf("invocation %d = {%s,%s}, want {%s,%s}", i, gotInv[i].at, gotInv[i].batch, w.AtLocal, w.PresetBatchID)
		}
	}
}

// replaceSchedules/replaceDayparts/replacePresetBatches apply one generation's
// authored rows over the previously applied set, matching on the row's own
// identity field (a preset batch's is `preset_id`, DAT-005). An authored row
// with an unknown id is appended, so a leg can add a row as well as re-author
// one.
//
// Each returns a FRESH slice rather than editing in place, which is not a style
// choice: a consumer hands its replacement a pointer to the row its baseline was
// resolved against (CarriedBaseline's own shape), and a driver that re-authored
// rows in the previous generation's backing array would have that pointer read
// the NEW row — every comparison equal, every leg carried, the case green
// against an engine that does nothing. A generation is its own parsed row set on
// the wire, and it must be one here too.
func replaceSchedules(cur, authored []Schedule) []Schedule {
	out := append([]Schedule(nil), cur...)
	for _, a := range authored {
		replaced := false
		for i := range out {
			if out[i].ID == a.ID {
				out[i], replaced = a, true
			}
		}
		if !replaced {
			out = append(out, a)
		}
	}
	return out
}

func replaceDayparts(cur, authored []Daypart) []Daypart {
	out := append([]Daypart(nil), cur...)
	for _, a := range authored {
		replaced := false
		for i := range out {
			if out[i].ID == a.ID {
				out[i], replaced = a, true
			}
		}
		if !replaced {
			out = append(out, a)
		}
	}
	return out
}

func replacePresetBatches(cur, authored []PresetBatch) []PresetBatch {
	out := append([]PresetBatch(nil), cur...)
	for _, a := range authored {
		replaced := false
		for i := range out {
			if out[i].PresetID == a.PresetID {
				out[i], replaced = a, true
			}
		}
		if !replaced {
			out = append(out, a)
		}
	}
	return out
}

// --- DAT-075: masked daypart fires nothing; fall-through fires base -----------

func checkPresetMasked075(t *testing.T, rawIn, rawExp json.RawMessage) {
	in := mustInput(t, rawIn)
	store := storeFromInput(in)
	loc, err := time.LoadLocation(effTZName(t, store, in.ResolveFor))
	if err != nil {
		t.Fatalf("load loc: %v", err)
	}
	var exp struct {
		ApplicableByPrec []string `json:"applicable_schedule_ids_by_precedence"`
		Results          []struct {
			Label                    string  `json:"label"`
			EffectiveDaypart         *string `json:"effective_daypart_id"`
			WinningScheduleID        *string `json:"winning_schedule_id"`
			PresetFired              *string `json:"preset_fired"`
			MaskedDaypartID          string  `json:"masked_daypart_id"`
			MaskedDaypartPresetFired bool    `json:"masked_daypart_preset_fired"`
		} `json:"results"`
		Invocations []struct {
			AtLocal       string `json:"at_local"`
			PresetBatchID string `json:"preset_batch_id"`
		} `json:"preset_invocations_in_order"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}

	firstMs := evalMs(t, store, in.ResolveFor, in.Evaluations[0])
	if got := scheduleIDs(ApplicableSchedules(store, in.ResolveFor, firstMs)); !eqStrs(got, exp.ApplicableByPrec) {
		t.Fatalf("applicable = %v, want %v", got, exp.ApplicableByPrec)
	}

	var prev *EffectiveState
	type inv struct{ at, batch string }
	var gotInv []inv
	for i, e := range in.Evaluations {
		tMs := evalMs(t, store, in.ResolveFor, e)
		st, err := Resolve(store, in.ResolveFor, tMs)
		if err != nil {
			t.Fatalf("Resolve %s: %v", e.Label, err)
		}
		fire := PresetTransition(prev, &st)
		r := exp.Results[i]

		if daypartID(st) != strp(r.EffectiveDaypart) {
			t.Fatalf("%s effective = %q, want %q", e.Label, daypartID(st), strp(r.EffectiveDaypart))
		}
		if st.ScheduleID != strp(r.WinningScheduleID) {
			t.Fatalf("%s winning schedule = %q, want %q", e.Label, st.ScheduleID, strp(r.WinningScheduleID))
		}
		firedID := ""
		if fire != nil {
			firedID = fire.PresetBatchID
		}
		if firedID != strp(r.PresetFired) {
			t.Fatalf("%s preset_fired = %q, want %q", e.Label, firedID, strp(r.PresetFired))
		}
		// The masked daypart is holding on its own schedule yet fires nothing:
		// only the effective daypart has effect (DAT-111).
		if r.MaskedDaypartID != "" {
			owning := scheduleOf(in.Schedules, in.Dayparts, r.MaskedDaypartID)
			held := HoldingDaypart(owning, in.Dayparts, loc, tMs)
			if held == nil || held.ID != r.MaskedDaypartID {
				t.Fatalf("%s masked daypart %q is not holding on its schedule", e.Label, r.MaskedDaypartID)
			}
			if firedID == held.PresetBatchID && held.ID != daypartID(st) {
				t.Fatalf("%s masked daypart preset must not fire", e.Label)
			}
			if exp.Results[i].MaskedDaypartPresetFired {
				t.Fatalf("%s corpus asserts masked fires false", e.Label)
			}
		}
		if fire != nil {
			gotInv = append(gotInv, inv{at: e.AtLocal, batch: fire.PresetBatchID})
		}
		cur := st
		prev = &cur
	}
	if len(gotInv) != len(exp.Invocations) {
		t.Fatalf("invocations = %d, want %d", len(gotInv), len(exp.Invocations))
	}
	for i, w := range exp.Invocations {
		if gotInv[i].batch != w.PresetBatchID || gotInv[i].at != w.AtLocal {
			t.Fatalf("invocation %d = {%s,%s}, want {%s,%s}", i, gotInv[i].at, gotInv[i].batch, w.AtLocal, w.PresetBatchID)
		}
	}
}

// --- DAT-092: preset-batch partial-failure outcome ----------------------------

func checkBatchOutcome092(t *testing.T, rawIn, rawExp json.RawMessage) {
	var in struct {
		PresetBatch       json.RawMessage `json:"preset_batch"`
		InvocationResults []struct {
			EntityID string `json:"entity_id"`
			Command  string `json:"command"`
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
		} `json:"invocation_results"`
	}
	if err := json.Unmarshal(rawIn, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var exp struct {
		Valid                 bool `json:"valid"`
		BothCommandsAttempted bool `json:"both_commands_attempted"`
		LastOutcome           struct {
			Outcome string `json:"outcome"`
			Results []struct {
				Target  string `json:"target"`
				Command string `json:"command"`
				OK      bool   `json:"ok"`
				Error   string `json:"error"`
			} `json:"results"`
			EvaluatedAt int64 `json:"evaluated_at"`
		} `json:"last_outcome"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}

	// The batch row itself validates (non-empty commands, no pack field, DAT-091).
	rs, errs := ValidateRows(RawRows{PresetBatches: []json.RawMessage{in.PresetBatch}})
	if exp.Valid && len(errs) != 0 {
		t.Fatalf("preset batch row invalid: %+v", errs)
	}
	if len(rs.PresetBatches) != 1 {
		t.Fatalf("expected 1 parsed preset batch, got %d", len(rs.PresetBatches))
	}
	batch := rs.PresetBatches[0]

	results := make([]CommandResult, 0, len(in.InvocationResults))
	for _, ir := range in.InvocationResults {
		results = append(results, CommandResult{Target: ir.EntityID, Command: ir.Command, OK: ir.OK, Error: ir.Error})
	}
	if (len(results) == len(batch.Commands)) != exp.BothCommandsAttempted {
		t.Fatalf("both_commands_attempted mismatch")
	}

	// The batch's outcome is recorded at the row's own updated instant, sourced
	// from the parsed row (not the expected block) so evaluated_at is a real check.
	out := BatchOutcome(results, batch.UpdatedAt)
	if out.Outcome != exp.LastOutcome.Outcome {
		t.Fatalf("outcome = %q, want %q", out.Outcome, exp.LastOutcome.Outcome)
	}
	if out.EvaluatedAt != exp.LastOutcome.EvaluatedAt {
		t.Fatalf("evaluated_at = %d, want %d", out.EvaluatedAt, exp.LastOutcome.EvaluatedAt)
	}
	if len(out.Results) != len(exp.LastOutcome.Results) {
		t.Fatalf("results len = %d, want %d", len(out.Results), len(exp.LastOutcome.Results))
	}
	for i, w := range exp.LastOutcome.Results {
		g := out.Results[i]
		if g.Target != w.Target || g.Command != w.Command || g.OK != w.OK || g.Error != w.Error {
			t.Fatalf("result %d = %+v, want %+v", i, g, w)
		}
	}
}

// --- DAT-101: pack-owned scheduler row rejected -------------------------------

func checkPackOwned101(t *testing.T, rawIn, rawExp json.RawMessage) {
	var in struct {
		Schedule json.RawMessage `json:"schedule"`
	}
	if err := json.Unmarshal(rawIn, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var exp struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Field string `json:"field"`
			Code  string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	_, errs := ValidateRows(RawRows{Schedules: []json.RawMessage{in.Schedule}})
	if len(errs) != 1 {
		t.Fatalf("errors = %+v, want exactly one", errs)
	}
	if errs[0].Code != exp.Errors[0].Code || errs[0].Field != exp.Errors[0].Field {
		t.Fatalf("error = {%s,%s}, want {%s,%s}", errs[0].Field, errs[0].Code, exp.Errors[0].Field, exp.Errors[0].Code)
	}
}

// --- DAT-004a/DAT-005b: the two identity rows ---------------------------------

// checkIdentityRows004a asserts a conformant screen identity row and device row
// validate (DAT-005b's baseline, positive half) and that neither is placed at a
// screen-kind node — the placement freedom DAT-004 grants.
//
// It does NOT check DAT-004a, and the case it drives no longer claims to. The
// id-separation assertions below — that each row's id is what a screen_id or
// device_id names, that neither equals a scope-node id in the fixture, that
// neither placement is the screen-kind node — are properties of the FIXTURE's
// own chosen constants. They hold whatever this validator does, because this
// validator is handed the row bundle and nothing else: it never sees a screen_id
// REFERENCE, so there is no resolution here for an implementation to get wrong.
// They are kept as fixture guards, not as tests: a fixture that reused a node id
// as a row id, or placed a row at the screen-kind node, would silently stop
// demonstrating DAT-004's placement freedom, and these catch that.
//
// DAT-004a's own obligation is a refusal on a surface that RESOLVES a screen_id,
// and it sits at TBD-wave1 for that reason — see
// conformance/unimplemented-error-codes.json's data-model-1 entry.
func checkIdentityRows004a(t *testing.T, rawIn, rawExp json.RawMessage) {
	var in struct {
		ScopeNodes map[string]string `json:"scope_nodes"`
		Screens    []json.RawMessage `json:"screens"`
		Devices    []json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(rawIn, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var exp struct {
		Valid                      bool   `json:"valid"`
		ScreenIDIsScreenRowsOwnID  string `json:"screen_id_is_the_screen_rows_own_id"`
		DeviceIDIsDeviceRowsOwnID  string `json:"device_id_is_the_device_rows_own_id"`
		IdentityIDsAreNotScopeIDs  bool   `json:"identity_ids_are_not_scope_node_ids"`
		PlacementsAreNotScreenKind bool   `json:"placements_are_not_screen_kind_nodes"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}

	set, errs := ValidateIdentityRows(RawIdentityRows{Screens: in.Screens, Devices: in.Devices})
	if exp.Valid && len(errs) != 0 {
		t.Fatalf("a conformant identity bundle was rejected: %+v", errs)
	}
	if len(set.Screens) != 1 || len(set.Devices) != 1 {
		t.Fatalf("parsed %d screen(s)/%d device(s), want 1/1", len(set.Screens), len(set.Devices))
	}

	if got := set.Screens[0].ID; got != exp.ScreenIDIsScreenRowsOwnID {
		t.Errorf("the screen row's own id = %q, but the case names the screen_id as %q", got, exp.ScreenIDIsScreenRowsOwnID)
	}
	if got := set.Devices[0].ID; got != exp.DeviceIDIsDeviceRowsOwnID {
		t.Errorf("the device row's own id = %q, but the case names the device_id as %q", got, exp.DeviceIDIsDeviceRowsOwnID)
	}

	if exp.IdentityIDsAreNotScopeIDs {
		for role, nodeID := range in.ScopeNodes {
			if set.Screens[0].ID == nodeID {
				t.Errorf("the screen identity row's id equals the %s scope node's id — DAT-004a makes them distinct rows", role)
			}
			if set.Devices[0].ID == nodeID {
				t.Errorf("the device row's id equals the %s scope node's id — DAT-004a makes them distinct rows", role)
			}
		}
	}
	if exp.PlacementsAreNotScreenKind {
		screenKindNode := in.ScopeNodes["screen_kind_node"]
		if screenKindNode == "" {
			t.Fatal("the case claims placements avoid a screen-kind node but names none")
		}
		if set.Screens[0].ScopeNode == screenKindNode || set.Devices[0].ScopeNode == screenKindNode {
			t.Error("a row is placed at the screen-kind node, so this case would not demonstrate DAT-004's placement freedom")
		}
	}
}

// checkIdentityBaseline005b asserts the identity rows are held to DAT-005a's id
// grammar and DAT-006's placement requirement — the baseline DAT-005b binds them
// to — by rejecting a pair that carries neither.
func checkIdentityBaseline005b(t *testing.T, rawIn, rawExp json.RawMessage) {
	var in struct {
		Screens []json.RawMessage `json:"screens"`
		Devices []json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(rawIn, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var exp struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Field string `json:"field"`
			Code  string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	if exp.Valid {
		t.Fatal("this case is the negative half; a `valid: true` expectation would assert nothing")
	}

	_, errs := ValidateIdentityRows(RawIdentityRows{Screens: in.Screens, Devices: in.Devices})
	if len(errs) == 0 {
		t.Fatal("an identity bundle with neither a canonical id nor a placement was accepted")
	}
	// Every {field, code} the case pins must be present. The comparison is by
	// SET rather than by index: the two tables are validated in a fixed order,
	// but a case asserting that order would be asserting an implementation
	// detail rather than the requirement.
	for _, want := range exp.Errors {
		var found bool
		for _, got := range errs {
			if got.Field == want.Field && got.Code == want.Code {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected a {%s, %s} error; got %+v", want.Field, want.Code, errs)
		}
	}
}

// --- DAT-111: per-instant layering, holiday over base -------------------------

func checkLayered111(t *testing.T, rawIn, rawExp json.RawMessage) {
	in := mustInput(t, rawIn)
	store := storeFromInput(in)
	var exp struct {
		ApplicableByPrec []string `json:"applicable_schedule_ids_by_precedence"`
		Results          []struct {
			Label             string `json:"label"`
			WinningScheduleID string `json:"winning_schedule_id"`
			EffectiveDaypart  string `json:"effective_daypart_id"`
			LeaseProjection   struct {
				Display       string `json:"display"`
				ContentSource string `json:"content_source"`
			} `json:"lease_projection"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	firstMs := evalMs(t, store, in.ResolveFor, in.Evaluations[0])
	if got := scheduleIDs(ApplicableSchedules(store, in.ResolveFor, firstMs)); !eqStrs(got, exp.ApplicableByPrec) {
		t.Fatalf("applicable = %v, want %v", got, exp.ApplicableByPrec)
	}
	for i, e := range in.Evaluations {
		st, err := Resolve(store, in.ResolveFor, evalMs(t, store, in.ResolveFor, e))
		if err != nil {
			t.Fatalf("Resolve %s: %v", e.Label, err)
		}
		r := exp.Results[i]
		if st.ScheduleID != r.WinningScheduleID || daypartID(st) != r.EffectiveDaypart {
			t.Fatalf("%s = {%s,%s}, want {%s,%s}", e.Label, st.ScheduleID, daypartID(st), r.WinningScheduleID, r.EffectiveDaypart)
		}
		assertLease(t, st, r.LeaseProjection.Display, r.LeaseProjection.ContentSource)
	}
}

// --- DAT-117: fallback layer falls through the precedence order ---------------

func checkFallback117(t *testing.T, rawIn, rawExp json.RawMessage) {
	in := mustInput(t, rawIn)
	store := storeFromInput(in)
	var exp struct {
		ApplicableByPrec  []string `json:"applicable_schedule_ids_by_precedence"`
		EffectiveDaypart  *string  `json:"effective_daypart_id"`
		FallbackSelection struct {
			SkippedNoFallback  string `json:"skipped_schedule_id_no_fallback"`
			ResolvedFallbackID string `json:"resolved_fallback_id"`
			ResolvedFrom       string `json:"resolved_from_schedule_id"`
		} `json:"fallback_selection"`
		LeaseProjection struct {
			Display       string `json:"display"`
			ContentSource string `json:"content_source"`
		} `json:"lease_projection"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	tMs := singleMs(t, store, in)
	if got := scheduleIDs(ApplicableSchedules(store, in.ResolveFor, tMs)); !eqStrs(got, exp.ApplicableByPrec) {
		t.Fatalf("applicable = %v, want %v", got, exp.ApplicableByPrec)
	}
	st, err := Resolve(store, in.ResolveFor, tMs)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if daypartID(st) != strp(exp.EffectiveDaypart) {
		t.Fatalf("effective daypart = %q, want none", daypartID(st))
	}
	if st.Fallback == nil || st.Fallback.ID != exp.FallbackSelection.ResolvedFallbackID {
		t.Fatalf("fallback = %v, want %q", st.Fallback, exp.FallbackSelection.ResolvedFallbackID)
	}
	if st.ScheduleID != exp.FallbackSelection.ResolvedFrom {
		t.Fatalf("resolved_from = %q, want %q", st.ScheduleID, exp.FallbackSelection.ResolvedFrom)
	}
	assertLease(t, st, exp.LeaseProjection.Display, exp.LeaseProjection.ContentSource)
}

// --- DAT-118: terminal default blank when nothing is applicable-in-force ------

func checkTerminal118(t *testing.T, rawIn, rawExp json.RawMessage) {
	in := mustInput(t, rawIn)
	store := storeFromInput(in)
	var exp struct {
		ApplicableInForce  []string `json:"applicable_in_force_schedule_ids"`
		EffectiveDaypart   *string  `json:"effective_daypart_id"`
		ResolvedFallbackID *string  `json:"resolved_fallback_id"`
		LeaseProjection    struct {
			Display string `json:"display"`
		} `json:"lease_projection"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	tMs := singleMs(t, store, in)
	if got := scheduleIDs(ApplicableSchedules(store, in.ResolveFor, tMs)); len(got) != 0 {
		t.Fatalf("applicable-in-force = %v, want []", got)
	}
	st, err := Resolve(store, in.ResolveFor, tMs)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if st.Daypart != nil || st.Fallback != nil {
		t.Fatalf("state = %+v, want terminal blank", st)
	}
	if st.Display != exp.LeaseProjection.Display {
		t.Fatalf("display = %q, want %q", st.Display, exp.LeaseProjection.Display)
	}
}

// --- DAT-119: fall-back window holds continuously through the repeated hour ----

func checkHoldSequence119(t *testing.T, rawIn, rawExp json.RawMessage) {
	in := mustInput(t, rawIn)
	store := storeFromInput(in)
	var exp struct {
		Results []struct {
			Label            string  `json:"label"`
			Holds            bool    `json:"holds"`
			EffectiveDaypart *string `json:"effective_daypart_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	for i, e := range in.Evaluations {
		st, err := Resolve(store, in.ResolveFor, evalMs(t, store, in.ResolveFor, e))
		if err != nil {
			t.Fatalf("Resolve %s: %v", e.Label, err)
		}
		r := exp.Results[i]
		if (st.Daypart != nil) != r.Holds {
			t.Fatalf("%s holds = %v, want %v", e.Label, st.Daypart != nil, r.Holds)
		}
		if daypartID(st) != strp(r.EffectiveDaypart) {
			t.Fatalf("%s effective = %q, want %q", e.Label, daypartID(st), strp(r.EffectiveDaypart))
		}
	}
}

// --- DAT-119: spring-forward window begins at the first real instant ----------

func checkSpringForward119(t *testing.T, rawIn, rawExp json.RawMessage) {
	in := mustInput(t, rawIn)
	store := storeFromInput(in)
	loc, err := time.LoadLocation(effTZName(t, store, in.ResolveFor))
	if err != nil {
		t.Fatalf("load loc: %v", err)
	}
	var exp struct {
		ApplicableByPrec []string `json:"applicable_schedule_ids_by_precedence"`
		Results          []struct {
			Label            string  `json:"label"`
			DaypartAHolds    bool    `json:"daypart_A_holds"`
			DaypartBHolds    bool    `json:"daypart_B_holds"`
			EffectiveDaypart *string `json:"effective_daypart_id"`
		} `json:"results"`
		WhollyInGap struct {
			DaypartID   string `json:"daypart_id"`
			HoldsOnDate bool   `json:"holds_on_date"`
		} `json:"window_wholly_in_gap_never_holds"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	firstMs := evalMs(t, store, in.ResolveFor, in.Evaluations[0])
	if got := scheduleIDs(ApplicableSchedules(store, in.ResolveFor, firstMs)); !eqStrs(got, exp.ApplicableByPrec) {
		t.Fatalf("applicable = %v, want %v", got, exp.ApplicableByPrec)
	}
	dpA := daypartByID(in.Dayparts, "01JDAYPARTAAA7RCG2V0CQV000")
	dpB := daypartByID(in.Dayparts, exp.WhollyInGap.DaypartID)
	for i, e := range in.Evaluations {
		tMs := evalMs(t, store, in.ResolveFor, e)
		r := exp.Results[i]
		if Holds(dpA, loc, tMs) != r.DaypartAHolds {
			t.Fatalf("%s A holds = %v, want %v", e.Label, Holds(dpA, loc, tMs), r.DaypartAHolds)
		}
		if Holds(dpB, loc, tMs) != r.DaypartBHolds {
			t.Fatalf("%s B holds = %v, want %v", e.Label, Holds(dpB, loc, tMs), r.DaypartBHolds)
		}
		if exp.WhollyInGap.HoldsOnDate == false && Holds(dpB, loc, tMs) {
			t.Fatalf("%s window wholly in gap must never hold", e.Label)
		}
		st, err := Resolve(store, in.ResolveFor, tMs)
		if err != nil {
			t.Fatalf("Resolve %s: %v", e.Label, err)
		}
		if daypartID(st) != strp(r.EffectiveDaypart) {
			t.Fatalf("%s effective = %q, want %q", e.Label, daypartID(st), strp(r.EffectiveDaypart))
		}
	}
}

// --- DAT-121: misfire default catch_up_once by kind vs rules/1 skip -----------

func checkMisfire121(t *testing.T, rawIn, rawExp json.RawMessage) {
	var in struct {
		Schedule Schedule `json:"schedule"`
		Daypart  Daypart  `json:"daypart"`
	}
	if err := json.Unmarshal(rawIn, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var exp struct {
		ScheduleMisfire string `json:"schedule_effective_misfire"`
		DaypartMisfire  string `json:"daypart_effective_misfire"`
		DaypartAtResume struct {
			EffectiveDaypartID  string `json:"effective_daypart_id"`
			ResolvedImmediately bool   `json:"resolved_immediately"`
		} `json:"daypart_state_at_relay_resume"`
		Rules1Misfire   string `json:"rules_1_time_trigger_effective_misfire"`
		Rules1FiredLate bool   `json:"rules_1_time_trigger_fired_late_at_resume"`
	}
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		t.Fatalf("decode expected: %v", err)
	}

	if got := in.Schedule.EffectiveMisfire(); got != exp.ScheduleMisfire {
		t.Fatalf("schedule misfire = %q, want %q", got, exp.ScheduleMisfire)
	}
	if got := in.Daypart.EffectiveMisfire(in.Schedule); got != exp.DaypartMisfire {
		t.Fatalf("daypart misfire = %q, want %q", got, exp.DaypartMisfire)
	}

	// catch_up_once: the current holding state is re-resolved fresh on resume, not
	// queued. The daypart (08:00-20:00, all days) holds at the 10:00 resume instant.
	resume := localMs(t, "UTC", "2026-07-14T10:00:00")
	held := Holds(in.Daypart, time.UTC, resume)
	if held != exp.DaypartAtResume.ResolvedImmediately || (held && in.Daypart.ID != exp.DaypartAtResume.EffectiveDaypartID) {
		t.Fatalf("resume-state holds = %v (id %s), want %v (%s)", held, in.Daypart.ID, exp.DaypartAtResume.ResolvedImmediately, exp.DaypartAtResume.EffectiveDaypartID)
	}

	// The rules/1 time trigger keeps rules/1's own default: "" → skip → no late
	// fire on resume (reusing the committed rules/schedule misfire engine).
	if exp.Rules1Misfire != "skip" {
		t.Fatalf("corpus rules_1 default = %q, want skip", exp.Rules1Misfire)
	}
	firedLate := len(schedule.ApplyMisfire("", []int64{resume})) > 0
	if firedLate != exp.Rules1FiredLate {
		t.Fatalf("rules/1 fired-late = %v, want %v", firedLate, exp.Rules1FiredLate)
	}
}

// --- small assertion helpers ---------------------------------------------------

func scheduleIDs(ss []Schedule) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func idPtr(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func eqStrPtrs(a, b []*string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strp(a[i]) != strp(b[i]) {
			return false
		}
	}
	return true
}

func ptrSeq(ss []*string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		if s == nil {
			out[i] = "null"
		} else {
			out[i] = *s
		}
	}
	return out
}

func deref(p *EffectiveState) EffectiveState {
	if p == nil {
		return EffectiveState{}
	}
	return *p
}

func daypartByID(dps []Daypart, id string) Daypart {
	for _, d := range dps {
		if d.ID == id {
			return d
		}
	}
	return Daypart{}
}

// scheduleOf returns the schedule owning the daypart with dpID.
func scheduleOf(schedules []Schedule, dayparts []Daypart, dpID string) Schedule {
	d := daypartByID(dayparts, dpID)
	for _, s := range schedules {
		if s.ID == d.ScheduleID {
			return s
		}
	}
	return Schedule{}
}
