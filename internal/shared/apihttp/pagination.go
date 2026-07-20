package apihttp

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// DefaultPageLimit is the page size api/1 applies when a list request supplies
// no limit (API-031).
const DefaultPageLimit = 50

// MaxPageLimit is the largest page size api/1 accepts; a supplied limit above
// it is rejected, never clamped (API-031).
const MaxPageLimit = 200

// cursorTokenPattern is api/1 API-036's opaque-cursor grammar: a continuation
// token is a client-opaque string of URL-safe base64url characters. A cursor
// that does not match is malformed and cannot name a keyset position.
var cursorTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// cursorScopeSep separates a scoped cursor's operation/resource-type tag from
// the keyset ULID it names. It is `_`, which the opaque-cursor grammar admits
// (API-036) but a canonical ULID never contains — a ULID is uppercase Crockford
// base32 (API-034) — so the first `_` unambiguously ends the scope tag. A scope
// tag therefore MUST NOT itself contain `_` (see EncodeCursor).
const cursorScopeSep = "_"

// PageParamError describes the Problem a page-parameter helper's caller MUST
// emit when a limit or cursor is rejected: the HTTP Status, the closed-registry
// Code (API-011: VALIDATION_FAILED or CURSOR_INVALID), a human Title, and a
// human-readable Detail. It is returned by pointer — a nil *PageParamError
// means the parameters are usable.
type PageParamError struct {
	Status int
	Code   string
	Title  string
	Detail string
}

// Error lets a *PageParamError satisfy the error interface; the message is the
// Problem Detail.
func (e *PageParamError) Error() string { return e.Detail }

// ParsePageParams parses a list request's raw `cursor` and `limit` query values
// (API-030/031). The limit defaults to DefaultPageLimit when rawLimit is empty;
// a supplied limit that is not an integer, or is outside the inclusive
// [1, MaxPageLimit] range, is rejected 400 / VALIDATION_FAILED and is NEVER
// silently clamped to a bound. The cursor is opaque here — it is passed through
// unchanged and validated only when a keyset position is needed, via
// DecodeCursor; an empty cursor means "start from the beginning".
func ParsePageParams(rawCursor string, rawLimit string) (cursor string, limit int, prob *PageParamError) {
	limit = DefaultPageLimit
	if rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n < 1 || n > MaxPageLimit {
			return "", 0, &PageParamError{
				Status: http.StatusBadRequest,
				Code:   "VALIDATION_FAILED",
				Title:  "Validation Failed",
				Detail: fmt.Sprintf("The limit parameter must be an integer between 1 and %d; got %q.", MaxPageLimit, rawLimit),
			}
		}
		limit = n
	}
	return rawCursor, limit, nil
}

// EncodeCursor returns the opaque continuation token for the keyset position
// after lastID within the given list operation / resource-type scope
// (API-033/036). A cursor carries no meaning across a different list operation
// or resource type — even where two id values are byte-identical (API-033) — so
// the token is bound to scope: it names a position ONLY for the scope that
// minted it, and DecodeCursor rejects it under any other.
//
// scope is a short, stable tag naming the list operation / resource type (e.g.
// "device", "automation"); it MUST NOT contain cursorScopeSep. The empty scope
// is the unscoped keyset: the token is lastID itself — the bare ULID (API-034),
// already a grammar-satisfying opaque token — which is the form api/1's
// automations list is pinned to by the corpus (API-032). A non-empty scope
// prefixes the tag and cursorScopeSep onto the ULID, still URL-safe under the
// opaque-cursor grammar (API-036). The client treats the value as opaque and
// only ever passes it back verbatim.
func EncodeCursor(scope, lastID string) string {
	if scope == "" {
		return lastID
	}
	if strings.Contains(scope, cursorScopeSep) {
		// A scope tag carrying the separator would make DecodeCursor's split
		// ambiguous; every caller passes a compile-time-constant tag, so this
		// is a programming error, surfaced rather than silently mis-scoped.
		panic("apihttp: EncodeCursor: cursor scope " + strconv.Quote(scope) + " must not contain " + strconv.Quote(cursorScopeSep))
	}
	return scope + cursorScopeSep + lastID
}

// DecodeCursor recovers the keyset position (the last id already seen) a cursor
// names within the given list operation / resource-type scope (API-033/035). It
// rejects 400 / CURSOR_INVALID a cursor that is malformed, or that was issued by
// a different operation or resource type (API-035):
//
//   - a cursor failing the opaque-cursor grammar (API-036);
//   - under the empty (unscoped) scope, a token that is not a syntactically
//     valid ULID keyset position (API-034) — which includes any scoped token,
//     since its scope tag and separator are not part of a ULID;
//   - under a non-empty scope, a token not carrying exactly that scope's tag,
//     or whose position is not a valid ULID — so a cursor minted for one scope
//     (e.g. a `device` id) is refused when replayed under another (e.g. the
//     `automation` list), never paged from as an arbitrary keyset position.
//
// scope MUST be the same tag EncodeCursor minted the cursor under. A rejected
// cursor NEVER silently degrades to "start from the beginning" (API-035): the
// caller emits the Problem and serves no page. An empty cursor is not a keyset
// position and is likewise rejected — callers pass an empty cursor through
// ParsePageParams and skip the decode entirely to start from the beginning,
// rather than decoding "".
func DecodeCursor(scope, cursor string) (lastID string, prob *PageParamError) {
	if !cursorTokenPattern.MatchString(cursor) {
		return "", cursorInvalid(cursor)
	}
	if scope == "" {
		// Unscoped keyset: the cursor IS the bare ULID position (API-034). A
		// scoped token carries a tag and the cursorScopeSep byte (never part of
		// a ULID), so ulid.Valid rejects it here — a scoped cursor can never
		// masquerade as an unscoped one.
		if !ulid.Valid(cursor) {
			return "", cursorInvalid(cursor)
		}
		return cursor, nil
	}
	prefix := scope + cursorScopeSep
	id, ok := strings.CutPrefix(cursor, prefix)
	if !ok || !ulid.Valid(id) {
		return "", cursorInvalid(cursor)
	}
	return id, nil
}

// cursorInvalid builds the 400 / CURSOR_INVALID Problem a rejected pagination
// cursor yields (API-035, closed code registry API-011).
func cursorInvalid(cursor string) *PageParamError {
	return &PageParamError{
		Status: http.StatusBadRequest,
		Code:   "CURSOR_INVALID",
		Title:  "Bad Request",
		Detail: fmt.Sprintf("The pagination cursor %q is not valid.", cursor),
	}
}

// PageEnvelope is the response body every api/1 list endpoint returns
// (API-032): the page's Items and the Cursor to fetch the next page. Cursor is
// a pointer so it serialises to JSON `null` on the last page (no further row
// remains) and to the opaque token string otherwise. Items always serialises to
// a JSON array, never null.
type PageEnvelope[T any] struct {
	Items  []T     `json:"items"`
	Cursor *string `json:"cursor"`
}

// Page cuts one page from window — the keyset-ordered rows strictly after the
// request's cursor position, fetched one beyond the limit so a further page can
// be detected — and returns the {items, cursor} envelope (API-032). When window
// holds more rows than limit, the page carries the first limit rows and a next
// cursor equal to EncodeCursor(scope, ...) of the last returned row's id;
// otherwise window is the final page and the cursor is null. scope is the list
// operation / resource-type tag the next cursor is bound to (see EncodeCursor);
// pass "" for the unscoped bare-ULID keyset. idOf extracts a row's keyset id. A
// page therefore never repeats or skips a row across the roundtrip (API-034).
func Page[T any](scope string, window []T, limit int, idOf func(T) string) PageEnvelope[T] {
	if limit < 0 {
		limit = 0
	}

	items := window
	var next *string
	if len(window) > limit {
		items = window[:limit]
		if len(items) > 0 {
			tok := EncodeCursor(scope, idOf(items[len(items)-1]))
			next = &tok
		}
	}
	if items == nil {
		items = []T{}
	}

	return PageEnvelope[T]{Items: items, Cursor: next}
}
