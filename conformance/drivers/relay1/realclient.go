package relay1

import (
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
)

// RealRelayClient is the first-photon RelayClient: it delegates straight to
// the real relay-side implementations (internal/relay/enroll,
// internal/relay/desiredstate) — the code that ships. It is the concrete
// target the relay/1 driver validates now; the teeth meta-test swaps in a
// broken client to prove the oracle can fail.
type RealRelayClient struct{}

// NewRealRelayClient returns the first-photon relay client.
func NewRealRelayClient() RealRelayClient { return RealRelayClient{} }

// Name implements RelayClient.
func (RealRelayClient) Name() string { return "real-relay" }

// Enroll implements RelayClient via internal/relay/enroll.Run.
func (RealRelayClient) Enroll(feederBaseURL string, store *identity.Store) error {
	return relayenroll.Run(feederBaseURL, store)
}

// Pull implements RelayClient via internal/relay/desiredstate.Pull — the
// verify-then-apply gate under test.
func (RealRelayClient) Pull(feederBaseURL string, store *identity.Store) (desiredstate.Applied, error) {
	return desiredstate.Pull(feederBaseURL, store)
}
