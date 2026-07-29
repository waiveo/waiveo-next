package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// redemptionsink.go adapts the authoring store's pairing-grant retirement to
// the relay connection's own RedemptionSink — the app-peer half of relay/1
// REL-124: a relay reports every pairing-grant redemption it performs upstream
// at its next connection opportunity, and the app peer stops delivering that
// grant in later generations.
//
// What this is NOT is the enforcement of one-time redemption. REL-124c is
// explicit that the site-wide at-most-once property is REL-121b/REL-121c's
// binding — the grant names the ONE relay that may consume it, and that relay's
// own atomic check-and-consume is therefore the site's. Retirement here always
// TRAILS the redemption it reflects (by the reporting relay's disconnection,
// REL-122, and by every other relay's interval between generations, REL-057),
// so nothing may be built on it arriving in time. It shrinks how long a spent
// grant keeps riding snapshots; it does not decide anything.

// retireOnRedemptionTimeout bounds one report's store write. A report is a
// best-effort, re-sendable record (REL-124a): a write that cannot complete
// promptly is answered as a failure so the relay keeps the redemption owed and
// re-sends it, rather than blocking the connection's read loop on the store.
const retireOnRedemptionTimeout = 5 * time.Second

// storeRedemptionSink retires a reported grant from desired state.
type storeRedemptionSink struct{ st *store.Store }

// ApplyRedemption implements feederrelayconn.RedemptionSink.
//
// relayID is the reporting connection's own AUTHENTICATED identity, passed
// straight through to the store, which conditions its delete on it (REL-124b —
// a relay may retire only a grant bound to itself, so no relay can cancel a
// pairing in progress at a sibling). A grant the store does not hold is an
// idempotent no-op, not a refusal: a relay re-sending an owed report, or
// reporting one already retired, is doing exactly what REL-124a requires.
func (s storeRedemptionSink) ApplyRedemption(relayID string, body wire.PairingRedeemedBody) error {
	ctx, cancel := context.WithTimeout(context.Background(), retireOnRedemptionTimeout)
	defer cancel()

	retired, err := s.st.RetirePairingGrant(ctx, body.GrantID, relayID)
	if errors.Is(err, store.ErrPairingGrantBoundElsewhere) {
		// Translated to the connection layer's own sentinel so it answers
		// PAIRING_REPORT_UNAUTHORIZED rather than a generic internal failure.
		log.Printf("waiveo-feeder: relay %s reported a redemption of grant %s, which is bound to another relay — refused (REL-124b)", relayID, body.GrantID)
		return feederrelayconn.ErrGrantBoundElsewhere
	}
	if err != nil {
		return err
	}
	if retired {
		log.Printf("waiveo-feeder: relay %s reported redeeming pairing grant %s; retired from desired state (REL-124)", relayID, body.GrantID)
	}
	return nil
}
