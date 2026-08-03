package schedulehost

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
)

// The property this whole delivery path exists for: a URL the RELAY mints is a
// URL the ORIGIN accepts.
//
// Every other test on either side proves one half against its own idea of the
// other. The relay's tests assert it produces a signed-looking URL; the origin's
// assert it accepts one contenturl minted. Neither would catch the two disagreeing
// about the key encoding, the query parameter names, or which bytes are signed —
// and a disagreement there is silent until a screen fetches, at which point every
// fetch on the site 403s with nothing saying why.

const mintedTestKey = "a-32-byte-test-key-for-hmac-0001"

// TestARelayMintedURLVerifiesAtTheOrigin drives both halves against each other.
func TestARelayMintedURLVerifiesAtTheOrigin(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	body := []byte("the actual content bytes")

	store := origin.New(
		origin.WithSigningKey([]byte(mintedTestKey)),
		origin.WithClock(func() int64 { return nowMs }),
	)
	assetRef, err := store.Add(body)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The relay's own minting seam, with the key as it arrives from an applied
	// snapshot.
	sign := contentSigner{origin: "https://origin.example", key: []byte(mintedTestKey), nowMs: nowMs}
	minted := sign.urlFor(assetRef)
	if minted == "" {
		t.Fatal("the relay minted no url for an asset the origin holds")
	}

	u, err := url.Parse(minted)
	if err != nil {
		t.Fatalf("the relay minted an unparseable url %q: %v", minted, err)
	}
	if u.Query().Get(contenturl.QuerySignature) == "" {
		t.Fatalf("the relay minted %q with no signature, against a key it was given", minted)
	}

	rec := httptest.NewRecorder()
	store.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("the origin refused a url the relay minted under the SAME key: %d %s", rec.Code, rec.Body.String())
	}
}

// TestAnUnsignedRelayURLIsRefusedByASigningOrigin is the control. Without it,
// an origin that accepted everything would satisfy the test above.
func TestAnUnsignedRelayURLIsRefusedByASigningOrigin(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	store := origin.New(
		origin.WithSigningKey([]byte(mintedTestKey)),
		origin.WithClock(func() int64 { return nowMs }),
	)
	assetRef, err := store.Add([]byte("the actual content bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// A relay that was delivered NO key mints the unsigned form.
	sign := contentSigner{origin: "https://origin.example", nowMs: nowMs}
	unsigned := sign.urlFor(assetRef)
	if strings.Contains(unsigned, "?") {
		t.Fatalf("a keyless relay produced a query string: %q", unsigned)
	}
	u, err := url.Parse(unsigned)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	rec := httptest.NewRecorder()
	store.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a signing origin served an UNSIGNED relay url: %d, want 403", rec.Code)
	}
}

// TestTheMintedDeadlineIsMeasuredFromTheRelaysOwnClock is REL-066d's point.
//
// Minting at serve time is what keeps an offline relay useful: it applies a
// cached snapshot for the whole of an outage, and a deadline fixed when that
// snapshot was BUILT would have passed long before the relay stopped serving it.
// A relay minting a week later must produce a URL good a week later.
func TestTheMintedDeadlineIsMeasuredFromTheRelaysOwnClock(t *testing.T) {
	const built = int64(1_700_000_000_000)
	const aWeekLater = built + 7*24*60*60*1000

	body := []byte("the actual content bytes")
	// The origin's clock has moved on with the relay's — they share wall time;
	// what differs is when the URL was minted.
	store := origin.New(
		origin.WithSigningKey([]byte(mintedTestKey)),
		origin.WithClock(func() int64 { return aWeekLater }),
	)
	assetRef, err := store.Add(body)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	serve := func(minted string) int {
		t.Helper()
		u, err := url.Parse(minted)
		if err != nil {
			t.Fatalf("parse %q: %v", minted, err)
		}
		rec := httptest.NewRecorder()
		store.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
		return rec.Code
	}

	stale := contentSigner{origin: "https://origin.example", key: []byte(mintedTestKey), nowMs: built}
	if got := serve(stale.urlFor(assetRef)); got != http.StatusForbidden {
		t.Errorf("a url minted at snapshot-build time and fetched a week later got %d, want 403 — it is exactly the "+
			"expiry this design has to have", got)
	}

	fresh := contentSigner{origin: "https://origin.example", key: []byte(mintedTestKey), nowMs: aWeekLater}
	if got := serve(fresh.urlFor(assetRef)); got != http.StatusOK {
		t.Errorf("a url minted by the relay AT SERVE TIME, a week into an outage, got %d, want 200 — an offline relay "+
			"must keep serving fetchable content for as long as it keeps serving anything (REL-050/066d)", got)
	}
}
