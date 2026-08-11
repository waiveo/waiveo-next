package origin

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
)

// signer_test.go proves the claim Store.Signer exists to make: a URL minted by
// THIS store's signer is a URL THIS store serves, whatever its signing posture.
//
// The defect it closes is the two halves being wired independently. When the key
// is a parameter each side is handed separately, a deployment can hold a
// verifying origin and a minting producer that disagree — and it did, in the
// worst possible direction: the origin held a key and every app-side producer
// held none, so an upload answered 201 and the url it returned answered 403. The
// bug was not in either half. It was in the wiring, which nothing tested because
// there was nothing to test — an argument, passed or not passed.

// mintAndServe mints a url for body's digest through s's own signer, then
// fetches it back through s's own handler, and returns the status.
func mintAndServe(t *testing.T, s *Store, assetRef string) (int, string) {
	t.Helper()
	raw, _ := s.Signer("https://origin.example", contenturl.ServeTTL).Mint(assetRef)
	if raw == "" {
		t.Fatalf("the store's own signer minted nothing for %s", assetRef)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("unparseable minted url %q: %v", raw, err)
	}
	return get(t, s, u.RequestURI()).StatusCode, raw
}

// TestAStoresOwnSignerMintsWhatThatStoreServes drives both postures. Neither
// case is interesting alone; the pair is, because it is the same two lines of
// caller code under an origin configured two different ways, and both must work
// without the caller knowing which it has.
func TestAStoresOwnSignerMintsWhatThatStoreServes(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	body := []byte("the bytes a screen fetches")

	t.Run("an origin that enforces signatures", func(t *testing.T) {
		s, _ := signedOrigin(t, nowMs, body)
		ref, err := s.Add(body)
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if !s.Signer("https://origin.example", contenturl.ServeTTL).Signs() {
			t.Fatal("a key-holding store handed out a signer that does not sign — the two halves are wired independently again")
		}
		if code, raw := mintAndServe(t, s, ref); code != http.StatusOK {
			t.Fatalf("the origin refused %q, a url its OWN signer minted: %d", raw, code)
		}
	})

	t.Run("an origin that verifies nothing", func(t *testing.T) {
		s := New(WithClock(func() int64 { return nowMs }))
		ref, err := s.Add(body)
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if s.Signer("https://origin.example", contenturl.ServeTTL).Signs() {
			t.Fatal("a keyless store handed out a SIGNING signer — every url it mints would be refused by the very origin that minted it")
		}
		if code, raw := mintAndServe(t, s, ref); code != http.StatusOK {
			t.Fatalf("the origin refused %q, a url its OWN signer minted: %d", raw, code)
		}
	})
}

// TestTheSignersDeadlineIsMeasuredFromTheStoresOwnClock: Handler judges expiry
// against s.nowMs(), so a signer measuring from any other clock measures a skew
// rather than a lifetime — and it fails in whichever direction the skew runs.
func TestTheSignersDeadlineIsMeasuredFromTheStoresOwnClock(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	s, _ := signedOrigin(t, nowMs, []byte("bytes"))
	ref, err := s.Add([]byte("bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, expiresAt := s.Signer("https://origin.example", time.Hour).Mint(ref)
	if want := nowMs + time.Hour.Milliseconds(); expiresAt != want {
		t.Errorf("expires_at = %d, want %d (the store's own clock + the stated ttl)", expiresAt, want)
	}
}

// TestTheSignerCarriesACopyOfTheKey: a caller holding a Signer must not be able
// to reach into the store's key material and change what it verifies under.
func TestTheSignerCarriesACopyOfTheKey(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	s, _ := signedOrigin(t, nowMs, []byte("bytes"))
	ref, err := s.Add([]byte("bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	sign := s.Signer("https://origin.example", contenturl.ServeTTL)
	for i := range sign.Key {
		sign.Key[i] ^= 0xff
	}
	if code, _ := mintAndServe(t, s, ref); code != http.StatusOK {
		t.Fatalf("scribbling on a handed-out Signer's key changed what the store verifies under: %d", code)
	}
}
