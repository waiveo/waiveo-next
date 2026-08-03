package origin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
)

// These drive the ENFORCEMENT half through the real http.Handler rather than
// through contenturl.Verify directly. That distinction is the point: the
// package-level tests prove the signature scheme is sound, and these prove the
// origin actually consults it before serving a byte. A scheme that verifies
// correctly and a handler that never calls it is the exact defect shape this
// work exists to close.

const signedTestKey = "a-32-byte-test-key-for-hmac-0001"

// signedOrigin returns a Store requiring signed URLs, holding body, at a fixed
// clock — plus the digest the content is addressed by.
func signedOrigin(t *testing.T, nowMs int64, body []byte) (*Store, string) {
	t.Helper()
	s := New(WithSigningKey([]byte(signedTestKey)), WithClock(func() int64 { return nowMs }))
	assetRef, err := s.Add(body)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return s, strings.TrimPrefix(assetRef, "sha256:")
}

// get issues one request against the store's handler and returns the response.
func get(t *testing.T, s *Store, target string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Result()
}

// TestASignedFetchIsServed is the control. Every refusal below is satisfied by
// a handler that refuses everything, so the accept path is pinned first.
func TestASignedFetchIsServed(t *testing.T) {
	const now, exp = int64(1_700_000_000_000), int64(1_700_000_060_000)
	body := []byte("the actual content bytes")
	s, digest := signedOrigin(t, now, body)

	signed, err := contenturl.URL("", []byte(signedTestKey), digest, exp)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	resp := get(t, s, signed)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a correctly signed, unexpired fetch got %d, want 200", resp.StatusCode)
	}
	got := make([]byte, len(body))
	if n, _ := resp.Body.Read(got); n != len(body) || string(got) != string(body) {
		t.Errorf("served body = %q, want %q", got[:n], body)
	}
}

// TestAnUnsignedFetchIsRefused is the defect this closes, stated plainly: the
// bare content-addressed URL that used to work must no longer work.
func TestAnUnsignedFetchIsRefused(t *testing.T) {
	const now = int64(1_700_000_000_000)
	s, digest := signedOrigin(t, now, []byte("the actual content bytes"))

	resp := get(t, s, "/content/"+digest)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an UNSIGNED fetch of a held asset got %d, want 403 — a bare content-addressed URL is a permanent, "+
			"unrevocable read capability for anyone who has ever seen the digest", resp.StatusCode)
	}
}

// TestAnExpiredSignatureIsRefused: the deadline is enforced by the origin's own
// clock, not by the client's good behaviour.
func TestAnExpiredSignatureIsRefused(t *testing.T) {
	const exp = int64(1_700_000_060_000)
	body := []byte("the actual content bytes")

	// Minted once; presented to two origins that differ ONLY in what time they
	// think it is. Same URL, same key, same asset — so the clock is provably
	// the thing deciding, rather than any difference in the request.
	live, digest := signedOrigin(t, exp, body)
	signed, err := contenturl.URL("", []byte(signedTestKey), digest, exp)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if resp := get(t, live, signed); resp.StatusCode != http.StatusOK {
		t.Fatalf("at the deadline instant the fetch got %d, want 200", resp.StatusCode)
	}
	stale, _ := signedOrigin(t, exp+1, body)
	if resp := get(t, stale, signed); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("one millisecond past the deadline the SAME url got %d, want 403", resp.StatusCode)
	}
}

// TestATamperedDeadlineIsRefused: a holder cannot extend its own capability by
// editing the query it was handed.
func TestATamperedDeadlineIsRefused(t *testing.T) {
	const now, exp = int64(1_700_000_000_000), int64(1_700_000_060_000)
	s, digest := signedOrigin(t, now, []byte("the actual content bytes"))

	signed, err := contenturl.URL("", []byte(signedTestKey), digest, exp)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	q.Set(contenturl.QueryExpires, strconv.FormatInt(exp+86_400_000, 10))
	u.RawQuery = q.Encode()

	if resp := get(t, s, u.String()); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a deadline extended by a day got %d, want 403", resp.StatusCode)
	}
}

// TestASignatureForOneAssetDoesNotFetchAnother: the capability names ONE asset.
//
// Both assets are really held by this origin, so a 403 here cannot be an
// accidental 404 wearing a different number — the only reason to refuse is the
// binding between digest and signature.
func TestASignatureForOneAssetDoesNotFetchAnother(t *testing.T) {
	const now, exp = int64(1_700_000_000_000), int64(1_700_000_060_000)
	s, digestA := signedOrigin(t, now, []byte("asset A"))
	refB, err := s.Add([]byte("asset B"))
	if err != nil {
		t.Fatalf("Add B: %v", err)
	}
	digestB := strings.TrimPrefix(refB, "sha256:")

	signedA, err := contenturl.Sign([]byte(signedTestKey), digestA, exp)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if resp := get(t, s, "/content/"+digestB+"?"+signedA); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("asset A's signature presented for asset B (both held) got %d, want 403", resp.StatusCode)
	}
}

// TestAnUnsignedRequestCannotProbeWhichAssetsExist pins the check ORDER at the
// handler, which is a distinct property from contenturl's internal ordering.
//
// If the store were consulted first, an unsigned request would return 404 for
// an absent digest and 403 for a held one — making the pair of responses a
// presence oracle over every asset this origin holds, readable by anyone, with
// no signature required to operate it.
func TestAnUnsignedRequestCannotProbeWhichAssetsExist(t *testing.T) {
	const now = int64(1_700_000_000_000)
	s, held := signedOrigin(t, now, []byte("the actual content bytes"))
	const absent = "0000000000000000000000000000000000000000000000000000000000000000"
	if s.Has(absent) {
		t.Fatal("the digest chosen as absent is actually held; this test would prove nothing")
	}

	heldStatus := get(t, s, "/content/"+held).StatusCode
	absentStatus := get(t, s, "/content/"+absent).StatusCode
	if heldStatus != absentStatus {
		t.Errorf("an unsigned fetch answers %d for a HELD asset and %d for an absent one — the difference is a presence "+
			"oracle over the whole store, operable by anyone with no signature at all", heldStatus, absentStatus)
	}
}

// TestWithoutAKeyTheOriginServesUnsigned pins the documented default, so the
// option's off-state is a decision on the record rather than an accident. It is
// what keeps the relay's REL-066 schedule-resolved URLs — which no key can
// sign today — fetchable until that half lands.
func TestWithoutAKeyTheOriginServesUnsigned(t *testing.T) {
	s := New()
	assetRef, err := s.Add([]byte("the actual content bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	digest := strings.TrimPrefix(assetRef, "sha256:")
	if resp := get(t, s, "/content/"+digest); resp.StatusCode != http.StatusOK {
		t.Fatalf("a keyless origin refused an unsigned fetch with %d; the default must stay unsigned until every "+
			"URL-constructing party can sign", resp.StatusCode)
	}
}

// TestAnEmptyKeyDoesNotEnableEnforcement: WithSigningKey ignores an empty key
// rather than storing it.
//
// The failure it prevents is quiet and total. HMAC under a zero-length key is
// well-formed and reproducible by anyone, so a deployment that computed its key
// and got nothing back would look enforced — 403 for the unsigned, 200 for the
// "signed" — while accepting a signature any caller can compute for themselves.
func TestAnEmptyKeyDoesNotEnableEnforcement(t *testing.T) {
	for _, key := range [][]byte{nil, {}} {
		s := New(WithSigningKey(key))
		assetRef, err := s.Add([]byte("the actual content bytes"))
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		digest := strings.TrimPrefix(assetRef, "sha256:")
		if resp := get(t, s, "/content/"+digest); resp.StatusCode != http.StatusOK {
			t.Errorf("a %d-byte key produced status %d; an empty key must leave enforcement OFF, never on against a "+
				"key that authenticates every forgery", len(key), resp.StatusCode)
		}
	}
}
