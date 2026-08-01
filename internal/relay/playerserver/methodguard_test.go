package playerserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPlayerRoutesRefuseTheWrongMethod pins the verb each player/1 route
// accepts, across the whole registered surface rather than the two the sweep
// happened to catch.
//
// The methods are contract-fixed, not hygiene: PLY-034 specifies
// `GET /player/v1/pair/status` and PLY-080 `GET /player/v1/program`, and the
// three write routes are POSTs. Two of the six guards could be deleted with the
// entire tree green — measured — and once the pattern is worth a test at all it
// is worth it for every route, since a new handler copied from a neighbour
// inherits whichever guard the neighbour had.
//
// Driven through Register's own mux, so this exercises the routing a relay
// actually serves rather than calling handlers directly. A route that stopped
// being registered fails here too.
func TestPlayerRoutesRefuseTheWrongMethod(t *testing.T) {
	srv := serverForRelay(t, "01J8ZRELAYMETHOD000000001")
	mux := http.NewServeMux()
	srv.Register(mux)

	for _, tc := range []struct {
		path  string
		allow string
	}{
		{"/player/v1/pair", http.MethodPost},
		{"/player/v1/pair/status", http.MethodGet},
		{"/player/v1/program", http.MethodGet},
		{"/player/v1/lease/ack", http.MethodPost},
		{"/player/v1/render/start", http.MethodPost},
		{"/player/v1/render/end", http.MethodPost},
	} {
		t.Run(tc.path, func(t *testing.T) {
			for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
				if m == tc.allow {
					continue
				}
				req := httptest.NewRequest(m, tc.path, strings.NewReader("{}"))
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s %s = %d, want 405 — the route's verb is fixed by player/1, and a state-changing "+
						"handler reachable by the wrong verb is reachable by callers that never intended to invoke it",
						m, tc.path, rec.Code)
				}
			}

			// The control. Without it a handler that answered 405 to EVERYTHING
			// satisfies every assertion above while serving no player at all.
			req := httptest.NewRequest(tc.allow, tc.path, strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code == http.StatusMethodNotAllowed {
				t.Errorf("%s %s = 405 on its OWN method — the route is unreachable", tc.allow, tc.path)
			}
		})
	}
}
