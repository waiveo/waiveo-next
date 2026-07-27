package relayconn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// TestUpgradeWithoutSubprotocolDrawsTypedError drives the one pre-upgrade
// refusal path on /relay/v1: a client offering no relay/1 subprotocol is
// refused with the typed error shape (REL-007) carried on the HTTP response —
// PROTOCOL_VERSION_UNSUPPORTED, no relay_id (the REL-005 pre-auth exception)
// — never a bare plain-text body.
func TestUpgradeWithoutSubprotocolDrawsTypedError(t *testing.T) {
	srv := New(
		func() (wire.StateSnapshotBody, error) { return wire.StateSnapshotBody{}, nil },
		nil, // key lookup never reached: the refusal fires before the upgrade
		nil, // revocation check likewise pre-upgrade-unreachable
		hello.SiteBinding{},
		hello.AppPeerImplementedMinors(1, 1),
		nil,
	)

	req := httptest.NewRequest(http.MethodGet, "/relay/v1", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	// Deliberately no Sec-WebSocket-Protocol header.
	rec := httptest.NewRecorder()
	apihttp.WithTraceID(srv.Handler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json (plain-text refusal regression?)", ct)
	}
	var frame wire.Frame
	if err := json.Unmarshal(rec.Body.Bytes(), &frame); err != nil {
		t.Fatalf("refusal body is not a JSON frame: %v\nbody: %s", err, rec.Body.String())
	}
	if frame.Type != wire.FrameTypeError {
		t.Errorf("type = %q, want %q", frame.Type, wire.FrameTypeError)
	}
	if frame.Code != "PROTOCOL_VERSION_UNSUPPORTED" {
		t.Errorf("code = %q, want PROTOCOL_VERSION_UNSUPPORTED", frame.Code)
	}
	if frame.RelayID != "" {
		t.Errorf("relay_id = %q, want empty (pre-auth exception)", frame.RelayID)
	}
	if frame.TraceID == "" {
		t.Errorf("trace_id absent from refusal frame")
	}
}
