package contenturl

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

var (
	testKey  = []byte("a-32-byte-test-key-for-hmac-0001")
	otherKey = []byte("a-32-byte-test-key-for-hmac-0002")
)

const (
	digestA = "cafebabe0123456789abcdef"
	digestB = "deadbeef0123456789abcdef"
)

// mustSign signs and parses in one step, since every verification test needs
// the query as url.Values rather than as the encoded string.
func mustSign(t *testing.T, key []byte, digest string, exp int64) url.Values {
	t.Helper()
	raw, err := Sign(key, digest, exp)
	if err != nil {
		t.Fatalf("Sign(%q, %d): %v", digest, exp, err)
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", raw, err)
	}
	return q
}

// TestAFreshlyMintedURLVerifies is the control, and it comes first
// deliberately: every refusal below is satisfied by an implementation that
// refuses everything, so the accept path has to be pinned or the whole file
// proves nothing.
func TestAFreshlyMintedURLVerifies(t *testing.T) {
	const exp = 1_800_000_000_000
	if err := Verify(testKey, digestA, mustSign(t, testKey, digestA, exp), exp-1); err != nil {
		t.Fatalf("a freshly minted, unexpired URL was refused: %v", err)
	}
}

// TestTheDeadlineIsInclusiveAtItsOwnInstant drives the boundary the doc
// commits to: valid THROUGH exp, expired strictly after.
//
// The two comparisons differ by one millisecond and by nothing else, so a test
// that only checked "expired long after" and "valid long before" would pass
// under either — which is how a TTL silently loses its last instant.
func TestTheDeadlineIsInclusiveAtItsOwnInstant(t *testing.T) {
	const exp = 1_800_000_000_000
	q := mustSign(t, testKey, digestA, exp)
	for _, tc := range []struct {
		name    string
		nowMs   int64
		wantErr bool
	}{
		{"one ms before the deadline", exp - 1, false},
		{"exactly at the deadline", exp, false},
		{"one ms after the deadline", exp + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(testKey, digestA, q, tc.nowMs)
			if tc.wantErr && !errors.Is(err, ErrExpired) {
				t.Fatalf("at now=%d (deadline %d) want ErrExpired, got %v", tc.nowMs, exp, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("at now=%d (deadline %d) want accepted, got %v", tc.nowMs, exp, err)
			}
		})
	}
}

// TestASignatureForOneDigestDoesNotAuthorizeAnother is the property the whole
// package exists for: a capability is for ONE asset.
//
// Asserted as a REASON and not merely as `err != nil` — a digest swap that was
// refused as malformed, or as expired, would satisfy a bare non-nil check while
// leaving the binding between digest and signature entirely unenforced.
func TestASignatureForOneDigestDoesNotAuthorizeAnother(t *testing.T) {
	const exp = 1_800_000_000_000
	q := mustSign(t, testKey, digestA, exp)
	if err := Verify(testKey, digestB, q, exp-1); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("digest A's signature presented for digest B: want ErrSignatureInvalid, got %v", err)
	}
}

// TestExtendingTheDeadlineInvalidatesTheSignature: exp is inside the MAC, so a
// client cannot buy itself more time by editing the query it was handed.
//
// Without this, `exp` would be an advisory number the origin reads back from
// the attacker — the exact shape of an expiry that exists on the wire and
// enforces nothing.
func TestExtendingTheDeadlineInvalidatesTheSignature(t *testing.T) {
	const exp = 1_800_000_000_000
	q := mustSign(t, testKey, digestA, exp)
	q.Set(QueryExpires, strconv.FormatInt(exp+86_400_000, 10)) // +1 day
	if err := Verify(testKey, digestA, q, exp+1000); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("a deadline extended by the holder: want ErrSignatureInvalid, got %v", err)
	}
}

// TestAnotherKeysSignatureIsRefused covers rotation and cross-deployment
// replay: a URL minted by a different origin is not a URL this one honours.
func TestAnotherKeysSignatureIsRefused(t *testing.T) {
	const exp = 1_800_000_000_000
	q := mustSign(t, otherKey, digestA, exp)
	if err := Verify(testKey, digestA, q, exp-1); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("a signature minted under a different key: want ErrSignatureInvalid, got %v", err)
	}
}

// TestTheSignatureIsCheckedBeforeTheDeadline pins the ORDER the doc commits
// to, which is a security property rather than a stylistic one.
//
// Reversed, an unsigned request bearing an invented past deadline would be
// answered "expired" — confirming to an attacker that everything except
// freshness was acceptable, and turning the origin into an oracle for which
// digests it holds. The test presents a URL that is BOTH unsigned-garbage and
// past its deadline: only the check order decides which error comes back.
func TestTheSignatureIsCheckedBeforeTheDeadline(t *testing.T) {
	q := url.Values{}
	q.Set(QueryExpires, "1000")
	q.Set(QuerySignature, strings.Repeat("ab", 32))
	err := Verify(testKey, digestA, q, 2000)
	if errors.Is(err, ErrExpired) {
		t.Fatal("an UNSIGNED request past an invented deadline was answered 'expired' — the deadline is being judged " +
			"before the signature, which tells a caller their forged digest and deadline were otherwise fine")
	}
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("want ErrSignatureInvalid, got %v", err)
	}
}

// TestADigestCarryingTheStatementSeparatorIsRefused is the collision guard.
//
// The signed statement is newline-joined, so its unambiguity rests entirely on
// no field being able to contain a newline — and the digest arrives from the
// request path, where an attacker writes every byte. The refusal must land at
// the GRAMMAR (ErrMalformed), before the bytes reach the MAC.
func TestADigestCarryingTheStatementSeparatorIsRefused(t *testing.T) {
	for _, bad := range []string{
		"cafe\n1800000000000",      // a digest that continues the statement
		statementDomain + "\ncafe", // a digest that restarts it
		"CAFEBABE",                 // uppercase: a second spelling of one digest
		"cafe babe",                // whitespace
		"",                         // empty
		"cafeg0",                   // not hex at all
		"cafe%0a1800000000000",     // percent-encoding, already decoded by the mux
	} {
		t.Run(strconv.Quote(bad), func(t *testing.T) {
			if _, err := Sign(testKey, bad, 1_800_000_000_000); !errors.Is(err, ErrMalformed) {
				t.Errorf("Sign accepted digest %q (want ErrMalformed, got %v)", bad, err)
			}
			q := mustSign(t, testKey, digestA, 1_800_000_000_000)
			if err := Verify(testKey, bad, q, 1); !errors.Is(err, ErrMalformed) {
				t.Errorf("Verify accepted digest %q (want ErrMalformed, got %v)", bad, err)
			}
		})
	}
}

// TestAnEmptyKeyIsRefusedRatherThanUsed: HMAC under a zero-length key is
// well-formed and reproducible by anyone, so an unset key must fail loudly on
// BOTH sides. A Verify that tolerated it would authenticate every forgery, and
// a Sign that tolerated it would hand out URLs that say nothing.
func TestAnEmptyKeyIsRefusedRatherThanUsed(t *testing.T) {
	for _, key := range [][]byte{nil, {}} {
		if _, err := Sign(key, digestA, 1_800_000_000_000); !errors.Is(err, ErrNoKey) {
			t.Errorf("Sign with a %d-byte key: want ErrNoKey, got %v", len(key), err)
		}
		if err := Verify(key, digestA, url.Values{}, 1); !errors.Is(err, ErrNoKey) {
			t.Errorf("Verify with a %d-byte key: want ErrNoKey, got %v", len(key), err)
		}
	}
}

// TestANonPositiveExpiryIsRefused: 0 is the value this codebase stamped on
// every content reference to mean "no expiry policy is defined yet". Signing
// one would launder that inert value into something that reads as a deliberate
// deadline, so the mint refuses it outright rather than let it flow.
func TestANonPositiveExpiryIsRefused(t *testing.T) {
	for _, exp := range []int64{0, -1, -1_800_000_000_000} {
		if _, err := Sign(testKey, digestA, exp); !errors.Is(err, ErrMalformed) {
			t.Errorf("Sign with expiry %d: want ErrMalformed, got %v", exp, err)
		}
	}
}

// TestAMissingOrUnparseableParameterIsMalformedNotAForgery keeps the two
// refusals apart. A URL with no signature at all has not FAILED verification —
// nothing was verified — and reporting it as a forgery would bury real
// tampering in a pile of ordinary malformed requests.
func TestAMissingOrUnparseableParameterIsMalformedNotAForgery(t *testing.T) {
	good := mustSign(t, testKey, digestA, 1_800_000_000_000)
	for _, tc := range []struct {
		name string
		q    url.Values
	}{
		{"no parameters at all", url.Values{}},
		{"signature but no expiry", url.Values{QuerySignature: {good.Get(QuerySignature)}}},
		{"expiry but no signature", url.Values{QueryExpires: {good.Get(QueryExpires)}}},
		{"expiry is not a number", url.Values{QueryExpires: {"soon"}, QuerySignature: {good.Get(QuerySignature)}}},
		{"expiry overflows int64", url.Values{QueryExpires: {"99999999999999999999"}, QuerySignature: {good.Get(QuerySignature)}}},
		{"signature is not hex", url.Values{QueryExpires: {good.Get(QueryExpires)}, QuerySignature: {"zzzz"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(testKey, digestA, tc.q, 1); !errors.Is(err, ErrMalformed) {
				t.Errorf("want ErrMalformed, got %v", err)
			}
		})
	}
}

// TestURLKeepsTheContractsPathGrammar: the signature rides the QUERY, so the
// `<base>/content/<hex>` path REL-061 defines is unchanged and every existing
// consumer that parses it still reads.
func TestURLKeepsTheContractsPathGrammar(t *testing.T) {
	const exp = 1_800_000_000_000
	got, err := URL("https://origin.example", testKey, digestA, exp)
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	wantPrefix := "https://origin.example/content/" + digestA + "?"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("minted URL %q does not keep the contract's path grammar %q", got, wantPrefix)
	}
	// And what it produced must verify through the ordinary parse path a
	// server takes, rather than only through the values Sign happened to hold.
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("Parse(%q): %v", got, err)
	}
	digest := strings.TrimPrefix(u.Path, "/content/")
	if err := Verify(testKey, digest, u.Query(), exp); err != nil {
		t.Fatalf("a URL this package minted did not verify after a round trip through url.Parse: %v", err)
	}
}

// TestSignIsDeterministic: the same inputs produce the same URL, so a caller
// that re-mints an unchanged reference does not perturb the snapshot bytes —
// and therefore does not perturb the REL-053 `hash` those bytes feed.
func TestSignIsDeterministic(t *testing.T) {
	const exp = 1_800_000_000_000
	first, err := Sign(testKey, digestA, exp)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	second, err := Sign(testKey, digestA, exp)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if first != second {
		t.Errorf("Sign is not deterministic:\n first: %s\nsecond: %s", first, second)
	}
}
