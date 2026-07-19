package hello

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// RelayKeyLookup resolves a relay's enrollment-learned public key — the key
// the app peer recorded for this relay when it issued its certificate
// (Enrollment, REL-012). VerifyChannelBinding checks a hello's
// channel_binding_signature against exactly this key (REL-032), never one
// derived from the hello itself. ok is false for a relay the app peer never
// enrolled, which the hello handler treats as an unverifiable channel binding
// and refuses (CHANNEL_BINDING_INVALID).
//
// Wave-1 first-photon looks the key up by the self-asserted hello.relay_id:
// its single co-located relay reaches the app peer over the loopback bootstrap
// TLS this handler is served on, which carries no client certificate yet. The
// mTLS-authenticated-identity lookup REL-041 mandates (never trusting the
// self-asserted relay_id) lands with mutual-TLS in a later wave; the
// security-critical property this handler already upholds is REL-032's own —
// the binding is verified against the enrollment key and refused on failure,
// transport success notwithstanding.
type RelayKeyLookup func(relayID string) (ed25519.PublicKey, bool)

// AppPeerServer is the app-peer (feeder) side of the relay/1 connection
// handshake (REL-030–039): it issues the single-use challenge nonce (REL-030),
// then verifies + negotiates a relay's hello and answers hello-ack (via
// BuildHelloAck), refusing with the typed errors REL-032/033 require.
//
// It is served over the same HTTPS listener the feeder's enrollment endpoints
// use, mounting GET /hello/challenge and POST /hello. Safe for concurrent use;
// its one outstanding-nonce slot is mutex-guarded. Wave-1 first-photon has a
// single co-located relay, so the app peer tracks exactly one outstanding
// challenge nonce at a time — the most recently issued, consumed by the hello
// that binds it (REL-030 single-use). A per-connection nonce table lands with
// the concurrent multi-relay transport in a later wave.
type AppPeerServer struct {
	lookup             RelayKeyLookup
	site               SiteBinding
	implementedMinors  []string
	recognizedFeatures []string
	deprecated         map[string]DeprecationNotice

	mu          sync.Mutex
	outstanding string // the currently-issued, not-yet-consumed challenge nonce
}

// NewAppPeerServer builds the app peer's hello handler. lookup resolves a
// relay's enrollment public key (REL-032); site is the app peer's own
// authoritative site_binding it reports as canonical for the connection
// (REL-036); implementedMinors are the protocol_version minors the app peer
// implements for negotiation (REL-033/034 — build with AppPeerImplementedMinors);
// recognizedFeatures are the capability flags the app peer understands, for the
// shared subset (REL-035); deprecated is the (possibly nil) deprecation map
// reported verbatim in every hello-ack (REL-039), normalized to a non-nil map
// per hello so it marshals as {}.
func NewAppPeerServer(
	lookup RelayKeyLookup,
	site SiteBinding,
	implementedMinors []string,
	recognizedFeatures []string,
	deprecated map[string]DeprecationNotice,
) *AppPeerServer {
	return &AppPeerServer{
		lookup:             lookup,
		site:               site,
		implementedMinors:  implementedMinors,
		recognizedFeatures: recognizedFeatures,
		deprecated:         deprecated,
	}
}

// Register mounts the handshake's two routes onto mux: GET /hello/challenge
// (the app peer's first message, REL-030) and POST /hello (the relay's hello,
// answered with hello-ack, REL-031/039). Callers serve mux over the feeder's
// own HTTPS listener alongside the enrollment endpoints.
func (s *AppPeerServer) Register(mux *http.ServeMux) {
	mux.HandleFunc("/hello/challenge", s.handleChallenge)
	mux.HandleFunc("/hello", s.handleHello)
}

// handleChallenge mints a fresh single-use challenge nonce (REL-030), records
// it as the one outstanding nonce, and returns it. Minting a new challenge
// discards any prior unconsumed one — the app peer issues a challenge at the
// start of each connection attempt, and only the most recent is live.
func (s *AppPeerServer) handleChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	c, err := NewChallenge()
	if err != nil {
		apihttp.WriteProblem(w, r, apihttp.TraceID(r), http.StatusInternalServerError, "INTERNAL", "Internal Error")
		return
	}

	s.mu.Lock()
	s.outstanding = c.Body.Nonce
	s.mu.Unlock()

	writeHelloJSON(w, http.StatusOK, c)
}

// handleHello verifies a relay's hello against the nonce this connection's
// challenge issued (REL-032) and the relay's enrollment key, negotiates the
// version (REL-033/034), grants the shared feature subset (REL-035), and
// answers hello-ack with the app peer's authoritative site_binding (REL-036,
// REL-039). It refuses — in place of hello-ack — with the typed errors on
// failure: CHANNEL_BINDING_INVALID (unverifiable binding, including an unknown
// relay or no outstanding challenge) or PROTOCOL_VERSION_UNSUPPORTED (major
// mismatch). The bound nonce is consumed on any decisive outcome, so it can
// never be replayed (REL-030 single-use).
func (s *AppPeerServer) handleHello(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	traceID := apihttp.TraceID(r)

	var h Hello
	if err := json.NewDecoder(r.Body).Decode(&h); err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "CHANNEL_BINDING_INVALID", "Channel Binding Invalid")
		return
	}

	// Consume the outstanding nonce: single-use per connection attempt (REL-030).
	// An absent one (no challenge issued, or already consumed) leaves nothing to
	// bind against, which is itself a channel-binding failure.
	s.mu.Lock()
	nonce := s.outstanding
	s.outstanding = ""
	s.mu.Unlock()
	if nonce == "" {
		apihttp.WriteProblem(w, r, traceID, http.StatusForbidden, "CHANNEL_BINDING_INVALID", "Channel Binding Invalid")
		return
	}

	// The enrollment-learned key for this relay (REL-032/041). An unknown relay
	// is an unverifiable binding, refused identically to a wrong signature.
	relayPub, ok := s.lookup(h.RelayID)
	if !ok {
		apihttp.WriteProblem(w, r, traceID, http.StatusForbidden, "CHANNEL_BINDING_INVALID", "Channel Binding Invalid")
		return
	}

	ack, err := BuildHelloAck(h, nonce, relayPub, s.implementedMinors, s.recognizedFeatures, s.site, s.deprecated)
	if err != nil {
		switch {
		case errors.Is(err, ErrChannelBindingInvalid):
			apihttp.WriteProblem(w, r, traceID, http.StatusForbidden, "CHANNEL_BINDING_INVALID", "Channel Binding Invalid")
		case errors.Is(err, ErrProtocolVersionUnsupported):
			apihttp.WriteProblem(w, r, traceID, http.StatusConflict, "PROTOCOL_VERSION_UNSUPPORTED", "Protocol Version Unsupported")
		default:
			apihttp.WriteProblem(w, r, traceID, http.StatusInternalServerError, "INTERNAL", "Internal Error")
		}
		return
	}

	writeHelloJSON(w, http.StatusOK, ack)
}

func writeHelloJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
