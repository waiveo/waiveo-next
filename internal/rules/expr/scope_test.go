package expr

import (
	"encoding/json"
	"reflect"
	"testing"
)

// parseRaw parses raw, failing the test on a parse error.
func parseRaw(t *testing.T, raw string) Expr {
	t.Helper()
	e, perr := Parse(json.RawMessage(raw))
	if perr != nil {
		t.Fatalf("Parse(%s) unexpected parse error: %v", raw, perr)
	}
	return e
}

// SourceEntities reports the entity IDs a pipeline's state/attr sources
// reference (RUL-282's cross-entity check operates on exactly this set).
func TestSourceEntities(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"literal-string", `"hello"`, nil},
		{"literal-number", `42`, nil},
		{"now-source", `{"expr":"now()"}`, nil},
		{"state-source", `{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z2')"}`, []string{"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}},
		{"attr-source", `{"expr":"attr('01J8Z3K4N5P6Q7R8S9T0V1W2Z2','battery')"}`, []string{"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}},
		// A source entity is fixed by the source stage; downstream filters cannot
		// introduce another entity reference (RUL-292).
		{"state-piped", `{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z2') | upper"}`, []string{"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SourceEntities(parseRaw(t, tt.raw))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("SourceEntities = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// Defense in depth (RUL-282): when the Env is edge-scoped to a subject entity,
// a state/attr source referencing any other entity fails closed, even though
// that entity resolves in the snapshot.
func TestEvaluate_EdgeScopeRejectsOtherEntity(t *testing.T) {
	env := baseEnv()
	env.EdgeScoped = true
	env.ScopeEntity = "01J8Z3K4N5P6Q7R8S9T0V1W2Z2"

	// Same entity as the scope: resolves normally.
	if v, failed := evalRaw(t, `{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z2')"}`, env); failed || v != "playing" {
		t.Fatalf("same-entity state: got (%#v, failed=%v), want (\"playing\", false)", v, failed)
	}

	// A different entity that DOES resolve in the snapshot still fails closed
	// because it is outside the edge scope.
	if _, failed := evalRaw(t, `{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z3')"}`, env); !failed {
		t.Fatalf("other-entity state: expected fail-closed (failed=true) under edge scope")
	}
	if _, failed := evalRaw(t, `{"expr":"attr('01J8Z3K4N5P6Q7R8S9T0V1W2Z3','media_title')"}`, env); !failed {
		t.Fatalf("other-entity attr: expected fail-closed (failed=true) under edge scope")
	}

	// With scoping OFF, the other entity resolves as before (Task 2 behavior).
	env.EdgeScoped = false
	if _, failed := evalRaw(t, `{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z3')"}`, env); failed {
		t.Fatalf("unscoped other-entity state: expected resolve (failed=false)")
	}
}
