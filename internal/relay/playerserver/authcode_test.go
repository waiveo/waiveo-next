package playerserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// TestProgram401TaxonomyIsDiscriminableFromHeadersAlone is the regression test
// for the on-device defect PLY-008 exists to close: Roku firmware's
// roUrlTransfer returns an EMPTY response body for ANY 401 while delivering the
// response headers intact, so a player reading only the Problem body cannot
// tell CHANNEL_TOKEN_INVALID from CHANNEL_TOKEN_REVOKED from
// CHANNEL_TOKEN_EXPIRED — collapsing PLY-073's three distinct behaviors
// (re-pair / re-pair / renew) into one.
//
// It asserts the property that fix depends on, and asserts it WITHOUT LOOKING
// AT THE BODY AT ALL: for each of the three taxonomy codes GET
// /player/v1/program can refuse with, the response headers alone carry enough
// to discriminate. A relay that emitted the code only in the body would pass
// every other 401 test in this package and fail this one.
func TestProgram401TaxonomyIsDiscriminableFromHeadersAlone(t *testing.T) {
	cases := []struct {
		name string
		// arrange returns the token to present, having mutated srv so that
		// presenting it draws wantCode.
		arrange  func(t *testing.T, srv *Server, token string) string
		wantCode string
	}{
		{
			name:     "no token presented",
			arrange:  func(t *testing.T, srv *Server, token string) string { return "" },
			wantCode: "CHANNEL_TOKEN_INVALID",
		},
		{
			name:     "unknown token",
			arrange:  func(t *testing.T, srv *Server, token string) string { return "ct-does-not-exist" },
			wantCode: "CHANNEL_TOKEN_INVALID",
		},
		{
			name: "revoked screen",
			arrange: func(t *testing.T, srv *Server, token string) string {
				screenID, _, ok := srv.LookupChannelToken(token)
				if !ok {
					t.Fatalf("freshly redeemed token %q is not known to the server", token)
				}
				srv.SetRevokedScreens(1, []string{screenID})
				return token
			},
			wantCode: "CHANNEL_TOKEN_REVOKED",
		},
		{
			name: "expired token",
			arrange: func(t *testing.T, srv *Server, token string) string {
				srv.mu.Lock()
				rec := srv.tokens[token]
				rec.ExpiresAt = time.Now().Add(-time.Minute).UnixMilli()
				srv.tokens[token] = rec
				srv.mu.Unlock()
				return token
			},
			wantCode: "CHANNEL_TOKEN_EXPIRED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, token := programTestServer(t)
			present := tc.arrange(t, srv, token)

			resp, _ := doProgram(t, srv, present, []string{"image", "video"})

			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			// Deliberately NOT reading the body: this asserts exactly what a
			// body-dropping client can see.
			got := resp.Header.Get(apihttp.ProblemCodeHeader)
			if got != tc.wantCode {
				t.Errorf("%s header = %q, want %q — a player that cannot read a 401 body has no other way to reach PLY-073's correct branch",
					apihttp.ProblemCodeHeader, got, tc.wantCode)
			}
		})
	}
}
