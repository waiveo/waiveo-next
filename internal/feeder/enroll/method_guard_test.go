package enroll

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
)

// TestBootstrapMethodGuardsAnswerTypedErrorFrames drives every bootstrap
// endpoint with the wrong HTTP method and asserts the refusal is the relay/1
// typed error frame (REL-007) — MALFORMED_MESSAGE with a trace_id — never a
// bare plain-text body. One driven case per refusal path.
func TestBootstrapMethodGuardsAnswerTypedErrorFrames(t *testing.T) {
	_, ts, _, _ := newTestServer(t)
	client := ts.Client()

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"claim-token rejects POST", http.MethodPost, "/claim-token"},
		{"enroll rejects GET", http.MethodGet, "/enroll"},
		{"reenroll challenge rejects POST", http.MethodPost, "/reenroll/challenge"},
		{"reenroll renew rejects GET", http.MethodGet, "/reenroll/renew"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			res, err := client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer res.Body.Close()

			if res.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusMethodNotAllowed)
			}
			raw, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			var frame reenroll.ErrorFrame
			if err := json.Unmarshal(raw, &frame); err != nil {
				t.Fatalf("refusal body is not JSON (plain-text http.Error regression?): %v\nbody: %s", err, raw)
			}
			if frame.Type != "error" {
				t.Errorf("type = %q, want \"error\"", frame.Type)
			}
			if frame.Code != "MALFORMED_MESSAGE" {
				t.Errorf("code = %q, want MALFORMED_MESSAGE", frame.Code)
			}
			if frame.TraceID == "" {
				t.Errorf("trace_id absent from refusal frame")
			}
		})
	}
}
