package api_test

import (
	"net/http"
	"testing"
)

// TestAnUnresolvablePlacementDoesNotSuppressOtherFaults pins api/1 API-013's
// multi-field answer against the placement check added for DAT-006.
//
// The check's first version returned on its own first fault, so a row carrying
// BOTH an unresolvable placement and an unrelated bad field reported one error
// where the platform had been reporting two. That is a corpus-pinned shape, and
// bodyschema.go declines exactly this trade in writing: a fail-fast gate ahead of
// the real validator replaces a richer published answer with a poorer one.
//
// It lives at the HTTP layer because that is where the degradation is observable
// — an errors[] array in a Problem document — and where the corpus pins it.
func TestAnUnresolvablePlacementDoesNotSuppressOtherFaults(t *testing.T) {
	e := newEnv(t)
	body := []byte(`{"scope_node":"01J8ZN0SVCHN0DEANYWHERE001",` +
		`"schedule_id":"01J8ZSCHEDULEDOESNOTEXIST",` +
		`"name":"Two faults","start":"06:00","end":"22:00","weekdays":[1,2,3,4,5],` +
		`"display_power":"sideways"}`)

	res, raw := e.do(t, http.MethodPost, "/api/v1/dayparts", body, nil)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", res.StatusCode, raw)
	}
	p := assertProblem(t, res, raw, "VALIDATION_FAILED")
	errsAny, _ := p["errors"].([]any)
	if len(errsAny) < 2 {
		t.Fatalf("errors[] carries %d entry(ies) (body %s), want the placement fault AND the unrelated one — a short-circuit here costs API-013's multi-field answer", len(errsAny), raw)
	}
	first, _ := errsAny[0].(map[string]any)
	if first["field"] != "scope_node" {
		t.Errorf("leading error names %v, want scope_node so the reference is what a caller reads about; body %s", first["field"], raw)
	}
}
