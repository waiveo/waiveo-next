package auth

import (
	"context"
	"testing"
)

// console_test.go covers the console binding's admission rule and verb policy
// where the FROZEN CORPUS does not reach — most importantly SEC-072's REFUSAL
// half, which the corpus never exercises because it carries no `peer_uid: 1000`
// case, and the full SEC-075 verb enumeration rather than the single member the
// corpus case names.

// TestConsoleRefusesANonRootPeer is SEC-072's REFUSAL half — the half the
// frozen corpus never exercises, since it carries no non-root case.
//
// Two properties are asserted, and the second is the one an implementation is
// most likely to get wrong: the refusal carries NO response body ("refused at
// accept time with no response body ... logged, not surfaced to the rejected
// peer beyond connection closure"), and it happens BEFORE the verb is
// considered — a non-root peer naming an allowed verb and one naming a
// forbidden verb must be refused identically, or the refusal itself becomes an
// oracle for the verb set.
func TestConsoleRefusesANonRootPeer(t *testing.T) {
	ctx := context.Background()
	st := newSecurityTestStore(t)
	console := NewConsole(st, nil, nil)

	for _, verb := range []string{ConsoleVerbServiceStatus, "workspace.query", ""} {
		resp := console.Dispatch(ctx, 1000, ConsoleRequest{Verb: verb})
		if resp.Admitted {
			t.Errorf("uid 1000 was admitted for verb %q (SEC-072)", verb)
		}
		if resp.Code != ErrCodeConsolePeerNotRoot {
			t.Errorf("uid 1000 / verb %q refused with %q, want %q — the uid check must precede the verb check, or the refusal code leaks the verb set",
				verb, resp.Code, ErrCodeConsolePeerNotRoot)
		}
		if resp.Body != nil {
			t.Errorf("uid 1000 / verb %q carried a response body %v; SEC-072 refuses with none", verb, resp.Body)
		}
		if resp.PrincipalKind != "" {
			t.Errorf("uid 1000 / verb %q was attributed to %q; a refused peer establishes no principal", verb, resp.PrincipalKind)
		}
	}
}

// TestConsoleAdmitsOnlyItsEnumeratedVerbs walks SEC-075's whole set rather than
// the single member the corpus case names, so adding a verb to the enumeration
// without an implementation, or removing one, is caught here.
func TestConsoleAdmitsOnlyItsEnumeratedVerbs(t *testing.T) {
	ctx := context.Background()
	st := newSecurityTestStore(t)
	console := NewConsole(st, nil, nil)

	for _, verb := range ConsoleVerbs() {
		resp := console.Dispatch(ctx, 0, ConsoleRequest{Verb: verb})
		if !resp.Admitted {
			t.Errorf("enumerated verb %q was not admitted (%s)", verb, resp.Code)
		}
		if resp.Code == ErrCodeConsoleVerbNotAllowed {
			t.Errorf("enumerated verb %q was refused as not-allowed", verb)
		}
	}
	for _, verb := range []string{"workspace.query", "scope-nodes.list", "shell", ""} {
		resp := console.Dispatch(ctx, 0, ConsoleRequest{Verb: verb})
		if resp.Admitted || resp.Executed {
			t.Errorf("unlisted verb %q was admitted/executed (SEC-075)", verb)
		}
		if resp.Code != ErrCodeConsoleVerbNotAllowed {
			t.Errorf("unlisted verb %q refused with %q, want %q", verb, resp.Code, ErrCodeConsoleVerbNotAllowed)
		}
	}
}

// newSecurityTestStore opens an ephemeral auth store on a fixed clock and a
// deterministic id source.
func newSecurityTestStore(t *testing.T) *Store {
	t.Helper()
	// 24 Crockford-base32 symbols (the alphabet excludes I, L, O and U, so the
	// mnemonic is spelled without them) plus the two-symbol counter below.
	const prefix = "01J8Z9AVTHCNSE0F1XTV0000"
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	n := 0
	st, err := Open(":memory:", func() int64 { return 1752537600000 }, func() string {
		hi := alphabet[(n/32)%32]
		lo := alphabet[n%32]
		n++
		return prefix + string([]byte{hi, lo})
	})
	if err != nil {
		t.Fatalf("auth.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// testIDs mints deterministic, ascending, valid ULIDs for a test store — the
// same shape newSecurityTestStore uses, exposed so a test that also needs an
// Auditor draws its record ids from one source rather than two.
func testIDs() func() string {
	const prefix = "01J8Z9AVTHCNSE0F1XTV0000"
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	n := 0
	return func() string {
		hi := alphabet[(n/32)%32]
		lo := alphabet[n%32]
		n++
		return prefix + string([]byte{hi, lo})
	}
}

// newAuditedTestStore is newSecurityTestStore with an events sink wired in, for
// the assertions about SEC-034's mandatory grant records — which a store opened
// without one does not emit, deliberately.
func newAuditedTestStore(t *testing.T, sink EventSink) (*Store, *Auditor) {
	t.Helper()
	ids := testIDs()
	clock := func() int64 { return 1752537600000 }
	auditor := NewAuditor(sink, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", clock, ids, nil)
	st, err := Open(":memory:", clock, ids, WithAuditor(auditor))
	if err != nil {
		t.Fatalf("auth.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, auditor
}
