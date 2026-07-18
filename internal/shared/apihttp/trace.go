// Package apihttp is the shared server-side HTTP conformance helper every
// Wave-1 HTTP handler retrofits onto: api/1's Trace-Id propagation
// (API-060–062) and its RFC 9457 Problem error shape (API-010/API-002,
// reused by player/1 via PLY-005/PLY-006). It exists so every handler in
// this codebase — the feeder's loopback enrollment/state-pull/content-origin
// servers and the relay's player/1 pairing server — emits exactly the same
// header and error-body shape, rather than each package hand-rolling its own
// `{code, message}` convention.
package apihttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
)

// TraceIDHeader is the header name api/1 API-060 defines, request and
// response side.
const TraceIDHeader = "Trace-Id"

// traceIDPattern is API-061's validation grammar: 20-36 characters,
// [A-Za-z0-9-] only. A Crockford-base32 ULID and a hyphenated UUID both
// satisfy it.
var traceIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{20,36}$`)

// validTraceID reports whether v satisfies API-061 in full (both the
// length bound and the charset — the regexp's {20,36} quantifier already
// encodes the length bound, but MatchString alone would accept a longer
// string containing a 20-36 char valid substring, so this also checks the
// match spans the whole value).
func validTraceID(v string) bool {
	return traceIDPattern.MatchString(v) && len(v) >= 20 && len(v) <= 36
}

// newTraceID generates a fresh server-side trace id: 32 lowercase hex
// characters from crypto/rand. That satisfies API-061 (length 32, within
// [20,36]; charset a strict subset of [A-Za-z0-9-]) without requiring a real
// ULID library, per this codebase's existing random-hex-id convention
// (internal/feeder/enroll.randomHex and internal/relay/playerserver's
// newOpaqueToken use the same pattern for their own opaque ids).
func newTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Reader failing is a fatal environment problem
		// (entropy source unavailable); there is no meaningful error to
		// propagate through this value-returning helper, matching the
		// existing panic convention for the same failure elsewhere in this
		// codebase (e.g. internal/feeder/enroll.randomHex).
		panic("apihttp: newTraceID: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// resolveTraceID implements API-060/061: the request's own Trace-Id header
// value, if present and valid, otherwise a freshly generated one. An invalid
// supplied value is discarded and replaced, never rejected — API-061 is
// explicit that a bad Trace-Id is never itself a request error.
func resolveTraceID(r *http.Request) string {
	if v := r.Header.Get(TraceIDHeader); validTraceID(v) {
		return v
	}
	return newTraceID()
}

// traceIDContextKey is the unexported context key WithTraceID stores a
// request's resolved trace id under.
type traceIDContextKey struct{}

// WithTraceID returns a middleware that resolves a request's trace id
// exactly once (API-060/061), sets the Trace-Id response header to that
// value before calling next — so it rides both a success and an error
// response alike, since a Problem body's own Content-Type/status/body are
// written by the handler afterward, never this middleware — and makes the
// resolved value available to next (and everything next calls) via
// TraceID(r) / TraceIDFromContext(r.Context()), so a Problem body's
// trace_id extension member (API-062) always echoes the exact same value the
// header carries.
func WithTraceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := resolveTraceID(r)
		w.Header().Set(TraceIDHeader, id)
		ctx := context.WithValue(r.Context(), traceIDContextKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TraceID returns r's resolved trace id: the value WithTraceID already
// stored in r's context, if r passed through that middleware, or a freshly
// resolved one (API-060/061) otherwise — a handler not mounted behind
// WithTraceID (there should be none among this codebase's server mux
// registrations) still gets a conformant value rather than an empty one, at
// the cost of that value not being reflected in a response header no
// middleware is present to set.
func TraceID(r *http.Request) string {
	if id, ok := r.Context().Value(traceIDContextKey{}).(string); ok && id != "" {
		return id
	}
	return resolveTraceID(r)
}
