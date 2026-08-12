package packs

import (
	"context"
	"sort"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// Catalog browse (marketplace/1 MKT-096): what the configured registry sources
// currently OFFER, as distinct from resolving one already-named reference.
//
// # What a listing is, and is not
//
// It is not an endorsement. Until channel-index/1's signature chain (CHI-050)
// is enforced, an index is untrusted transport — the resolver says so in as
// many words — so a listing conveys exactly "this source's index says it has
// this", and nothing about the artifact behind it has been checked at the point
// it is listed. Nothing here fetches an artifact, verifies a digest, or asks
// packsig anything, because there is nothing yet to ask about.
//
// That is safe for the same reason it is honest: what protects an operator who
// ACTS on a listing is unchanged. Installing a browsed reference runs the same
// resolution-time rules and the same install-time envelope verification against
// the host's own anchors as installing a typed one, so a forged listing can
// misdescribe an artifact but cannot install one. Browse widens what an
// operator can SEE without widening what the box will accept.

// CatalogEntry is one artifact a source offers.
type CatalogEntry struct {
	PackID  string
	Version string
	Kind    string
	// Source is the registry source that served this entry, and it is not
	// decoration: source order is a resolution preference and never a trust
	// decision (MKT-061), so an operator comparing two sources' offerings has
	// to be able to see which is which.
	Source  string
	Channel string
	// Status is the entry's own channel-index status, carried through for the
	// informational values (`deprecated`) that do not withhold the entry.
	Status string
}

// CatalogSource is one source's contribution, including its failure.
//
// A source that could not be read is REPORTED rather than dropped. An empty
// catalog and an unreachable registry are different facts (MKT-096), and a
// browse that silently omits the failing source states the first when the
// second is true — the same asymmetry the update badge draws, for the same
// reason: absence of evidence must never be rendered as evidence of absence.
type CatalogSource struct {
	ID      string
	Channel string
	Entries []CatalogEntry
	// Unavailable is why this source contributed nothing, empty when it did.
	Unavailable string
}

// browsableStatuses are the entry statuses a listing may carry.
//
// `yanked` is excluded because resolution refuses it outright (CHI-072):
// offering it is offering a refusal. `archived` is excluded because MKT-044
// defines it as the publisher's own withdrawal from discovery/browse — a host
// that lists it anyway overrides the publisher's decision. Both remain
// resolvable by an operator who names the exact version (MKT-044 preserves
// that path for `archived`), which is the difference between not advertising
// something and refusing it.
//
// An entry whose status this deployment does not recognise is NOT listed: a
// future status is far likelier to be a new way of withholding an entry than a
// new way of offering one, so the unknown case fails closed.
func browsable(status string) bool {
	switch status {
	case statusActive, statusDeprecated:
		return true
	default:
		return false
	}
}

// browsableKinds are the artifact kinds this surface enumerates (MKT-040/041).
func browsableKind(kind string) bool {
	return kind == "pack" || kind == "content-package"
}

// Browse enumerates what every configured source offers (MKT-096).
//
// Sources are returned in configured order, each with its own entries, and one
// source's failure never suppresses another's answer — the list is the point,
// and a deployment with three registries should not lose two catalogs because
// the first is down.
func (m *Market) Browse(ctx context.Context) []CatalogSource {
	out := make([]CatalogSource, 0, len(m.sources))
	for _, src := range m.sources {
		cs := CatalogSource{ID: src.ID, Channel: src.Channel}
		doc, err := m.fetchIndex(ctx, src)
		if err != nil {
			// The resolver's own message, which already names the source and
			// says whether it could not be read, did not parse, or served a
			// format/channel this client refuses.
			cs.Unavailable = err.Error()
			out = append(out, cs)
			continue
		}
		for _, e := range doc.Signed.Artifacts {
			if !browsableKind(e.Kind) || !browsable(e.Status) {
				continue
			}
			// artifact_id IS the pack id — the resolver compares it to one
			// directly (`e.ArtifactID != packID`). It carries no version suffix.
			if e.ArtifactID == "" || e.Version == "" {
				continue
			}
			cs.Entries = append(cs.Entries, CatalogEntry{
				PackID:  e.ArtifactID,
				Version: e.Version,
				Kind:    e.Kind,
				Source:  src.ID,
				Channel: doc.Signed.Channel,
				Status:  e.Status,
			})
		}
		// Ordered by pack, then by version string, so a catalog reads the same
		// way twice. This is NOT MKT-050a's numeric version order: that order
		// decides which version wins a resolution, and imposing it here would
		// suggest a precedence a listing does not have.
		sort.SliceStable(cs.Entries, func(i, j int) bool {
			if cs.Entries[i].PackID != cs.Entries[j].PackID {
				return cs.Entries[i].PackID < cs.Entries[j].PackID
			}
			return cs.Entries[i].Version < cs.Entries[j].Version
		})
		out = append(out, cs)
	}
	return out
}

// BrowseCatalog is the Installer's own entry point to MKT-096, mirroring how
// Update reaches the market.
//
// A deployment with no registry sources configured returns an EMPTY catalog and
// no error, which is the honest answer: nothing is offered because nothing is
// configured to offer it. That is deliberately unlike Update, which refuses —
// there, the operator asked about a specific installed pack and deserves to be
// told why it cannot be re-resolved; here they asked what is on offer, and
// "nothing" is a complete answer to that question rather than a failure to
// answer it.
func (in *Installer) BrowseCatalog(ctx context.Context) []CatalogSource {
	if in.market == nil {
		return []CatalogSource{}
	}
	return in.market.Browse(ctx)
}

// SetEnabled turns an installed pack off or back on (MKT-097).
//
// Disabling withdraws the pack's surfaces and preserves everything an uninstall
// would remove — bundle, rows, records. Enabling restores exactly that and
// re-runs no installation: nothing was removed, so there is nothing to resolve,
// verify or re-apply.
//
// A REQUIRED pack cannot be disabled. MKT-093b refuses removing one below its
// floor, and a required pack that is present and serving nothing is removal in
// everything but name — a deployment that declared it cannot run without a pack
// has not agreed to run without its surfaces either. The same
// REQUIRED_PACK_FLOOR the uninstall path returns, so a client that already
// renders that refusal renders this one.
//
// Enabling is never refused on those grounds: restoring a required pack is the
// direction the floor exists to protect.
func (in *Installer) SetEnabled(ctx context.Context, packID string, enabled bool) error {
	pack, found, err := in.store.GetPack(ctx, packID)
	if err != nil {
		return err
	}
	if !found {
		return store.ErrNotFound
	}
	if !enabled {
		if floor, required := in.store.RequiredFloor(packID); required {
			return artifactErr("REQUIRED_PACK_FLOOR",
				"%s is a required pack on this deployment (floor %s), so it cannot be disabled: a pack that is present and serving nothing is removal in everything but name (marketplace/1 MKT-097, MKT-093b)",
				packID, floor)
		}
	}
	if pack.Enabled == enabled {
		// Already in the requested state. PUT of a state, so this is a success
		// rather than a conflict — a retry must not be an error.
		return nil
	}
	ok, err := in.store.SetPackEnabled(ctx, packID, enabled)
	if err != nil {
		return err
	}
	if !ok {
		// Uninstalled between the read and the write.
		return store.ErrNotFound
	}
	return nil
}
