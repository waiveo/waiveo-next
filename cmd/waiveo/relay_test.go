package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestRelayStatusOnAnUnenrolledBox: the command must be wired end to end, and
// the one thing it says about a box with no store must be the diagnosis, not a
// stack trace.
func TestRelayStatusOnAnUnenrolledBox(t *testing.T) {
	store := filepath.Join(t.TempDir(), "relay.db")
	e, out, _ := newEnv("")
	code, err := run(context.Background(), []string{"relay", "status", "--store", store}, e)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != exitFailure {
		t.Errorf("exit %d for a relay with no identity, want %d", code, exitFailure)
	}
	for _, want := range []string{"never enrolled", "Not visible from here", "connection state"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("relay status does not mention %q:\n%s", want, out.String())
		}
	}
}

// TestRelayStatusProbesHealthzWhenPointedAtOne: the /healthz read is the only
// part that touches the running relay, and it must be OPT-IN — a status command
// that dialled a LAN address nobody named would be a status command that
// multicasts by accident.
func TestRelayStatusProbesHealthzWhenPointedAtOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("the probe hit %q, not /healthz", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"component":"waiveo-relay","status":"ok","vitals":{"uptime_s":7}}`))
	}))
	t.Cleanup(srv.Close)

	store := filepath.Join(t.TempDir(), "relay.db")
	e, out, _ := newEnv("")
	if _, err := run(context.Background(), []string{"relay", "status", "--store", store, "--relay", srv.URL, "--json"}, e); err != nil {
		t.Fatalf("run: %v", err)
	}
	var rep struct {
		Healthz *struct {
			Status    int    `json:"http_status"`
			Component string `json:"component"`
		} `json:"healthz"`
		Blind []string `json:"blind_spots"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	if rep.Healthz == nil || rep.Healthz.Status != 200 || rep.Healthz.Component != "waiveo-relay" {
		t.Errorf("the healthz probe did not reach the report: %+v", rep.Healthz)
	}
	if len(rep.Blind) == 0 {
		t.Error("the JSON report omits the blind-spot list")
	}
}

func TestRelayStatusDoesNotProbeUnlessAsked(t *testing.T) {
	probed := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { probed = true }))
	t.Cleanup(srv.Close)
	store := filepath.Join(t.TempDir(), "relay.db")
	e, out, _ := newEnv("")
	if _, err := run(context.Background(), []string{"relay", "status", "--store", store}, e); err != nil {
		t.Fatalf("run: %v", err)
	}
	if probed {
		t.Error("relay status dialled a relay nobody named")
	}
	if !strings.Contains(out.String(), "not probed") {
		t.Errorf("the report does not say the relay was never contacted:\n%s", out.String())
	}
}
