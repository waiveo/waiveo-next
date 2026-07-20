package apihttp

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"

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
// after lastID (API-033/036). api/1 keys list pagination on the resource's ULID
// (API-034), and a ULID is already a URL-safe token that satisfies the opaque-
// cursor grammar, so the token IS lastID itself — the position is self-
// describing and nothing is constructed on top of it. The client treats the
// value as opaque and only ever passes it back verbatim.
func EncodeCursor(lastID string) string {
	return lastID
}

// DecodeCursor recovers the keyset position (the last id already seen) a cursor
// names (API-033/035). A cursor that fails the opaque-cursor grammar
// (API-036), or that satisfies the grammar but does not decode to a valid
// keyset position — for the ULID keyset, a syntactically valid ULID (API-034) —
// is rejected 400 / CURSOR_INVALID. A rejected cursor NEVER silently degrades
// to "start from the beginning" (API-035): the caller emits the Problem and
// serves no page. An empty cursor is not a keyset position and is likewise
// rejected — callers pass an empty cursor through ParsePageParams and skip the
// decode entirely to start from the beginning, rather than decoding "".
func DecodeCursor(cursor string) (lastID string, prob *PageParamError) {
	if !cursorTokenPattern.MatchString(cursor) || !ulid.Valid(cursor) {
		return "", &PageParamError{
			Status: http.StatusBadRequest,
			Code:   "CURSOR_INVALID",
			Title:  "Bad Request",
			Detail: fmt.Sprintf("The pagination cursor %q is not valid.", cursor),
		}
	}
	return cursor, nil
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
// cursor equal to EncodeCursor of the last returned row's id; otherwise window
// is the final page and the cursor is null. idOf extracts a row's keyset id.
// A page therefore never repeats or skips a row across the roundtrip (API-034).
func Page[T any](window []T, limit int, idOf func(T) string) PageEnvelope[T] {
	if limit < 0 {
		limit = 0
	}

	items := window
	var next *string
	if len(window) > limit {
		items = window[:limit]
		if len(items) > 0 {
			tok := EncodeCursor(idOf(items[len(items)-1]))
			next = &tok
		}
	}
	if items == nil {
		items = []T{}
	}

	return PageEnvelope[T]{Items: items, Cursor: next}
}
