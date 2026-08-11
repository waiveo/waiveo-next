package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// wellFormedHealth is a SystemHealth body that satisfies the document's own
// schema: every required member present, every enum value in range.
func wellFormedHealth() map[string]any {
	return map[string]any{
		"status":        "ok",
		"checked_at_ms": 1754870000000,
		"uptime_ms":     123456,
		"version":       "dev",
		"services": []any{
			map[string]any{"name": "store", "status": "ok", "detail": "readable, 12 rows"},
		},
		"storage": map[string]any{
			"path": "/var/lib/waiveo", "status": "ok", "detail": "104 GB free",
			"total_bytes": 200000000000, "free_bytes": 104000000000, "used_percent": 48,
		},
		"relays": []any{
			map[string]any{"relay_id": "relay-1", "address": "https://10.0.0.9:7421", "screen_count": 2},
		},
		"screens": map[string]any{
			"total": 2, "live": 2, "fetching": 0, "rejected": 0, "stale": 0, "never_seen": 0,
			"paired": 2, "overridden": 0, "live_window_ms": 90000,
			"content_transfer_window_ms": 60000, "fetching_max_unacked_pulls": 3,
		},
	}
}

// healthBox serves the two routes `waiveo health` probes. body is what
// /api/v1/system-health answers with.
func healthBox(t *testing.T, body any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"component":"app","status":"ok"}`))
		case "/api/v1/system-health":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Trace-Id", "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5")
			_ = json.NewEncoder(w).Encode(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHealthPassesAWellFormedDeployment(t *testing.T) {
	srv := healthBox(t, wellFormedHealth())
	e, out, _ := newEnv("")
	code, err := run(context.Background(), []string{"health", "--api", srv.URL}, e)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != exitOK {
		t.Fatalf("exit %d, want 0:\n%s", code, out.String())
	}
	for _, want := range []string{"reachable", "system-health", "payload-shape", "HEALTH OK", "relay-1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("health output does not mention %q:\n%s", want, out.String())
		}
	}
}

// TestHealthIsNotJustAPing is the whole point of this command, stated as three
// separate failures. The workspace's standard is "port-bound + HTTP 200 +
// correct payload shape"; each case below satisfies strictly more of that
// standard than the one before it, and every one of them must still fail.
func TestHealthIsNotJustAPing(t *testing.T) {
	t.Run("nothing listening", func(t *testing.T) {
		// A port nothing is bound to. httptest hands out a real address and then
		// closes it, so this is a genuinely dead endpoint rather than a made-up one.
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := dead.URL
		dead.Close()

		e, out, _ := newEnv("")
		code, _ := run(context.Background(), []string{"health", "--api", url}, e)
		if code != exitFailure {
			t.Fatalf("exit %d against a dead port, want %d:\n%s", code, exitFailure, out.String())
		}
	})

	t.Run("listening and 200 but the wrong shape", func(t *testing.T) {
		// This is the case a `curl -sf /healthz` and even a "200 from the API" check
		// both pass. `screens` is missing entirely — a deployment serving a build
		// whose health summary predates the fleet roll-up would answer exactly this.
		body := wellFormedHealth()
		delete(body, "screens")
		srv := healthBox(t, body)

		e, out, _ := newEnv("")
		code, _ := run(context.Background(), []string{"health", "--api", srv.URL}, e)
		if code != exitFailure {
			t.Fatalf("exit %d for a body missing a required member, want %d:\n%s", code, exitFailure, out.String())
		}
		if !strings.Contains(out.String(), "payload-shape") {
			t.Errorf("the shape check is not what reported the failure:\n%s", out.String())
		}
	})

	t.Run("shape valid but a value outside the declared enum", func(t *testing.T) {
		body := wellFormedHealth()
		body["status"] = "fine" // the document declares ok/degraded/down/unknown
		srv := healthBox(t, body)

		e, out, _ := newEnv("")
		code, _ := run(context.Background(), []string{"health", "--api", srv.URL}, e)
		if code == exitOK {
			t.Fatalf("a status outside the declared enum passed:\n%s", out.String())
		}
	})
}

// TestHealthGradesDegradedApartFromDown: an operator paged for a degraded box
// and one paged for a dark box go to different places, and a script that cannot
// tell them apart treats every partial failure as an outage.
func TestHealthGradesDegradedApartFromDown(t *testing.T) {
	body := wellFormedHealth()
	body["status"] = "degraded"
	body["services"] = []any{map[string]any{"name": "relay-link", "status": "degraded", "detail": "1 of 2 relays connected"}}
	srv := healthBox(t, body)

	e, out, _ := newEnv("")
	code, err := run(context.Background(), []string{"health", "--api", srv.URL}, e)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != exitDegraded {
		t.Fatalf("exit %d for a degraded deployment, want %d:\n%s", code, exitDegraded, out.String())
	}

	body["status"] = "down"
	srv2 := healthBox(t, body)
	e, out, _ = newEnv("")
	code, _ = run(context.Background(), []string{"health", "--api", srv2.URL}, e)
	if code != exitFailure {
		t.Fatalf("exit %d for a down deployment, want %d:\n%s", code, exitFailure, out.String())
	}
}

// TestHealthSaysSoWhenItIsNotAuthorized: a 401 means the box is UP and this CLI
// cannot see it. Grading that as an outage would send someone to the datacentre
// over a missing token file.
func TestHealthSeparatesUnauthorizedFromUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	e, out, _ := newEnv("")
	code, err := run(context.Background(), []string{"health", "--api", srv.URL}, e)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != exitDegraded {
		t.Fatalf("exit %d for an unauthorized read, want %d (unknown, not down):\n%s", code, exitDegraded, out.String())
	}
	if !strings.Contains(out.String(), "not authorized") {
		t.Errorf("the verdict does not say the deployment is up and unreadable:\n%s", out.String())
	}
}

func TestHealthJSONIsMachineReadable(t *testing.T) {
	srv := healthBox(t, wellFormedHealth())
	e, out, _ := newEnv("")
	if _, err := run(context.Background(), []string{"health", "--api", srv.URL, "--json"}, e); err != nil {
		t.Fatalf("run: %v", err)
	}
	var v healthVerdict
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	if v.Verdict != "ok" || len(v.Checks) < 3 || v.Summary == nil {
		t.Errorf("verdict lost something: %+v", v)
	}
}
