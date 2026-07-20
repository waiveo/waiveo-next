package apihttp

import (
	"encoding/json"
	"net/http"
)

// ProblemContentType is the media type API-002/API-010 mandate for every
// error-response body.
const ProblemContentType = "application/problem+json"

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
func WriteProblem(w http.ResponseWriter, r *http.Request, traceID string, status int, code, title string) {
	w.Header().Set("Content-Type", ProblemContentType)
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
// shadow a base member.
func WriteProblemExt(w http.ResponseWriter, r *http.Request, traceID string, status int, code, title, detail string, extra map[string]any) {
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
	if r != nil && r.URL != nil && r.URL.Path != "" {
		body["instance"] = r.URL.Path
	}
	for k, v := range extra {
		body[k] = v
	}

	w.Header().Set("Content-Type", ProblemContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
