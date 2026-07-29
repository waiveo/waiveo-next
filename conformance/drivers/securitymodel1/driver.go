// Package securitymodel1 is the executable security-model/1 conformance
// driver: the §10 differential oracle for the platform's grant lifecycle, its
// routine credential-reset flow, the app-tier clock floor and its accuracy
// assessment, the console binding's admission rule and verb policy, the
// first-boot claim window, and factory-reset key-material destruction.
//
// It replays every case frozen under conformance/corpora/security-model
// against LIVE internal/app code and diffs the observed behavior against each
// case's own `expected` block:
//
//   - SEC-035 and SEC-120 drive the real grant store and the real, HTTP-mounted
//     first-boot claim route (internal/app/api's mux, POST /api/v1/auth/setup);
//   - SEC-121 drives the real, HTTP-mounted data-subject delete operation
//     (POST /api/v1/workspace/delete) to completion on a real JobRunner and
//     then inspects the key material it was supposed to destroy;
//   - SEC-050, SEC-066, SEC-067, SEC-072 and SEC-075 drive
//     internal/app/auth components directly, because no HTTP or socket surface
//     exists for them in this tree. That is a real gap, stated here rather than
//     hidden: the credential-reset flow has no route and no CLI, and the
//     console binding has no Unix-domain-socket listener (SEC-070/071) — only
//     the admission rule and verb policy the corpus can exercise.
//
// # Two hard rules this driver holds to
//
// NO WALL-CLOCK SLEEP. Every timing-dependent case (a grant `ttl` elapsing, a
// clock floor surviving a restart) moves an INJECTED clock variable, which is
// exactly what contracts/security-model.md's own Conformance notes call for:
// "Lockout backoff timing (SEC-090) and grant `ttl` expiry (SEC-032, SEC-035)
// are timing-dependent and exercised against an injectable clock in a driver
// harness, not wall-clock sleeps."
//
// NO REAL SOCKET. The console cases pass `peer_uid` in as an injected input,
// which the same Conformance notes bless outright: "conformance cases model
// them as a given input (`peer_uid: 0` or `peer_uid: 1000`) exactly as api/1's
// own corpus treats an authenticated principal as a given — the driver harness
// that actually opens a Unix socket and checks SO_PEERCRED is a
// systemd-install-smoke-lane concern, not this static corpus's."
package securitymodel1

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
)

const contract = "security-model/1"

// target names the implementation-under-test this driver drove.
const target = "internal/app/auth (grants, clock floor, console binding, credential reset) + internal/app/api (POST /api/v1/auth/setup, POST /api/v1/workspace/delete) + internal/app/workspacekey"

// Run loads the frozen security-model corpus from disk and drives every case
// against the live implementation.
func Run() report.Report {
	rep := report.Report{Driver: "securitymodel1", Target: target}
	cases, err := LoadCorpus()
	if err != nil {
		rep.Fail("corpus", contract, fmt.Sprintf("load security-model corpus: %v", err))
		return rep
	}
	driveCases(&rep, cases)
	return rep
}

// RunCases drives the identical per-case logic Run uses against an explicit,
// caller-supplied case set — the seam the teeth-tests use: load the real
// corpus, corrupt one case's `expected` block in memory, and confirm the SAME
// comparison logic (never a re-implementation of it) reports FAIL.
func RunCases(cases map[string]corpus.Case) report.Report {
	rep := report.Report{Driver: "securitymodel1", Target: target}
	driveCases(&rep, cases)
	return rep
}

// corpusDir resolves the frozen corpus directory from this source file's own
// location, so the driver is correct whatever the caller's working directory is
// (`go run` from the repo root, `go test` from the package directory).
func corpusDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "corpora", "security-model")
}

// LoadCorpus loads every frozen security-model case.
func LoadCorpus() (map[string]corpus.Case, error) { return corpus.LoadDir(corpusDir()) }

// drivers maps each frozen case's stable short id to the function that drives
// it. It is a map from SHORT id (never the descriptive full filename stem), so
// renaming a case file's tail does not silently unwire it.
var drivers = map[string]func(*report.Report, corpus.Case){
	"SEC-035": driveGrantExpired,
	"SEC-050": driveCredentialReset,
	"SEC-066": driveClockFloorSurvivesRestart,
	"SEC-067": driveClockFloorAdvanceGate,
	"SEC-072": driveConsoleAdmission,
	"SEC-075": driveConsoleVerbNotAllowed,
	"SEC-120": driveFirstBootClaimOutsideWindow,
	"SEC-121": driveFactoryReset,
}

// pendingCaseIDs are cases frozen under conformance/corpora/security-model that
// this driver deliberately does NOT execute, each with the reason §10's "no
// silent caps" rule requires.
//
// It is EMPTY: every frozen case is driven. The map stays rather than being
// deleted so the next undrivable case has somewhere to be recorded WITH a
// reason, instead of a mechanism having to be rebuilt before one can be.
//
// "Driven" is not "fully asserted", and this driver does not let the two blur:
// a case whose `expected` block names something the live surface cannot observe
// is driven for what IS observable and records the rest as an explicit Note on
// its own report row (report.Case.Notes). SEC-121 is the one that does so, and
// its notes are why several SEC-121-adjacent traceability rows stay TBD.
var pendingCaseIDs = map[string]string{}

func driveCases(rep *report.Report, cases map[string]corpus.Case) {
	shorts := make([]string, 0, len(drivers))
	for short := range drivers {
		shorts = append(shorts, short)
	}
	sort.Strings(shorts)
	for _, short := range shorts {
		c, ok := corpus.ByID(cases, short)
		if !ok {
			rep.Fail(short, contract, "case not found in frozen corpus")
			continue
		}
		drivers[short](rep, c)
	}
	for _, short := range sortedKeys(pendingCaseIDs) {
		c, ok := corpus.ByID(cases, short)
		if !ok {
			rep.Fail(short, contract, "case not found in frozen corpus")
			continue
		}
		rep.Pending(c.CaseID, contract, pendingCaseIDs[short])
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- the differential oracle's comparison side -------------------------------

// check accumulates one case's expected-vs-actual comparisons. Every assertion
// reads its EXPECTED value out of the frozen corpus case — never from a literal
// in this driver — which is what makes this an oracle rather than a second
// implementation of the contract.
//
// A field the corpus does not carry is a DIFF, not a skip: a case whose
// expectation the driver silently failed to find would report a pass for having
// asserted nothing, which is the exact failure mode §10's "no silent caps"
// discipline exists to prevent.
type check struct {
	c     corpus.Case
	diffs []report.Diff
	notes []string
}

func newCheck(c corpus.Case) *check { return &check{c: c} }

// note records an expected field the live surface genuinely cannot observe, so
// it was NOT asserted. It is the honest alternative to asserting a vacuous
// truth, and it is surfaced on the report row a reader sees.
func (k *check) note(format string, args ...any) {
	k.notes = append(k.notes, fmt.Sprintf(format, args...))
}

func (k *check) diff(field string, expected, actual any) {
	k.diffs = append(k.diffs, report.Diff{Field: field, Expected: expected, Actual: actual})
}

// boolAt compares the corpus's expected boolean at path against actual.
func (k *check) boolAt(path string, actual bool) {
	v, ok := k.c.Expect(path)
	if !ok {
		k.diff(path, "a boolean in the frozen expected block", "absent")
		return
	}
	want, ok := v.(bool)
	if !ok {
		k.diff(path, "a boolean", fmt.Sprintf("%T", v))
		return
	}
	if want != actual {
		k.diff(path, want, actual)
	}
}

// stringAt compares the corpus's expected string at path against actual.
func (k *check) stringAt(path string, actual string) {
	v, ok := k.c.Expect(path)
	if !ok {
		k.diff(path, "a string in the frozen expected block", "absent")
		return
	}
	want, ok := v.(string)
	if !ok {
		k.diff(path, "a string", fmt.Sprintf("%T", v))
		return
	}
	if want != actual {
		k.diff(path, want, actual)
	}
}

// intAt compares the corpus's expected number at path against actual. JSON
// numbers decode as float64; a millisecond epoch is well inside float64's exact
// integer range, so the conversion is lossless for every value this corpus
// carries.
func (k *check) intAt(path string, actual int64) {
	v, ok := k.c.Expect(path)
	if !ok {
		k.diff(path, "a number in the frozen expected block", "absent")
		return
	}
	f, ok := v.(float64)
	if !ok {
		k.diff(path, "a number", fmt.Sprintf("%T", v))
		return
	}
	if want := int64(f); want != actual {
		k.diff(path, want, actual)
	}
}

// nullAt asserts the corpus expects an explicit null at path (the SEC-072
// case's `"error": null`), and that the live outcome carried none — actual is
// the observed error code, "" for none.
func (k *check) nullAt(path string, actual string) {
	v, present := k.c.Expect(path)
	if !present {
		k.diff(path, "an explicit null in the frozen expected block", "absent")
		return
	}
	if v != nil {
		k.diff(path, "null", v)
		return
	}
	if actual != "" {
		k.diff(path, nil, actual)
	}
}

// fail records a driver-side failure that stopped the case from being driven at
// all (a fixture that could not be built). It is deliberately a FAIL rather than
// a PENDING: pending means "deliberately not driven", and a harness that broke
// is not a deliberate deferral.
func (k *check) fail(rep *report.Report, format string, args ...any) {
	rep.Fail(k.c.CaseID, contract, fmt.Sprintf(format, args...), k.diffs...)
}

// finish records the case's outcome: PASS when every compared field matched,
// FAIL with the full diff set otherwise.
func (k *check) finish(rep *report.Report) {
	if len(k.diffs) == 0 {
		rep.Pass(k.c.CaseID, contract, k.notes...)
		return
	}
	rep.Add(report.Case{
		CaseID: k.c.CaseID, Contract: contract, Status: report.FAIL,
		Reason: fmt.Sprintf("%d expected field(s) diverged from the live implementation", len(k.diffs)),
		Diffs:  k.diffs, Notes: k.notes,
	})
}

// --- input readers -----------------------------------------------------------

// inputString reads a dotted path out of a case's `input` block as a string.
func inputString(c corpus.Case, path string) (string, bool) {
	v, ok := digInput(c, path)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// inputInt reads a dotted path out of a case's `input` block as an int64.
func inputInt(c corpus.Case, path string) (int64, bool) {
	v, ok := digInput(c, path)
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

// inputStrings reads a dotted path out of a case's `input` block as a string
// slice (SEC-050's `argv_capture`).
func inputStrings(c corpus.Case, path string) ([]string, bool) {
	v, ok := digInput(c, path)
	if !ok {
		return nil, false
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// digInput walks a dotted path through a case's `input` block. It reuses the
// case's own Expect machinery by constructing an equivalent walk, since
// corpus.Case exposes a reader for `expected` only.
func digInput(c corpus.Case, path string) (any, bool) {
	cur := any(c.Input)
	for _, key := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[key]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(key)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// inputTimeMs reads an RFC3339 timestamp out of a case's `input` block as epoch
// milliseconds — the unit every clock in internal/app/auth is injected in.
func inputTimeMs(c corpus.Case, path string) (int64, bool) {
	s, ok := inputString(c, path)
	if !ok {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// inputDurationMs reads an ISO-8601 duration out of a case's `input` block as
// milliseconds. It handles the `PT<n>H<n>M<n>S` forms the corpora actually use
// (the grant `ttl` SEC-032 talks about); a date-component duration (`P1D`) is
// refused rather than silently mis-parsed, since no case carries one and a
// wrong ttl would make a timing case pass or fail for the wrong reason.
func inputDurationMs(c corpus.Case, path string) (int64, bool) {
	s, ok := inputString(c, path)
	if !ok || !strings.HasPrefix(s, "PT") {
		return 0, false
	}
	var (
		total int64
		num   strings.Builder
	)
	for _, r := range s[2:] {
		switch {
		case r >= '0' && r <= '9':
			num.WriteRune(r)
		case r == 'H' || r == 'M' || r == 'S':
			n, err := strconv.ParseInt(num.String(), 10, 64)
			if err != nil {
				return 0, false
			}
			num.Reset()
			switch r {
			case 'H':
				total += n * 3600_000
			case 'M':
				total += n * 60_000
			case 'S':
				total += n * 1000
			}
		default:
			return 0, false
		}
	}
	if num.Len() != 0 || total <= 0 {
		return 0, false
	}
	return total, true
}

// deterministicIDs mints deterministic, ascending, valid 26-character ULIDs
// (DAT-005a) — the injected id source every store in this driver draws from, so
// a case's outcome is reproducible rather than dependent on a fresh random
// ulid.New() per run. `pinned` is emitted first, in order, for the cases whose
// own frozen input names an id the live store must actually mint (SEC-072's
// `session_id`).
func deterministicIDs(pinned ...string) func() string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	queue := append([]string(nil), pinned...)
	n := 0
	return func() string {
		if len(queue) > 0 {
			out := queue[0]
			queue = queue[1:]
			return out
		}
		hi := alphabet[(n/32)%32]
		lo := alphabet[n%32]
		n++
		return idPrefix + string([]byte{hi, lo})
	}
}

// idPrefix is the 24-symbol Crockford-base32 stem every generated id in this
// driver carries; the two-symbol counter deterministicIDs appends completes a
// 26-character ULID (DAT-005a). The alphabet excludes I, L, O and U, so the
// mnemonic is spelled without them — "SECMDRV" rather than "SECMODEL", which is
// not a typo.
const idPrefix = "01J8Z9DRVSECMDRV1CASE000"
