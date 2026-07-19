package relay1

import (
	"fmt"

	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
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

// Hello implements RelayClient via internal/relay/hello.PerformHello — the
// relay/1 connection handshake (REL-030–039) under test, using the
// enrollment identity Enroll persisted into store to sign the channel
// binding (REL-032).
func (RealRelayClient) Hello(feederBaseURL string, store *identity.Store, decl hello.Declaration) (hello.HelloAck, error) {
	id, ok, err := store.Identity()
	if err != nil {
		return hello.HelloAck{}, fmt.Errorf("real-relay: Hello: read identity: %w", err)
	}
	if !ok {
		return hello.HelloAck{}, fmt.Errorf("real-relay: Hello: no enrolled identity")
	}
	return hello.PerformHello(feederBaseURL, id.PrivateKey, id.RelayID, decl)
}

// ReEnroll implements RelayClient via internal/relay/reenroll.ReEnroll — the
// Expired-certificate re-enrollment client under test (REL-020/027).
func (RealRelayClient) ReEnroll(feederBaseURL string, store *identity.Store) error {
	return reenroll.ReEnroll(feederBaseURL, store)
}
