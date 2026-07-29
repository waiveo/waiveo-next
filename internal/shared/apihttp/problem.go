package apihttp

import (
	"encoding/json"
	"net/http"
)

// ProblemContentType is the media type API-002/API-010 mandate for every
// error-response body.
const ProblemContentType = "application/problem+json"

// ProblemCodeHeader is the response header every Problem this package writes
// echoes its own `code` member into (player/1 PLY-008). It exists because a
// real client platform in this system — Roku firmware's roUrlTransfer, which
// player-v3 is built on — returns an EMPTY response body for a 401 regardless
// of what the server sent, while delivering the response HEADERS intact
// (measured on device against this relay: the relay sends a correct 177-byte
// application/problem+json body that curl reads fine and the firmware drops).
// Without a header channel such a client cannot tell CHANNEL_TOKEN_INVALID
// from CHANNEL_TOKEN_EXPIRED from CHANNEL_TOKEN_REVOKED, and PLY-073's
// requirement that the first two clear the channel token and re-pair while the
// third only renews is simply unimplementable there.
//
// The body stays canonical: the header is an ADDITIONAL, byte-identical
// carrier of the same `code`, never a replacement, and never carries anything
// the body does not already carry to the same caller in the same response.
//
// Set here — in the one function that already owns writing `code` into a
// Problem — rather than at the individual 401 call sites, so the invariant is
// "wherever a `code` reaches a body, the identical `code` reaches this header"
// with no second rule about which statuses qualify for a future handler to get
// wrong. See problem.go's own package tests for the assertion.
//
// Header-value safety: `code` is drawn from a contract's closed error-code
// registry (API-011/PLY-007) — uppercase ASCII and underscores — so it cannot
// carry a header-injecting control character; net/http additionally replaces
// CR/LF in a header value with spaces when writing.
const ProblemCodeHeader = "Problem-Code"

// problemType is the `type` member every Problem this package writes
// carries. api/1 API-016 mandates the literal string "about:blank" for this
// contract version — `code` (API-011) is the sole machine-readable
// discriminant api/1 defines today, so this package never mints a
// dereferenceable `type` URI.
const problemType = "about:blank"

// problem is the RFC 9457 document api/1 API-010 (reused by player/1 via
// PLY-005) mandates for every error response: at minimum `type`, `title`,
// `status`, and the extension members `code` and `trace_id`; `instance`
// (API-015), when present, is the request's own path.
type problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Code     string `json:"code"`
	TraceID  string `json:"trace_id"`
	Instance string `json:"instance,omitempty"`
}

// WriteProblem writes status and an RFC 9457 application/problem+json body
// to w: {type: "about:blank", title, status, code, trace_id: traceID,
// instance: r.URL.Path}. code MUST be a value from the calling contract's
// own error-code registry (api/1 API-011 or a sibling contract's own
// registry per PLY-007) — this helper does not validate it, since that
// registry is per-contract and the caller already knows which value applies.
//
// traceID MUST be the same value the response's Trace-Id header carries
// (API-062) — ordinarily apihttp.TraceID(r), the value WithTraceID already
// resolved and set as the response header for this same request.
//
// The same code is also echoed into the ProblemCodeHeader response header
// (PLY-008) — see that constant's own doc for why.
func WriteProblem(w http.ResponseWriter, r *http.Request, traceID string, status int, code, title string) {
	w.Header().Set("Content-Type", ProblemContentType)
	w.Header().Set(ProblemCodeHeader, code)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{
		Type:     problemType,
		Title:    title,
		Status:   status,
		Code:     code,
		TraceID:  traceID,
		Instance: r.URL.Path,
	})
}

// WriteProblemExt writes a Problem like WriteProblem but additionally carries a
// human-readable `detail` member and any contract-defined extension members in
// extra (e.g. `current_revision` for a REVISION_CONFLICT, API-023, or an
// `errors` array for a multi-field VALIDATION_FAILED, API-013). The base
// members (`type: about:blank`, `title`, `status`, `code`, `trace_id`,
// `instance`) are identical to WriteProblem; `detail` is omitted when empty;
// each key in extra becomes a top-level Problem extension member. As with
// WriteProblem, code MUST come from the calling contract's own closed error-
// code registry (API-011) — this helper does not validate it — and traceID
// MUST equal the response's Trace-Id header value (API-062). extra MUST NOT
// shadow a base member. As with WriteProblem, `code` is also echoed into the
// ProblemCodeHeader response header (PLY-008).
func WriteProblemExt(w http.ResponseWriter, r *http.Request, traceID string, status int, code, title, detail string, extra map[string]any) {
	instance := ""
	if r != nil && r.URL != nil {
		instance = r.URL.Path
	}
	body := ProblemBody(traceID, status, code, title, detail, instance, extra)

	w.Header().Set("Content-Type", ProblemContentType)
	w.Header().Set(ProblemCodeHeader, code)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ProblemBody builds the RFC 9457 document WriteProblemExt writes, WITHOUT
// writing it anywhere — the members and their names in one place, for a carrier
// that is not an http.ResponseWriter.
//
// It exists because api/1's Problem shape is reused verbatim over a transport
// that has no HTTP response writer at all: the security-model/1 console binding
// (SEC-074, "the console binding's request and response bodies MUST reuse
// api/1's own conventions unmodified: the Problem error shape ... this contract
// defines the console binding's transport and admission rule, never a second
// error shape"). A second hand-written map over there is exactly how "the same
// shape" becomes two shapes that drift, so the socket encoder calls this and
// WriteProblemExt calls this, and neither owns a copy.
//
// instance is the request's own path (API-015) where the carrier has one, and
// empty where it does not — a console request names a verb, not a URL path.
func ProblemBody(traceID string, status int, code, title, detail, instance string, extra map[string]any) map[string]any {
	body := map[string]any{
		"type":     problemType,
		"title":    title,
		"status":   status,
		"code":     code,
		"trace_id": traceID,
	}
	if detail != "" {
		body["detail"] = detail
	}
	if instance != "" {
		body["instance"] = instance
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}
