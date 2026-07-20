// Package deviceclass1 is the executable device-class-registry/1 conformance
// driver: the §10 differential oracle for the registry's structural grammar
// (Validate), its no-shadow registration policy (REG-012/031), and its
// resolution/consultation API (Classify, CommandExists). It replays every
// conformance/corpora/device-class-registry case against the LIVE
// internal/deviceclass implementation and diffs the actual behavior against
// each case's own declared `expected` block.
//
// Unlike the relay1/player1 drivers, device-class-registry/1 has no pluggable
// wire target to swap: internal/deviceclass is a pure, stdlib-only data
// library, not a network peer with alternative implementations. So this
// driver's "teeth" proof (TestDeviceClass1DriverHasTeeth, driver_test.go)
// takes the opposite shape from theirs: instead of pointing the SAME driver
// at a deliberately-broken implementation, it hands the SAME driver a
// deliberately-corrupted `expected` block for a case whose real behavior is
// known-good, and confirms the driver reports FAIL rather than rubber-
// stamping every case PASS.
//
// Registration-time collision policy (REG-012/031) has no runtime
// registration verb yet — the pack→host registration entry point belongs to
// ctx/1, a later contract (device-class-registry/1's own Conformance note:
// "the corpus exercises the structural accept/reject only"). This driver
// exercises that structural accept/reject decision directly: given the
// platform's already-established Builtin() registry as the "existing" state,
// would a registration attempt for a colliding class id / group name be
// accepted or rejected, and does a rejection leave the existing entry
// unshadowed. attemptClassRegistration/attemptGroupRegistration are that
// structural policy, live in this driver rather than internal/deviceclass,
// because they model a REGISTRATION TIME EVENT ctx/1 will later drive
// end-to-end — not a fact internal/deviceclass's own Validate/Registry API
// needs to expose today.
package deviceclass1

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/deviceclass"
)

const contract = "device-class-registry/1"

// Run loads the frozen device-class-registry corpus from disk and drives
// every case against the live internal/deviceclass implementation.
func Run() report.Report {
	rep := report.Report{Driver: "deviceclass1", Target: "internal/deviceclass"}

	cases, err := LoadCorpus()
	if err != nil {
		rep.Fail("corpus", contract, fmt.Sprintf("load device-class-registry corpus: %v", err))
		return rep
	}
	driveCases(&rep, cases)
	return rep
}

// RunCases drives the identical per-case logic Run uses against an explicit,
// caller-supplied case set. It is the seam TestDeviceClass1DriverHasTeeth
// uses: load the real corpus, corrupt one case's `expected` block in memory,
// and confirm the SAME comparison logic (never a re-implementation of it)
// reports FAIL against the corrupted expectation.
func RunCases(cases map[string]corpus.Case) report.Report {
	rep := report.Report{Driver: "deviceclass1", Target: "internal/deviceclass"}
	driveCases(&rep, cases)
	return rep
}

func driveCases(rep *report.Report, cases map[string]corpus.Case) {
	driveValidateCase(rep, cases, "roku-media-player")
	driveValidateCase(rep, cases, "REG-011")
	driveValidateCase(rep, cases, "REG-042")
	driveREG012(rep, cases)
	driveREG031(rep, cases)
	driveREG021(rep, cases)
	driveREG052(rep, cases)
}

// driveValidateCase drives a purely-structural case: decode input's
// device_classes into a RawRegistry, run deviceclass.Validate, and diff the
// resulting valid/errors against the case's declared expectation. This
// covers both the happy-path oracle (roku-media-player) and every negative
// structural-grammar case (REG-011 ORIGIN_INVALID, REG-042
// ATTRIBUTE_UNIT_NOT_APPLICABLE) with one function, mirroring how
// deviceclass.Validate itself reports every violation uniformly.
func driveValidateCase(rep *report.Report, cases map[string]corpus.Case, short string) {
	c, ok := corpus.ByID(cases, short)
	if !ok {
		rep.Fail(short, contract, "case not found in frozen corpus")
		return
	}

	var raw deviceclass.RawRegistry
	if err := decodeField(c.Input, &raw); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input.device_classes: %v", err))
		return
	}

	gotErrs := deviceclass.Validate(raw)
	gotValid := len(gotErrs) == 0
	gotCodes := errorCodes(gotErrs)

	var diffs []report.Diff

	wantValid, present := c.Expect("valid")
	if !present {
		diffs = append(diffs, report.Diff{Field: "valid", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if wb, ok := wantValid.(bool); !ok || wb != gotValid {
		diffs = append(diffs, report.Diff{Field: "valid", Expected: wantValid, Actual: gotValid})
	}

	wantCodes, err := expectedErrorCodes(c)
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "errors", Expected: "<a parseable errors array>", Actual: err.Error()})
	} else if !reflect.DeepEqual(wantCodes, gotCodes) {
		diffs = append(diffs, report.Diff{Field: "errors[].code", Expected: wantCodes, Actual: gotCodes})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "structural validation diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// driveREG012 drives the class-identifier no-shadow collision case (REG-012):
// an extension attempts to register a new class whose identifier already
// resolves in the existing (built-in) registry. The attempt MUST be rejected
// outright, and the existing entry MUST NOT be shadowed, overridden, or
// merged with the rejected attempt's content.
func driveREG012(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REG-012")
	if !ok {
		rep.Fail("REG-012", contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		Attempt struct {
			ID    string                 `json:"id"`
			Entry deviceclass.ClassEntry `json:"entry"`
		} `json:"attempt"`
	}
	if err := decodeField(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input.attempt: %v", err))
		return
	}

	existing := deviceclass.Builtin()
	before, existedBefore := existing.Class(input.Attempt.ID)

	rejected, code := attemptClassRegistration(existing, input.Attempt.ID, input.Attempt.Entry)

	after, existedAfter := existing.Class(input.Attempt.ID)

	var diffs []report.Diff
	if want := c.ExpectBool("registration_rejected"); rejected != want {
		diffs = append(diffs, report.Diff{Field: "registration_rejected", Expected: want, Actual: rejected})
	}
	if want := c.ExpectString("error_code"); code != want {
		diffs = append(diffs, report.Diff{Field: "error_code", Expected: want, Actual: code})
	}
	if existedBefore != existedAfter || !reflect.DeepEqual(before, after) {
		diffs = append(diffs, report.Diff{Field: "existing_entry_unshadowed", Expected: before, Actual: after})
	}
	if want := c.ExpectString("existing_origin_unchanged"); want != "" && after.Origin != want {
		diffs = append(diffs, report.Diff{Field: "existing_origin_unchanged", Expected: want, Actual: after.Origin})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "class-identifier collision policy diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// attemptClassRegistration models device-class-registry/1's REG-012
// collision policy at the structural level: a candidate class id that already
// resolves in the existing registry is rejected outright — never merged into
// or shadowing the existing entry, regardless of whether that existing entry
// is itself built-in or extension-registered. The actual pack→host
// registration verb belongs to ctx/1 (deferred, out of scope per this
// contract's Conformance note); this is the accept/reject decision that verb
// will enforce.
func attemptClassRegistration(existing deviceclass.Registry, id string, _ deviceclass.ClassEntry) (rejected bool, code string) {
	if _, ok := existing.Class(id); ok {
		return true, "CLASS_IDENTIFIER_COLLISION"
	}
	return false, ""
}

// driveREG031 drives the group-name no-shadow collision case (REG-031): an
// extension attempts to register a semantic group whose name already exists
// for that class. The attempt MUST be rejected outright, and the existing
// group's membership MUST NOT be redefined or merged with the rejected
// attempt's members.
func driveREG031(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REG-031")
	if !ok {
		rep.Fail("REG-031", contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		Attempt struct {
			DeviceClass string   `json:"device_class"`
			GroupName   string   `json:"group_name"`
			Members     []string `json:"members"`
		} `json:"attempt"`
	}
	if err := decodeField(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input.attempt: %v", err))
		return
	}

	existing := deviceclass.Builtin()
	entryBefore, _ := existing.Class(input.Attempt.DeviceClass)
	before := append([]string(nil), entryBefore.SemanticGroups[input.Attempt.GroupName]...)

	rejected, code := attemptGroupRegistration(existing, input.Attempt.DeviceClass, input.Attempt.GroupName, input.Attempt.Members)

	entryAfter, _ := existing.Class(input.Attempt.DeviceClass)
	after := entryAfter.SemanticGroups[input.Attempt.GroupName]

	var diffs []report.Diff
	if want := c.ExpectBool("registration_rejected"); rejected != want {
		diffs = append(diffs, report.Diff{Field: "registration_rejected", Expected: want, Actual: rejected})
	}
	if want := c.ExpectString("error_code"); code != want {
		diffs = append(diffs, report.Diff{Field: "error_code", Expected: want, Actual: code})
	}
	if !reflect.DeepEqual(before, after) {
		diffs = append(diffs, report.Diff{Field: "existing_group_members_unshadowed", Expected: before, Actual: after})
	}

	var wantMembers []string
	if raw, present := c.Expected["existing_group_members_unchanged"]; present {
		if err := decodeField(map[string]any{"v": raw}, &struct {
			V *[]string `json:"v"`
		}{&wantMembers}); err != nil {
			diffs = append(diffs, report.Diff{Field: "existing_group_members_unchanged", Expected: "<a parseable string array>", Actual: err.Error()})
		} else if !reflect.DeepEqual(wantMembers, after) {
			diffs = append(diffs, report.Diff{Field: "existing_group_members_unchanged", Expected: wantMembers, Actual: after})
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "group-name collision policy diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// attemptGroupRegistration models device-class-registry/1's REG-031
// collision policy at the structural level: a candidate group name that
// already exists for the target class is rejected outright — never merged
// into or redefining the existing group's membership. Same deferred-verb
// relationship to ctx/1 as attemptClassRegistration.
func attemptGroupRegistration(existing deviceclass.Registry, deviceClass, groupName string, _ []string) (rejected bool, code string) {
	entry, ok := existing.Class(deviceClass)
	if !ok {
		return false, ""
	}
	if _, exists := entry.SemanticGroups[groupName]; exists {
		return true, "GROUP_NAME_COLLISION"
	}
	return false, ""
}

// driveREG021 drives the whitelist raw-value classification case
// (REG-021/062): each raw value the media-player class explicitly recognizes
// classifies to its own designated state, and a raw value the driver has
// never seen before classifies to the class's own unknown_state_fallback —
// never to a blacklisted-away permissive default.
func driveREG021(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REG-021")
	if !ok {
		rep.Fail("REG-021", contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		DeviceClass string   `json:"device_class"`
		RawValues   []string `json:"raw_values"`
	}
	if err := decodeField(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var expected struct {
		ClassifiedStates []string `json:"classified_states"`
	}
	if err := decodeField(c.Expected, &expected); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	reg := deviceclass.Builtin()
	got := make([]string, len(input.RawValues))
	for i, raw := range input.RawValues {
		got[i] = reg.Classify(input.DeviceClass, raw)
	}

	if !reflect.DeepEqual(got, expected.ClassifiedStates) {
		rep.Fail(c.CaseID, contract, "whitelist classification diverged",
			report.Diff{Field: "classified_states", Expected: expected.ClassifiedStates, Actual: got})
		return
	}
	rep.Pass(c.CaseID, contract)
}

// driveREG052 drives command resolution's fail-closed behavior against an
// unknown device_class (DEVICE_CLASS_UNKNOWN) alongside a known-good control:
// a device_command action naming media-player's own launch command resolves,
// while the identical command against a device_class absent from the
// registry does not — REG-052's "unresolved command name MUST fail... never
// a silent no-op" extended to the class reference itself failing to resolve.
func driveREG052(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REG-052")
	if !ok {
		rep.Fail("REG-052", contract, "case not found in frozen corpus")
		return
	}

	var input struct {
		Known struct {
			DeviceClass string `json:"device_class"`
			Command     string `json:"command"`
		} `json:"known"`
		UnknownClass struct {
			DeviceClass string `json:"device_class"`
			Command     string `json:"command"`
		} `json:"unknown_class"`
	}
	if err := decodeField(c.Input, &input); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var expected struct {
		KnownResolves        bool `json:"known_resolves"`
		UnknownClassResolves bool `json:"unknown_class_resolves"`
	}
	if err := decodeField(c.Expected, &expected); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	reg := deviceclass.Builtin()
	gotKnown := reg.CommandExists(input.Known.DeviceClass, input.Known.Command)
	gotUnknownClass := reg.CommandExists(input.UnknownClass.DeviceClass, input.UnknownClass.Command)

	var diffs []report.Diff
	if gotKnown != expected.KnownResolves {
		diffs = append(diffs, report.Diff{Field: "known_resolves", Expected: expected.KnownResolves, Actual: gotKnown})
	}
	if gotUnknownClass != expected.UnknownClassResolves {
		diffs = append(diffs, report.Diff{Field: "unknown_class_resolves", Expected: expected.UnknownClassResolves, Actual: gotUnknownClass})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "command resolution fail-closed behavior diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// decodeField re-marshals a corpus case's generic map[string]any input (or
// any sub-object of it) into a concrete typed value — the driver's own
// fields are JSON-tagged to match the frozen corpus's field names exactly,
// so this is a lossless re-decode, not a schema translation.
func decodeField(m map[string]any, v any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal corpus field: %w", err)
	}
	return json.Unmarshal(b, v)
}

// expectedErrorCodes decodes a structural case's `expected.errors` array
// (each `{code, field, message}`, matching the wider corpus's own
// manifest-1/data-model-1 convention for a Validate-style multi-error
// oracle) into the sorted list of expected codes. An absent errors array
// decodes to an empty, non-nil slice — the same normalized shape
// errorCodes returns for zero actual violations, so a valid:true case with
// no errors key still compares cleanly.
func expectedErrorCodes(c corpus.Case) ([]string, error) {
	raw, ok := c.Expected["errors"]
	if !ok {
		return []string{}, nil
	}
	var entries []struct {
		Code string `json:"code"`
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, err
	}
	codes := make([]string, 0, len(entries))
	for _, e := range entries {
		codes = append(codes, e.Code)
	}
	sort.Strings(codes)
	return codes, nil
}

// errorCodes extracts and sorts the Code of every deviceclass.Error —
// the actual-side counterpart to expectedErrorCodes.
func errorCodes(errs []deviceclass.Error) []string {
	codes := make([]string, 0, len(errs))
	for _, e := range errs {
		codes = append(codes, e.Code)
	}
	sort.Strings(codes)
	return codes
}

func corpusDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "corpora", "device-class-registry")
}

// LoadCorpus loads every frozen device-class-registry corpus case, keyed by
// case_id — the exact set Run itself reads. Exported so the driver's own
// tests can independently verify every case_id in the corpus DIRECTORY is
// accounted for (driven or pending), and so the teeth-test can load the real
// corpus and hand RunCases a deliberately-corrupted copy of one case.
func LoadCorpus() (map[string]corpus.Case, error) {
	return corpus.LoadDir(corpusDir())
}
