// Package contenturl mints and verifies signed, expiring content URLs — the
// capability REL-061 names when it calls a `screen_programs[].content[]` entry
// a "signed content reference", and which until now this codebase did not have.
//
// # What shipped before this, and why it was a defect
//
// The content origin served `/content/<hex>` to anyone who asked, and every
// reference stamped `expires_at: 0`. Content-addressed storage makes that
// SOUND safe — you cannot ask for bytes whose hash you do not already know —
// but the hash is not a secret. It rides the snapshot to every enrolled relay
// and onward to every screen, and a `content` array is exactly a list of the
// digests a site is currently showing. Anyone who observes one holds a
// permanent, unrevocable read capability for that asset: no expiry to wait
// out, no signature to invalidate, and no way to tell a legitimate fetch from
// a replayed one. Rotating the asset does not help, because the old digest
// keeps serving.
//
// # The shape
//
// A minted URL carries two query parameters beside the content-addressed path:
//
//	<base>/content/<hex>?exp=<unix-ms>&sig=<hex-hmac>
//
// `sig` is HMAC-SHA256 over a domain-separated statement binding the digest to
// the deadline, under a key only the minting side and the verifying origin
// hold. The path itself is untouched, so REL-061's `<base>/content/<hex>` URL
// grammar — and every existing consumer that parses it — still reads.
//
// # Why the statement is built the way it is
//
// The signed statement is a domain tag, the digest, and the deadline, joined
// by newlines. That construction is only unambiguous if no field can contain
// the separator, so Sign and Verify BOTH reject a digest that is not pure
// lowercase hex before it ever reaches the MAC. This is not defensive
// throat-clearing: the digest arrives from the request PATH, an attacker
// controls it byte for byte, and a digest permitted to carry a newline would
// let one statement be read as another — two different (digest, deadline)
// pairs producing identical signed bytes. This repo has already found that
// exact collision in its envelope signing, and the guard is the lesson.
//
// A key is likewise rejected when empty. HMAC accepts a zero-length key
// happily and produces a perfectly well-formed signature that anybody can
// reproduce, so an unset key would not fail loudly — it would authenticate
// every forgery.
//
// # What this package is NOT
//
// It is not the screen's credential. A signed URL authorizes ONE asset until
// ONE deadline; it says nothing about who is fetching. Authenticating the
// fetcher is a separate obligation, and a bearer URL is deliberately weaker:
// it is the half that can ride REL-066's relay-constructed URLs, which are
// built by a party that holds no session with the origin.
package contenturl

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Query parameter names. Kept as constants because the verifying side reads
// what the minting side wrote, and a typo in either is a signature that never
// validates — a failure that looks exactly like an attack.
const (
	QueryExpires   = "exp"
	QuerySignature = "sig"
)

// statementDomain separates this signature's input from every other MAC this
// codebase computes under any key. Without it, a key reused for a second
// purpose could have one construction's bytes accepted as the other's.
const statementDomain = "waiveo-content-url/1"

// PathPrefix is the ONE spelling of the content route's path segment — the
// `/content/` between an origin's base URL and an asset's hex digest, which is
// all REL-061's `<base>/content/<hex>` grammar consists of beyond those two.
//
// It is exported so that the serving origin and every producer alike reach the
// grammar through this package rather than each concatenating it inline, and
// that is not a style preference. Concatenating it inline is precisely how this
// deployment shipped an origin that enforced signatures (Verify) alongside four
// separate call sites that went on emitting the bare, unsigned form: the app's
// own upload route handed a caller a `url` that the very same process answered
// 403, and no `image` or `video` layer could display on any screen. A grammar
// spelled in one place cannot acquire a fifth producer that forgot to sign
// without that showing up as a diff in this package.
// TestEveryContentURLIsMintedByThisPackage enforces it over the whole tree.
const PathPrefix = "/content/"

// assetRefPrefix is the `sha256:` scheme a content-addressed asset_ref carries
// (REL-061). A URL's path holds the bare hex digest, so Mint strips it.
const assetRefPrefix = "sha256:"

// The two content-URL lifetimes this codebase mints under. They differ by an
// order of magnitude because the two minting sites differ in WHEN they mint
// relative to when the URL is fetched, and a deadline is only ever as good as
// that gap.
//
// The rule that generates both: a minted URL must outlive the longest ordinary
// delay between the instant it is minted and the LAST fetch anyone will make
// with it. Everything else — the poll interval, a prefetch retry, a screen's
// content cache — is dwarfed by that gap, so it is the only term worth sizing
// against.
const (
	// ServeTTL is for a URL minted at the moment it is handed to its consumer:
	// the relay's schedule-resolved Lease items (REL-066d), and the app api's
	// upload/list responses. The gap there is a session — a screen fetching the
	// program it just pulled, a console rendering the media library page it just
	// loaded — so 24 hours is already some four orders of magnitude of headroom
	// over the player's ~10 s program poll and every prefetch retry behind it,
	// while keeping an observed URL's usefulness to an attacker bounded to a day.
	ServeTTL = 24 * time.Hour

	// SnapshotTTL is for a URL minted at snapshot BUILD time into a signed
	// desired-state generation (internal/feeder/snapshot). That gap is not a
	// session and not a poll interval: it is however long that generation stays
	// the one being served, and three separate mechanisms make it long.
	//
	// The feeder caches its built snapshot by store generation and rebuilds only
	// when an api write advances it or a pending screen-override expiry lapses —
	// so a site nobody authors for a fortnight serves a fortnight-old mint. A
	// relay applies its cached snapshot for the whole of an outage (REL-050),
	// with nothing able to refresh it. And internal/relay/playerserver's
	// SetServedProgram carries an app-authored `url` through to the Lease
	// UNMODIFIED, so for a pinned screen override — push-now, the very path this
	// defect was found on — the app-minted URL is the only one that screen will
	// ever be given for that program.
	//
	// So the deadline here has to outlive an AUTHORING LULL, and 24 hours does
	// not: a screen showing the same pushed cast for two days would 403 on the
	// second, which is the same P0 in a slower costume. Thirty days covers a
	// month of an unedited site while still bounding an observed URL to a month
	// rather than to forever — which is the entire property this package exists
	// to establish. Any authored write re-mints, so an actively-managed site's
	// real exposure is far shorter than the ceiling.
	//
	// The durable fix for the gap itself is REL-066d, which requires a relay to
	// re-mint EVERY url it serves at the moment it serves it, app-authored ones
	// included; the relay does that for the items it resolves itself and not yet
	// for the app-authored baseline it passes through. When it does, this
	// constant can come down to ServeTTL. Until then it is sized for the world
	// that exists rather than the one the contract describes.
	SnapshotTTL = 30 * 24 * time.Hour
)

// Distinguishable refusals. A caller branches on these: an expired URL is a
// client that needs a fresh reference, while a bad signature is a forgery or a
// key mismatch, and reporting the two as one error would hide an attack inside
// ordinary staleness.
var (
	// ErrMalformed is a URL missing a parameter, or carrying one that is not
	// the shape it must be. It is NOT a signature failure — nothing was
	// checked, because there was nothing well-formed to check.
	ErrMalformed = errors.New("contenturl: malformed signed content URL")
	// ErrExpired is a well-formed, correctly signed URL presented past its
	// deadline. The signature is verified BEFORE expiry is judged, so this
	// error is only ever reached by a URL this deployment really minted.
	ErrExpired = errors.New("contenturl: signed content URL has expired")
	// ErrSignatureInvalid is a well-formed URL whose signature does not verify
	// under this key: a forgery, a tampered digest or deadline, or a key that
	// has rotated out from under a reference still in flight.
	ErrSignatureInvalid = errors.New("contenturl: signed content URL signature is invalid")
	// ErrNoKey is a Sign or Verify attempted with no key. Returned rather than
	// tolerated: HMAC under an empty key is well-formed and reproducible by
	// anyone, so a missing key must fail loudly rather than authenticate
	// everything.
	ErrNoKey = errors.New("contenturl: no signing key")
)

// isLowerHex reports whether s is a non-empty string of lowercase hex digits.
//
// This is the guard that makes the newline-joined statement unambiguous, so it
// is written out rather than delegated to hex.DecodeString: DecodeString
// accepts UPPERCASE too, and two spellings of one digest that both verify
// would be two distinct URLs for the same signed capability.
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// statement is the exact byte string the MAC is computed over.
//
// Callers reach it only through Sign and Verify, both of which validate
// hexDigest first — the separator's unambiguity depends on that having
// happened, and a third caller that forgot would reintroduce the collision.
func statement(hexDigest string, expiresAtMs int64) []byte {
	return []byte(statementDomain + "\n" + hexDigest + "\n" + strconv.FormatInt(expiresAtMs, 10))
}

// Sign returns the query string authorizing hexDigest until expiresAtMs
// (inclusive), in the form `exp=<ms>&sig=<hex>`.
//
// expiresAtMs must be positive. Zero is refused specifically because it is the
// value this codebase used to stamp on every reference to mean "no expiry
// policy": accepting it here would let that same inert value flow through a
// signing path and read as a deliberate deadline of the epoch — either
// permanently expired or, under a laxer comparison, permanently valid. Neither
// is a policy anyone chose.
func Sign(key []byte, hexDigest string, expiresAtMs int64) (string, error) {
	if len(key) == 0 {
		return "", ErrNoKey
	}
	if !isLowerHex(hexDigest) {
		return "", fmt.Errorf("%w: digest %q is not lowercase hex", ErrMalformed, hexDigest)
	}
	if expiresAtMs <= 0 {
		return "", fmt.Errorf("%w: expiry %d is not a positive instant", ErrMalformed, expiresAtMs)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(statement(hexDigest, expiresAtMs))
	q := url.Values{}
	q.Set(QueryExpires, strconv.FormatInt(expiresAtMs, 10))
	q.Set(QuerySignature, hex.EncodeToString(mac.Sum(nil)))
	return q.Encode(), nil
}

// URL returns the complete signed URL for hexDigest under base — the
// `<base>/content/<hex>` grammar REL-061 defines, with Sign's query appended.
func URL(base string, key []byte, hexDigest string, expiresAtMs int64) (string, error) {
	q, err := Sign(key, hexDigest, expiresAtMs)
	if err != nil {
		return "", err
	}
	return UnsignedURL(base, hexDigest) + "?" + q, nil
}

// UnsignedURL returns the bare `<base>/content/<hex>` URL carrying no signature
// — the form a deployment holding NO key mints, and the only form that makes
// sense against an origin holding no key, since it verifies nothing.
//
// It is exported so even the deliberately-unsigned path spells the grammar here
// (PathPrefix's doc): the point of one spelling is lost if the unsigned branch
// keeps its own copy, and the unsigned branch is where a forgotten signature
// hides.
func UnsignedURL(base, hexDigest string) string {
	return base + PathPrefix + hexDigest
}

// Signer mints the content URLs one deployment's content origin will verify.
//
// # Why the four fields travel as one value
//
// Because a URL minted from one origin's base, under another origin's key, or
// against a clock other than the one its deadline will be read against, is not
// a URL that verifies — and each of those is an ordinary mistake when the four
// are four separate parameters threaded through a dozen call sites.
// internal/relay/schedulehost reached the same conclusion for its own three and
// bundled them; this is that type, promoted so the app side cannot grow a
// second, subtly different copy.
//
// # The empty key is a statement, not an omission
//
// An empty Key means THIS DEPLOYMENT DOES NOT SIGN, and Mint answers with the
// unsigned form. That is only correct if the verifying origin also holds no key
// — it skips verification exactly then (origin.Store.Handler) — so the two ends
// must be the same key or the mint is refused by the very process that made it.
// Do not assemble that agreement by hand: origin.Store.Signer builds a Signer
// carrying the STORE'S OWN key and the STORE'S OWN clock, which makes a
// disagreeing pair unconstructible rather than merely unlikely. A Signer built
// literally is for a caller that holds no origin at all (a fixture builder, a
// test), and states its posture in the literal where a reader can see it.
type Signer struct {
	// Base is the content origin a screen fetches from — REL-066's
	// `content_origin`. Empty means this deployment can state no content URL,
	// and Mint returns none rather than fabricating a box-local one (REL-140).
	Base string
	// Key is the HMAC key the origin verifies under (REL-066a). Empty means
	// this deployment does not sign; see the type doc.
	Key []byte
	// TTL is how long a minted URL stays fetchable — ServeTTL or SnapshotTTL,
	// chosen by when the mint happens relative to the fetch.
	TTL time.Duration
	// NowMs is the instant the deadline is measured from, injected rather than
	// read from a clock inside Mint so a generation built at a stated instant
	// is reproducible from that instant alone.
	NowMs int64
}

// Signs reports whether this Signer will produce signed URLs — equivalently,
// whether the origin it was built from enforces them.
func (s Signer) Signs() bool { return len(s.Key) > 0 }

// Mint returns the URL for one content-addressed assetRef (`sha256:<hex>`, or a
// bare hex digest) and the instant that URL expires at — the value belonging on
// the wire beside it, so a consumer is told when its capability dies instead of
// discovering it as a 403.
//
// It returns ("", 0) — no URL, which every consumer already degrades on — in
// each case where a fetchable URL cannot honestly be stated:
//
//   - no Base: there is no origin to point at, and no box-local one is invented
//     (REL-140). This is the pre-existing empty-origin degrade.
//   - a digest that will not sign (not lowercase hex): returning the UNSIGNED
//     form instead would be worse than returning none — against a verifying
//     origin it 403s, which reads to an operator as an authorization fault
//     rather than as the malformed asset_ref it is.
//   - a Key with no usable TTL or no clock (`NowMs <= 0`): a signer that can
//     sign but cannot state a deadline would otherwise mint one measured from
//     the epoch — a URL born thirty days expired, which fails as a 403 at the
//     screen and as nothing at all at the mint. A misconfigured Signer must be
//     visible where it is constructed, not on a wall a week later.
//
// With no Key it returns the unsigned form and a zero deadline, because nothing
// about that URL expires: the origin that will serve it verifies nothing.
func (s Signer) Mint(assetRef string) (url string, expiresAtMs int64) {
	if s.Base == "" {
		return "", 0
	}
	hexDigest := strings.TrimPrefix(assetRef, assetRefPrefix)
	if !s.Signs() {
		return UnsignedURL(s.Base, hexDigest), 0
	}
	if s.TTL <= 0 || s.NowMs <= 0 {
		return "", 0
	}
	expiresAtMs = s.NowMs + s.TTL.Milliseconds()
	signed, err := URL(s.Base, s.Key, hexDigest, expiresAtMs)
	if err != nil {
		return "", 0
	}
	return signed, expiresAtMs
}

// Verify checks the signed URL for hexDigest carrying query q, as of nowMs.
//
// # Order of checks, and why it is this order
//
// The signature is verified BEFORE the deadline is judged. Reversed, an
// expired-URL answer would be given to a request whose signature was never
// checked — telling an attacker that a digest they guessed and a deadline they
// invented were otherwise acceptable, and turning this into an oracle for
// which digests the origin holds. Verifying first means ErrExpired is only
// ever reached by a URL this deployment genuinely minted.
//
// # The deadline boundary
//
// A URL is valid THROUGH its deadline and expired strictly after it: nowMs
// greater than exp refuses. The inclusive reading is the one that matches what
// `expires_at` says on the wire — the instant it expires AT is the last
// instant it is good for — and the alternative silently shortens every TTL in
// the system by the clock's own resolution.
func Verify(key []byte, hexDigest string, q url.Values, nowMs int64) error {
	if len(key) == 0 {
		return ErrNoKey
	}
	if !isLowerHex(hexDigest) {
		return fmt.Errorf("%w: digest %q is not lowercase hex", ErrMalformed, hexDigest)
	}
	rawExp, rawSig := q.Get(QueryExpires), q.Get(QuerySignature)
	if rawExp == "" || rawSig == "" {
		return fmt.Errorf("%w: missing %s or %s", ErrMalformed, QueryExpires, QuerySignature)
	}
	expiresAtMs, err := strconv.ParseInt(rawExp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: expiry %q is not an integer", ErrMalformed, rawExp)
	}
	got, err := hex.DecodeString(rawSig)
	if err != nil {
		return fmt.Errorf("%w: signature %q is not hex", ErrMalformed, rawSig)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(statement(hexDigest, expiresAtMs))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrSignatureInvalid
	}
	if nowMs > expiresAtMs {
		return fmt.Errorf("%w: deadline %d, now %d", ErrExpired, expiresAtMs, nowMs)
	}
	return nil
}

// KeyBytes is the length of a content-URL signing key: 32 bytes, matching
// HMAC-SHA256's block-derived natural key size.
const KeyBytes = 32

// LoadOrCreateKey returns the deployment's content-URL signing key, generating
// and persisting one at path on first call and reading it back on every later
// one.
//
// # Why it persists rather than being derived
//
// Because a URL outlives the process that minted it. A relay applies a cached
// snapshot for the whole of an outage (REL-050) and a screen holds URLs it has
// not fetched yet, so a key regenerated at boot would invalidate every
// outstanding URL on every restart — turning an ordinary process restart into a
// site-wide content outage lasting until the next snapshot reached every relay.
//
// It is NOT derived from the feeder's signing identity. Deriving it would tie
// two different secrets' lifetimes together, so rotating the signing identity
// (an expiry, a re-enrollment) would silently invalidate every content URL as a
// side effect, and rotating the content key would be impossible without
// touching the identity. They rotate for different reasons and on different
// schedules.
//
// The file is written 0600 and read back with its permissions checked, because
// a key readable by another local account is a key that can mint URLs for every
// asset at the site.
func LoadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(b) != KeyBytes {
			return nil, fmt.Errorf("contenturl: key file %s is %d bytes, want %d — refusing to sign with a "+
				"truncated or foreign key rather than minting URLs nothing can verify", path, len(b), KeyBytes)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("contenturl: stat key file %s: %w", path, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf("contenturl: key file %s is mode %04o — it is readable beyond its owner, and a "+
				"reader of this file can mint a URL for every asset at this site; chmod 600 it", path, perm)
		}
		return b, nil
	case errors.Is(err, fs.ErrNotExist):
		key := make([]byte, KeyBytes)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("contenturl: generate key: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("contenturl: create key dir: %w", err)
		}
		// O_EXCL, so two feeders racing on one data dir cannot each believe they
		// wrote the key: the loser sees ErrExist and reads the winner's, rather
		// than both proceeding under keys that disagree.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				return LoadOrCreateKey(path)
			}
			return nil, fmt.Errorf("contenturl: create key file: %w", err)
		}
		defer f.Close()
		if _, err := f.Write(key); err != nil {
			return nil, fmt.Errorf("contenturl: write key file: %w", err)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("contenturl: read key file %s: %w", path, err)
	}
}
