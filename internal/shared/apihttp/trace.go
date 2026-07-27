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
	"net/http"
	"regexp"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
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

// newTraceID generates a fresh server-side trace id as a real ULID
// (internal/shared/ulid), which API-061 requires by name: "A value failing
// this check MUST be discarded and replaced with a freshly server-generated
// ULID."
//
// This was previously 32 lowercase hex characters, which satisfies API-061's
// GRAMMAR (length within [20,36]; charset a subset of [A-Za-z0-9-]) but not
// its stated TYPE, and the difference is load-bearing rather than pedantic.
// API-063 requires that "when a request causes work in another component (a
// relay-bound command, a durable event, a background job), the server MUST
// propagate the same Trace-Id value into that component's own record of the
// work" — and events/1 EVT-010 types a durable event's own `trace_id` field
// as a ULID, enforced by events.Validate. A hex trace id therefore could not
// be propagated into an events/1 envelope at all: it would fail the EVT-013
// delivery gate, so a producer had to substitute a fresh, uncorrelated id and
// API-063 silently did not hold. Minting a ULID here closes that, and costs
// nothing — internal/shared/ulid already exists (it was added later than this
// function's original hex convention, which is why the hex predated it).
func newTraceID() string {
	return ulid.New()
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
