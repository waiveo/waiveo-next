// Package manifest1 is the executable manifest/1 conformance driver: the §10
// differential oracle for internal/manifest's PackManifest grammar and its
// Validate entry point. It replays every conformance/corpora/manifest-1 case
// against the LIVE internal/manifest implementation and diffs the actual
// valid/errors[].code outcome against each case's own declared `expected`
// block — the same code-only comparison convention conformance/drivers/corpus
// documents for manifest-1/data-model-1-style Validate oracles (each error is
// a {code, field, message} triple; the code is the stable, corpus-pinned
// identity a driver diffs against).
//
// internal/manifest is a dependency leaf (stdlib-only; it must not import
// internal/deviceclass, internal/events, or internal/rules), so its
// install-time inputs — the host's recognized capability/feature/page-type/
// device-class/provider sets and memory floor — arrive as a plain
// manifest.HostRegistries value, not a live registry object. No corpus case
// embeds its own host fixture, so this driver validates every case against
// defaultHost(), a fixed, documented fixture registry that deliberately
// mirrors internal/manifest's own validate_test.go testHost() fixture (this
// driver cannot import that unexported _test.go helper across a package
// boundary, so the same values are independently declared here).
package manifest1

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/manifest"
)

const contract = "manifest/1"

// Run loads the frozen manifest-1 corpus from disk and drives every case
// against the live internal/manifest.Validate implementation.
func Run() report.Report {
	rep := report.Report{Driver: "manifest1", Target: "internal/manifest"}

	cases, err := LoadCorpus()
	if err != nil {
		rep.Fail("corpus", contract, fmt.Sprintf("load manifest-1 corpus: %v", err))
		return rep
	}
	driveCases(&rep, cases)
	return rep
}

// RunCases drives the identical per-case logic Run uses against an explicit,
// caller-supplied case set. It is the seam TestManifest1DriverHasTeeth uses:
// load the real corpus, corrupt one case's `expected` block in memory, and
// confirm the SAME comparison logic (never a re-implementation of it) reports
// FAIL against the corrupted expectation.
func RunCases(cases map[string]corpus.Case) report.Report {
	rep := report.Report{Driver: "manifest1", Target: "internal/manifest"}
	driveCases(&rep, cases)
	return rep
}

// driveCases drives every case in the frozen manifest-1 corpus: the original
// identity/compat/capability/egress/data-model happy-path and negative
// fixtures, plus the five negative cases this task adds — each turning a
// requirement whose traceability row was "covered" only by a positive
// full-featured fixture into one a genuine failure-path case also exercises.
func driveCases(rep *report.Report, cases map[string]corpus.Case) {
	driveValidateCase(rep, cases, "MAN-001")
	driveValidateCase(rep, cases, "MAN-020")
	driveValidateCase(rep, cases, "MAN-021")
	driveValidateCase(rep, cases, "MAN-030")
	driveValidateCase(rep, cases, "MAN-051")
	driveValidateCase(rep, cases, "MAN-052")
	driveValidateCase(rep, cases, "MAN-054")
	driveValidateCase(rep, cases, "MAN-072")
	driveValidateCase(rep, cases, "MAN-081")
	driveValidateCase(rep, cases, "MAN-090")
	driveValidateCase(rep, cases, "MAN-091")
}

// driveValidateCase drives one manifest-1 case uniformly: decode input into a
// manifest.PackManifest, run manifest.Validate against defaultHost(), and diff
// the resulting valid/errors[].code against the case's declared expectation.
// The same function drives both the happy-path oracles (MAN-001/020/051) and
// every negative case — Validate itself reports every violation uniformly, so
// the driver's comparison does too.
func driveValidateCase(rep *report.Report, cases map[string]corpus.Case, short string) {
	c, ok := corpus.ByID(cases, short)
	if !ok {
		rep.Fail(short, contract, "case not found in frozen corpus")
		return
	}

	var m manifest.PackManifest
	if err := decodeField(c.Input, &m); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	gotErrs := manifest.Validate(m, defaultHost())
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
		rep.Fail(c.CaseID, contract, "manifest validation diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// defaultHost is this driver's documented default fixture registry: the
// host's recognized capability/feature/page-type/device-class/provider sets
// and memory floor every manifest-1 corpus case validates against. It
// deliberately mirrors internal/manifest's own validate_test.go testHost()
// fixture value-for-value (see that file's doc comment for the rationale
// behind each entry) — this driver's cases must behave identically to the
// manifest package's own unit tests against the same host, not against a
// silently-drifted second fixture.
func defaultHost() manifest.HostRegistries {
	return manifest.HostRegistries{
		Capabilities:   map[string]bool{"device.read": true, "egress.http": true, "notifications.send": true},
		Features:       map[string]bool{},
		PageTypes:      map[string]bool{"dashboard": true, "list-detail": true, "settings-form": true},
		DeviceClasses:  map[string]bool{"media-player": true},
		Providers:      map[string]bool{"weather-api/api-key": true},
		BundleFiles:    map[string]bool{"messages/en.json": true},
		MemoryFloorMiB: 32,
	}
}

// decodeField re-marshals a corpus case's generic map[string]any input into a
// concrete typed value — manifest.PackManifest's own JSON tags match the
// frozen corpus's field names exactly, so this is a lossless re-decode, not a
// schema translation.
func decodeField(m map[string]any, v any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal corpus field: %w", err)
	}
	return json.Unmarshal(b, v)
}

// expectedErrorCodes decodes a case's `expected.errors` array (each
// {code, field, message}) into the sorted list of expected codes. An absent
// errors array decodes to an empty, non-nil slice — the same normalized shape
// errorCodes returns for zero actual violations, so a valid:true case with no
// errors key still compares cleanly.
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

// errorCodes extracts and sorts the Code of every manifest.Error — the
// actual-side counterpart to expectedErrorCodes.
func errorCodes(errs []manifest.Error) []string {
	codes := make([]string, 0, len(errs))
	for _, e := range errs {
		codes = append(codes, e.Code)
	}
	sort.Strings(codes)
	return codes
}

func corpusDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "corpora", "manifest-1")
}

// LoadCorpus loads every frozen manifest-1 corpus case, keyed by case_id — the
// exact set Run itself reads. Exported so the driver's own tests can
// independently verify every case_id in the corpus DIRECTORY is accounted for
// (driven or pending), and so the teeth-test can load the real corpus and hand
// RunCases a deliberately-corrupted copy of one case.
func LoadCorpus() (map[string]corpus.Case, error) {
	return corpus.LoadDir(corpusDir())
}
