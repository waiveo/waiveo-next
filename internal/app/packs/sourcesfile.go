package packs

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/manifest"
)

// sourcesfile.go is the on-disk half of marketplace/1 MKT-060/061/062: how a
// deployment DECLARES the registry sources it resolves pack installs against,
// and how this host turns that declaration into the ordered []Source a Market is
// built over.
//
// Until this file existed, NewMarket, Source and the whole 8,000-line resolution
// path were reachable only from this package's own tests. The feeder installed
// packs from an uploaded zip and never from a registry, and answered every
// marketplace reference with MARKETPLACE_REF_UNRESOLVED — "this deployment has
// no registry sources configured" — because nothing a deployment could author
// ever reached the option that configures them. A resolver no operator can point
// at a registry is not a marketplace.
//
// ---- What this file does NOT do -------------------------------------------
//
// It supplies sources. It does not decide what a source may serve, and it must
// never become the place that does. Everything below is configuration feeding
// checks that already exist in marketplace.go and install.go, and it is worth
// naming which, because the failure mode for a config surface is undermining a
// check rather than omitting one:
//
//   - MKT-021 (`first-party` iff the publisher segment is `waiveo`) is enforced
//     on the REFERENCE, in Ref.validate, before any source is consulted. Nothing
//     a source declares can grant or withhold it, and there is deliberately no
//     per-source trust-channel field here that could look like it might.
//   - MKT-062 (a reserved namespace resolves only from a host-authorized source)
//     is enforced in Market.resolve, against Source.ReservedNamespaces. This file
//     is where a host authorizes one, so the default is what matters: a source
//     that says nothing authorizes NOTHING, and a `waiveo/...` pack refuses from
//     it. Omitting the member is not "unspecified", it is "none".
//   - MKT-009b (the artifact's signature envelope, against the host's own trust
//     anchors) is enforced in Installer.install, on the bytes, after resolution.
//     No source configured here can relax it: a resolved artifact takes the same
//     path a hand-uploaded one does.
//   - MKT-050's anti-rollback and the pack-grain downgrade floor are enforced
//     inside the install transaction. Configuration cannot reach them.
//
// And one this file cannot help with, stated rather than implied: MKT-009's
// per-entry `publisher_signature` check and MKT-061's per-source index-signer
// key binding are NOT implemented anywhere in this repository — the signed-index
// trust chain does not exist (marketplace.go's package doc;
// conformance/unimplemented-error-codes.json lists PUBLISHER_SIGNATURE_UNVERIFIED
// and channel-index/1's nine). Binding a key id in this document would therefore
// configure a value nothing reads, which is the exact defect the deadcode gate
// exists to catch, so there is no key-id member here. What a configured source
// gets today is transport: the right to be READ. The host's trust anchors, not
// the source list, remain the only thing that admits bytes.
//
// ---- Absent, empty, and UNRESOLVABLE ---------------------------------------
//
// All three leave the box installing packs from an uploaded file exactly as it
// does today; none of them is fatal, and the difference between them is what the
// operator is told.
//
//   - ABSENT — no document. No registry source; installing by marketplace
//     reference refuses. This is every dev checkout, every CI run, and every box
//     nobody has provisioned. It is the default and needs no file.
//   - EMPTY — a document declaring `"sources": []`. Same enforcement, different
//     event: the deployment authored a list and chose to put nothing in it.
//   - UNRESOLVABLE — a document the host cannot read, parse, or validate. NO
//     source is configured, the boot says loudly why, and the box runs on.
//
// The last one is deliberately NOT the required-pack roster's posture, which is
// to refuse to start, and the asymmetry is the same one MKT-093a itself draws
// between an anchor set and a roster. A roster withholds a RESTRICTION: a host
// that fails to read one and carries on is running with floors the deployment
// declared silently lifted, which is worse than not booting. A source list
// withholds a CAPABILITY: a host that fails to read one and carries on can
// install nothing it could not install before, refuses every marketplace
// reference, and stays reachable for the operator to fix it. Refusing to boot
// there would take a screen fleet offline over a JSON comma.
//
// What it must never do is the middle path — load the sources it could parse and
// skip the one it could not. A partially-loaded list silently changes the
// operator's declared resolution ORDER (MKT-061), which is a security-relevant
// value: the source that was going to answer first may now be second, or gone.
// So the document loads whole or not at all.

// SourcesFormat is the registry sources document's format discriminant. It is
// the check that keeps some OTHER well-formed JSON document — the required-pack
// roster and the trust anchors being the obvious neighbours, provisioned by the
// same party into the same directory — from resolving to a valid, empty source
// list.
const SourcesFormat = "registry-sources/1"

// maxSourcesBytes caps the sources document this host will read. A source list
// is a handful of ids and urls; the cap exists so a file at the configured path
// cannot make the boot allocate without bound.
const maxSourcesBytes = 1 << 20

// sourcesWhat names the document in the shared guard's messages.
const sourcesWhat = "registry sources document"

// sourcesFile is the on-disk registry sources document. It carries deployment
// configuration only: no key material, no artifact bytes.
type sourcesFile struct {
	Format string `json:"format"`
	// Sources is an ARRAY, not an object keyed by source id, for two reasons
	// that are both requirements rather than taste. MKT-061 makes the list
	// ORDERED — "order MAY be used only as a plain resolution preference" — and
	// a JSON object has no order a decoder preserves. And Go's decoder silently
	// keeps the last of two entries under one key, so a keyed object could not
	// express a duplicate id at all: a document naming `upstream` twice would
	// resolve to one of them with no diagnostic.
	Sources []sourceEntry `json:"sources"`
}

// sourceEntry is one declared registry source.
//
// Every member here is read by something: `id` is recorded in the install record
// (MKT-094) and selects a source when a reference names one (MKT-060a(d)),
// `channel` is checked against the index document's own (fetchIndex),
// `index_url` is fetched and determines the transport, and
// `reserved_namespaces` is MKT-062's host authorization. There is deliberately
// no `stale_source` member: MKT-063 makes that a CONSEQUENCE of the scheme, and
// an operator able to write `"stale_source": false` beside a `file://` url could
// clear a mark the contract requires.
type sourceEntry struct {
	ID                 string   `json:"id"`
	Channel            string   `json:"channel"`
	IndexURL           string   `json:"index_url"`
	ReservedNamespaces []string `json:"reserved_namespaces"`
}

// SourcesAbsent reports whether no sources document existed at path — as
// distinct from one that existed and declared no source.
//
// The two are the same to the resolver (neither resolves anything) and very
// different to an operator, for the reason RosterAbsent exists: a typo'd env
// var, an edited-out unit line, a wrong working directory, or a document on a
// filesystem that mounts after the unit all land on ABSENT, and because the
// document is read once that is permanent for the process's life. A boot report
// that printed one line for both could not confirm the deployment loaded the
// list it meant to.
func SourcesAbsent(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

// LoadSources resolves the deployment's registry source list from the document
// at path (MKT-060/061/062), in the configured order.
//
// It returns, in the three cases above:
//
//   - ABSENT file → nil sources and a nil error.
//   - a readable, well-formed, valid document → its sources, IN DOCUMENT ORDER.
//   - anything else → a non-nil error AND nil sources. Never a partial list.
//
// The caller decides what an error means for the process; the feeder logs it and
// carries on with no marketplace. path should already be absolute — the caller
// pins it once at config load, for the reason the trust-anchor path is pinned: a
// relative path means the file the host consults follows the process's working
// directory.
func LoadSources(path string) ([]Source, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	raw, err := readHostConfig(path, sourcesWhat, maxSourcesBytes)
	if err != nil {
		return nil, sourcesUnusable(err)
	}
	var doc sourcesFile
	if err := decodeHostConfig(raw, path, sourcesWhat, &doc); err != nil {
		return nil, sourcesUnusable(err)
	}
	if doc.Format != SourcesFormat {
		return nil, sourcesUnusable(fmt.Errorf("the %s %s declares format %q, want %q — a document that is not a source list configures no source, never an empty one", sourcesWhat, path, doc.Format, SourcesFormat))
	}

	seen := make(map[string]bool, len(doc.Sources))
	out := make([]Source, 0, len(doc.Sources))
	for i, e := range doc.Sources {
		src, err := e.source()
		if err != nil {
			return nil, sourcesUnusable(fmt.Errorf("the %s %s: source %d (%q): %w", sourcesWhat, path, i, e.ID, err))
		}
		// A duplicate id is refused rather than deduplicated: the id is what an
		// install record pins as `source` (MKT-094) and what a reference selects
		// by (MKT-060a(d)), so two sources wearing one id make the record
		// ambiguous about which registry served the bytes, and make a
		// source-pinned reference resolve against a set rather than the one
		// source it named.
		if seen[src.ID] {
			return nil, sourcesUnusable(fmt.Errorf("the %s %s declares the source id %q twice (source %d) — an install record could not say which registry served the bytes", sourcesWhat, path, src.ID, i))
		}
		seen[src.ID] = true
		out = append(out, src)
	}
	// Order IS the declaration (MKT-061). Returned exactly as authored: nothing
	// here sorts, dedupes-by-preference, or promotes a source.
	return out, nil
}

// sourcesUnusable stamps every failure with what it MEANS for this document —
// applied to all of them, so no route out of LoadSources reports a broken
// document as anything an operator could read as "ignored".
func sourcesUnusable(err error) error {
	return fmt.Errorf("packs: %w — NO registry source is configured (marketplace/1 MKT-060)", err)
}

// source validates one declared entry and builds the Source it denotes.
//
// Everything refused here is refused because the alternative is CONFIGURATION
// THAT LOOKS LIKE IT DOES SOMETHING AND DOES NOT. That is the recurring failure
// in a declarative surface: a value the operator wrote, a document that parsed,
// and a rule nothing enforces.
func (e sourceEntry) source() (Source, error) {
	// The id must be able to appear in a log line, a URL query and an install
	// record without escaping, and must be stable. MAN-001's segment grammar is
	// already the repository's answer to that question, so it is reused rather
	// than a second charset being invented here.
	if !manifest.IsIDSegment(e.ID) {
		return Source{}, fmt.Errorf("the source id must be 2-39 characters of lowercase letters, digits and hyphens, starting with a letter (manifest/1 MAN-001's segment grammar)")
	}
	// NewMarket DROPS a source wearing the direct-install sentinel, silently,
	// which is the right behaviour for a programmatic caller and the wrong one
	// for a document: the operator's source would simply vanish, and the box
	// would report "no source resolves this" for a source it appeared to have.
	// Refused here, where there is someone to tell.
	if e.ID == store.SourceDirect {
		return Source{}, fmt.Errorf("%q is the install record's sentinel for an install NO registry mediated (marketplace/1 MKT-094a) — a source wearing it would make a registry-mediated install indistinguishable from a hand-uploaded one", store.SourceDirect)
	}
	// The channel binding is required, not optional-with-no-check. Source.Channel
	// left empty makes fetchIndex skip the agreement check entirely, so an
	// omitted member would silently buy a source the right to serve an index for
	// any channel at all — the permissive reading of a missing value, which is
	// the direction this whole file refuses to fail in.
	if e.Channel == "" {
		return Source{}, fmt.Errorf("a source must declare the channel-index/1 channel its index is expected to name; without it an index naming ANY channel is accepted from this source")
	}
	if strings.TrimSpace(e.Channel) != e.Channel || strings.ContainsAny(e.Channel, "\n\r\t") {
		return Source{}, fmt.Errorf("the channel %q carries surrounding or embedded whitespace, so it could never equal the channel an index document names", e.Channel)
	}

	src := Source{ID: e.ID, Channel: e.Channel}
	if err := src.applyIndexURL(e.IndexURL); err != nil {
		return Source{}, err
	}

	seen := map[string]bool{}
	for _, ns := range e.ReservedNamespaces {
		if !manifest.IsIDSegment(ns) {
			return Source{}, fmt.Errorf("the authorized namespace %q is not a publisher namespace any installable pack id could carry (manifest/1 MAN-001), so authorizing it would authorize nothing", ns)
		}
		// MKT-062's set is fixed by the contract (`waiveo`, `waiveo-*`,
		// `official-*`, `system-*`). Authorizing anything else is inert — a
		// non-reserved namespace needs no source authorization to resolve — and an
		// operator who wrote one believed they were granting something.
		if !isReservedNamespace(ns) {
			return Source{}, fmt.Errorf("%q is not one of marketplace/1 MKT-062's reserved namespaces (waiveo, waiveo-*, official-*, system-*); only those need a source authorization, so this line grants nothing", ns)
		}
		if seen[ns] {
			return Source{}, fmt.Errorf("the namespace %q is authorized twice", ns)
		}
		seen[ns] = true
		src.ReservedNamespaces = append(src.ReservedNamespaces, ns)
	}
	return src, nil
}

// applyIndexURL sets the source's index location, transport and MKT-063 stale
// mark from the one value that determines all three.
func (s *Source) applyIndexURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("index_url %q is not a url (%w)", rawURL, err)
	}
	switch u.Scheme {
	case "file":
		// A file:// index names a real path on this box. The directory holding it
		// becomes the registry ROOT the FileFetcher confines every fetch to, and
		// the index url is rewritten root-relative, because that is the coordinate
		// system FileFetcher.resolve works in: a `download_url` in the index is
		// resolved against the root, never against the filesystem. So the index
		// and the artifacts it names live in one directory tree, and nothing an
		// index author writes can read outside it.
		if u.Host != "" {
			return fmt.Errorf("index_url %q names a host; a file:// registry source is a path on this box (write file:///absolute/path)", rawURL)
		}
		p := filepath.FromSlash(u.Path)
		if !filepath.IsAbs(p) {
			return fmt.Errorf("index_url %q must be an absolute path (file:///absolute/path/index.json)", rawURL)
		}
		p = filepath.Clean(p)
		root, file := filepath.Split(p)
		root = filepath.Clean(root)
		// An index directly in the filesystem root would make the ENTIRE
		// filesystem the registry root, so every path an index author writes
		// becomes a read of this host. Refused rather than confined to nothing.
		if root == string(filepath.Separator) || root == "." || file == "" {
			return fmt.Errorf("index_url %q must name an index file inside a registry directory, not the filesystem root — the directory holding it becomes the root every fetch from this source is confined to", rawURL)
		}
		s.IndexURL = "file:///" + file
		s.Fetcher = FileFetcher{Root: root}
		// MKT-063: every install resolved through a file:// source is marked
		// pending a revocation-feed re-check. Derived from the scheme, never
		// declared, so it cannot be cleared by the document.
		s.StaleSource = true
		return nil
	case "https":
		if u.Host == "" {
			return fmt.Errorf("index_url %q names no host", rawURL)
		}
		if u.User != nil {
			return fmt.Errorf("index_url %q carries credentials in the url", rawURL)
		}
		s.IndexURL = rawURL
		s.Fetcher = HTTPFetcher{Base: rawURL}
		s.StaleSource = false
		return nil
	case "http":
		// Named separately from "unknown scheme" because the operator who wrote
		// it needs the reason, not the taxonomy: an index is the document that
		// decides which signed artifact this box installs, and over plaintext
		// anyone on the path rewrites it — including editing out a yank.
		return fmt.Errorf("index_url %q is plaintext http; an index served in the clear can be rewritten in flight by anyone on the path, including to hide a yank, so only https:// and file:// are served", rawURL)
	case "":
		return fmt.Errorf("index_url %q has no scheme; write an absolute https://… or file:///… url", rawURL)
	default:
		return fmt.Errorf("index_url %q uses the %q scheme; this deployment serves https:// and file:// registry sources (marketplace/1 MKT-060)", rawURL, u.Scheme)
	}
}
