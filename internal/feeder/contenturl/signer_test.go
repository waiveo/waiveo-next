package contenturl

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// signer_test.go covers Signer — the value every producer mints through — and in
// particular the three ways it declines to produce a URL. Each of those is a
// case where the alternative is a URL that LOOKS fine and 403s on a wall.

const (
	signerNow = int64(1_700_000_000_000)
	signerRef = "sha256:cafebabe0123456789abcdef"
	signerHex = "cafebabe0123456789abcdef"
)

// TestAMintedURLVerifiesUnderTheSameKey is the control the rest hangs off: the
// happy path really does produce something Verify accepts, at the deadline it
// reports.
func TestAMintedURLVerifiesUnderTheSameKey(t *testing.T) {
	s := Signer{Base: "https://origin.example", Key: testKey, TTL: time.Hour, NowMs: signerNow}
	raw, expiresAt := s.Mint(signerRef)
	if raw == "" {
		t.Fatal("Mint produced no url for a well-formed ref under a real key")
	}
	if want := signerNow + time.Hour.Milliseconds(); expiresAt != want {
		t.Fatalf("expiresAt = %d, want %d", expiresAt, want)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("Mint produced an unparseable url %q: %v", raw, err)
	}
	if got, want := u.Scheme+"://"+u.Host+u.Path, "https://origin.example"+PathPrefix+signerHex; got != want {
		t.Errorf("minted path = %q, want %q — the REL-061 grammar is the path, and the signature is only its query", got, want)
	}
	// The reported deadline is the SIGNED one, not a second number beside it.
	if err := Verify(testKey, signerHex, u.Query(), expiresAt); err != nil {
		t.Errorf("the origin refused a url minted for it: %v", err)
	}
	if err := Verify(testKey, signerHex, u.Query(), expiresAt+1); err == nil {
		t.Error("a url one millisecond past the deadline Mint REPORTED still verified — the reported deadline is not the signed one")
	}
}

// TestAKeylessSignerMintsTheUnsignedForm: an empty key is a statement that this
// deployment does not sign, and the origin it was taken from verifies nothing.
// The URL must therefore be the bare address, with no deadline claimed for it.
func TestAKeylessSignerMintsTheUnsignedForm(t *testing.T) {
	s := Signer{Base: "https://origin.example", TTL: time.Hour, NowMs: signerNow}
	raw, expiresAt := s.Mint(signerRef)
	if want := "https://origin.example" + PathPrefix + signerHex; raw != want {
		t.Errorf("keyless mint = %q, want %q", raw, want)
	}
	if strings.Contains(raw, "?") {
		t.Errorf("keyless mint carried a query: %q", raw)
	}
	if expiresAt != 0 {
		t.Errorf("keyless mint reported expires_at %d — nothing about an unsigned url expires, and a screen told "+
			"otherwise stops fetching content it could still have (PLY-086)", expiresAt)
	}
	if s.Signs() {
		t.Error("Signs() is true for a keyless Signer")
	}
}

// TestASignerWithNoOriginMintsNothing is the REL-140 degrade, unchanged: no
// box-local origin is ever fabricated.
func TestASignerWithNoOriginMintsNothing(t *testing.T) {
	raw, expiresAt := Signer{Key: testKey, TTL: time.Hour, NowMs: signerNow}.Mint(signerRef)
	if raw != "" || expiresAt != 0 {
		t.Errorf("Mint with no Base produced (%q, %d), want (\"\", 0) — a url must never name an origin nobody authorized", raw, expiresAt)
	}
}

// TestAnUnsignableDigestMintsNothingRatherThanTheUnsignedForm: falling back to
// the bare address here would turn a malformed asset_ref into an AUTHORIZATION
// failure at the screen, which is the wrong thing for an operator to be
// debugging.
func TestAnUnsignableDigestMintsNothingRatherThanTheUnsignedForm(t *testing.T) {
	s := Signer{Base: "https://origin.example", Key: testKey, TTL: time.Hour, NowMs: signerNow}
	for _, bad := range []string{"sha256:CAFEBABE", "sha256:not-hex", "sha256:", "sha256:cafe babe"} {
		raw, expiresAt := s.Mint(bad)
		if raw != "" || expiresAt != 0 {
			t.Errorf("Mint(%q) = (%q, %d), want (\"\", 0)", bad, raw, expiresAt)
		}
	}
}

// TestASignerThatCannotStateADeadlineMintsNothing is the misconfiguration guard.
//
// A Signer holding a key but no TTL, or no clock, would otherwise mint a URL
// whose deadline is measured from the epoch — born decades expired, correct in
// every visible respect, and failing only as a 403 on a screen. Refusing at the
// mint puts the failure where the Signer was constructed.
func TestASignerThatCannotStateADeadlineMintsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Signer
	}{
		{"no ttl", Signer{Base: "https://origin.example", Key: testKey, NowMs: signerNow}},
		{"negative ttl", Signer{Base: "https://origin.example", Key: testKey, TTL: -time.Hour, NowMs: signerNow}},
		{"no clock", Signer{Base: "https://origin.example", Key: testKey, TTL: time.Hour}},
		{"clock before the epoch", Signer{Base: "https://origin.example", Key: testKey, TTL: time.Hour, NowMs: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if raw, expiresAt := tc.s.Mint(signerRef); raw != "" || expiresAt != 0 {
				t.Errorf("Mint = (%q, %d), want (\"\", 0) — a signer that cannot state a deadline must fail where it "+
					"was built, not on a wall a month later", raw, expiresAt)
			}
		})
	}
}

// TestMintAcceptsABareHexDigestToo: the api's listing holds hex digests while a
// playlist row holds `sha256:<hex>`, and both feed the same minter. One of the
// two silently minting a url whose path carried the scheme prefix would 404.
func TestMintAcceptsABareHexDigestToo(t *testing.T) {
	s := Signer{Base: "https://origin.example", Key: testKey, TTL: time.Hour, NowMs: signerNow}
	withPrefix, _ := s.Mint(signerRef)
	bare, _ := s.Mint(signerHex)
	if withPrefix != bare {
		t.Errorf("Mint(%q) = %q but Mint(%q) = %q — the two spellings of one asset must mint one url", signerRef, withPrefix, signerHex, bare)
	}
}

// TestTheTTLsAreSizedByWhatRefreshesThem states the sizing rule as an assertion
// rather than leaving it in a comment.
//
// The rule changed, and the change is the point. A build-time deadline used to be
// sized to outlive an AUTHORING LULL, because nothing but an authored write
// re-minted it — thirty days of bearer capability bought to cover a case the
// deployment could not otherwise survive. It is now sized like the serve-time one
// because the feeder re-mints on a TIMER (SnapshotRemintInterval), so what a
// build-time url has to outlive is that interval and nothing longer. The pair of
// bounds below is what keeps those two facts from drifting apart: shorten the
// interval freely, but a TTL that does not comfortably exceed it is a site whose
// urls die between re-mints.
func TestTheTTLsAreSizedByWhatRefreshesThem(t *testing.T) {
	if ServeTTL <= 0 || SnapshotTTL <= 0 {
		t.Fatalf("a non-positive TTL mints nothing: ServeTTL=%s SnapshotTTL=%s", ServeTTL, SnapshotTTL)
	}
	// A day is the floor for the serve-time one: comfortably beyond the player's
	// ~10 s program poll and every prefetch retry behind it.
	if ServeTTL < 24*time.Hour {
		t.Errorf("ServeTTL is %s — a screen that cannot fetch content it was just told to play is the failure this "+
			"lifetime is sized against, and the poll/prefetch loop needs far more headroom than that", ServeTTL)
	}
	if SnapshotRemintInterval <= 0 {
		t.Fatalf("SnapshotRemintInterval is %s — a non-positive re-mint interval is not a bound, it is a rebuild on "+
			"every single relay pull", SnapshotRemintInterval)
	}
	// Twice the interval, not merely more than it: a url minted at the start of a
	// window must still be good for the whole of the NEXT one, since a relay that
	// pulls just before a re-mint holds that url until it pulls again.
	if SnapshotTTL < 2*SnapshotRemintInterval {
		t.Errorf("SnapshotTTL (%s) is less than twice SnapshotRemintInterval (%s): a relay that pulls one millisecond "+
			"before a re-mint holds those urls for a full interval afterwards, so anything less leaves a window in which "+
			"a screen is handed a url that dies before its next pull", SnapshotTTL, SnapshotRemintInterval)
	}
	// And the collapse itself, asserted. A build-time deadline LONGER than a
	// serve-time one is the shape that has to be re-argued: it can only be
	// justified by something that stops the build-time mint being refreshed, and
	// the feeder's timer is precisely what removed that.
	if SnapshotTTL > ServeTTL {
		t.Errorf("SnapshotTTL (%s) exceeds ServeTTL (%s). That is only defensible if a built generation can be served "+
			"for longer than the feeder's re-mint interval; if that is now true again, fix the refresh rather than "+
			"lengthening the capability — a long-lived content url is a long-lived unrevocable read of every asset the "+
			"site displays, which is the property this package exists to remove", SnapshotTTL, ServeTTL)
	}
}
