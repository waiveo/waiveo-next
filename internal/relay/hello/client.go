package hello

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// helloHTTPTimeout bounds the relay's loopback handshake exchange with the
// co-located app peer — like enrollment (internal/relay/enroll), this is a
// same-host call, so a short timeout is generous.
const helloHTTPTimeout = 10 * time.Second

// Declaration is everything a relay declares in its hello (REL-031) except the
// channel_binding_signature — PerformHello computes that from the challenge
// nonce and the relay's enrollment key (REL-032). SiteBinding is the relay's
// own cached copy, if any; it is only a declaration, since the app peer's
// hello-ack returns the authoritative site_binding the relay then adopts
// (REL-036).
type Declaration struct {
	ProtocolVersion string
	Features        []string
	SiteBinding     SiteBinding
	SubnetMetadata  SubnetMetadata
	ClockState      ClockState
}

// RefusedError is the typed refusal a relay observes when the app peer answers
// a problem+json (api/1) in place of hello-ack — the wire form of REL-032's
// CHANNEL_BINDING_INVALID and REL-033's PROTOCOL_VERSION_UNSUPPORTED. Code
// carries the taxonomy code the relay branches on; a channel-binding refusal
// (CHANNEL_BINDING_INVALID) means the app peer would not accept this relay's
// identity even though the TLS handshake itself succeeded, and the relay MUST
// treat the connection as unusable.
type RefusedError struct {
	Status int
	Code   string
	Title  string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("hello refused (HTTP %d): %s: %s", e.Status, e.Code, e.Title)
}

// PerformHello runs the relay's side of the REL-030–039 handshake against the
// co-located app peer at feederBaseURL (e.g. "https://127.0.0.1:7420"): it
// fetches the app peer's challenge (REL-030), signs the nonce with priv — the
// relay's enrollment private key — as channel_binding_signature (REL-032),
// posts its hello carrying decl (REL-031), and returns the app peer's
// hello-ack. The caller adopts hello-ack's negotiated_version and its
// authoritative site_binding (REL-036), feeding the latter into rules/1's
// engine location.
//
// A problem+json refusal (CHANNEL_BINDING_INVALID, PROTOCOL_VERSION_UNSUPPORTED)
// is returned as a *RefusedError so the caller can branch on the taxonomy code;
// a transport/decoding failure is returned as an ordinary wrapped error.
//
// Like enrollment's bootstrap client (internal/relay/enroll), the loopback
// TLS is server-authenticated but not chain-validated here — REL-032's
// application-layer channel binding, not this transport, is what proves the
// app peer accepts the relay's identity, and the pinned post-enrollment trust
// anchor (REL-137) lands with the persistent-CA mTLS transport in a later wave.
func PerformHello(feederBaseURL string, priv ed25519.PrivateKey, relayID string, decl Declaration) (HelloAck, error) {
	client := loopbackHelloClient()

	nonce, err := fetchChallenge(client, feederBaseURL)
	if err != nil {
		return HelloAck{}, fmt.Errorf("hello: PerformHello: fetch challenge: %w", err)
	}

	sig, err := SignChannelBinding(priv, nonce)
	if err != nil {
		return HelloAck{}, fmt.Errorf("hello: PerformHello: sign channel binding: %w", err)
	}

	h := Hello{
		Type:    "hello",
		RelayID: relayID,
		Body: HelloBody{
			ProtocolVersion:         decl.ProtocolVersion,
			Features:                decl.Features,
			SiteBinding:             decl.SiteBinding,
			SubnetMetadata:          decl.SubnetMetadata,
			ClockState:              decl.ClockState,
			ChannelBindingSignature: sig,
		},
	}

	ack, err := postHelloRequest(client, feederBaseURL, h)
	if err != nil {
		return HelloAck{}, err
	}
	return ack, nil
}

// fetchChallenge performs REL-030's GET /hello/challenge, returning the
// app peer's fresh single-use nonce.
func fetchChallenge(client *http.Client, feederBaseURL string) (string, error) {
	resp, err := client.Get(feederBaseURL + "/hello/challenge")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", refusalFrom(resp)
	}

	var c Challenge
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return "", fmt.Errorf("decode challenge: %w", err)
	}
	if c.Body.Nonce == "" {
		return "", fmt.Errorf("challenge carried an empty nonce")
	}
	return c.Body.Nonce, nil
}

// postHelloRequest performs REL-031's POST /hello, decoding the app peer's
// hello-ack or surfacing its typed refusal.
func postHelloRequest(client *http.Client, feederBaseURL string, h Hello) (HelloAck, error) {
	reqBody, err := json.Marshal(h)
	if err != nil {
		return HelloAck{}, fmt.Errorf("hello: PerformHello: marshal hello: %w", err)
	}

	resp, err := client.Post(feederBaseURL+"/hello", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return HelloAck{}, fmt.Errorf("hello: PerformHello: POST /hello: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return HelloAck{}, refusalFrom(resp)
	}

	var ack HelloAck
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return HelloAck{}, fmt.Errorf("hello: PerformHello: decode hello-ack: %w", err)
	}
	return ack, nil
}

// refusalFrom decodes the app peer's RFC-9457 problem+json refusal into a
// *RefusedError, falling back to a status-only refusal if the body is not that
// shape.
func refusalFrom(resp *http.Response) error {
	var pb struct {
		Code  string `json:"code"`
		Title string `json:"title"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, &pb)
	return &RefusedError{Status: resp.StatusCode, Code: pb.Code, Title: pb.Title}
}

func loopbackHelloClient() *http.Client {
	return &http.Client{
		Timeout: helloHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // REL-010/137 loopback bootstrap, see PerformHello doc
		},
	}
}
