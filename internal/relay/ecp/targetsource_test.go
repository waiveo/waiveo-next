package ecp

import (
	"net/http"
	"sync"
	"testing"
)

// targetsource_test.go covers the live-resolution half of this controller: the
// WithTargetSource seam that replaced a target map fixed at construction.
//
// The map was not merely inconvenient — it was the reason a discovered device
// could never be driven. The set of drivable devices is the intersection of
// what the app peer adopted and what this relay can locate, and BOTH sides of
// that move at runtime, so a snapshot of it taken at process start is wrong for
// the whole life of the process. These cases pin the two properties that make
// the dynamic seam safe: it is consulted per dispatch (not cached), and a
// source that declines is a REFUSAL, never a fallback to some default address.

// TestDispatchResolvesTargetOnEveryCall proves the source is consulted per
// dispatch. The same entity resolves to nothing, then to a real device, then to
// nothing again — the exact sequence an adoption followed by an un-adoption
// produces — and each dispatch must reflect the answer at the time it was made.
func TestDispatchResolvesTargetOnEveryCall(t *testing.T) {
	srv, recv := newFakeECP(t, http.StatusOK)
	target := targetFromServer(t, srv)

	var mu sync.Mutex
	adopted := false
	c := New(nil, WithTargetSource(func(entityID string) (Target, bool) {
		mu.Lock()
		defer mu.Unlock()
		if entityID != "entity-1" || !adopted {
			return Target{}, false
		}
		return target, true
	}))

	// Before adoption: refused, and the device is never touched.
	assertControllerError(t, c.Dispatch("entity-1", "home", nil), "COMMAND_UNRESOLVED")
	select {
	case got := <-recv:
		t.Fatalf("server received %+v for an unadopted entity — control must be adoption-gated", got)
	default:
	}

	mu.Lock()
	adopted = true
	mu.Unlock()

	// After adoption: the SAME controller instance now reaches the device, with
	// no reconstruction and no restart.
	if err := c.Dispatch("entity-1", "home", nil); err != nil {
		t.Fatalf("Dispatch after adoption = %v, want nil", err)
	}
	select {
	case got := <-recv:
		if got.path != "/keypress/Home" {
			t.Fatalf("server saw path %q, want /keypress/Home", got.path)
		}
	default:
		t.Fatal("server never received the post-adoption command")
	}

	mu.Lock()
	adopted = false
	mu.Unlock()

	// After un-adoption: refused again. A controller that had cached the target
	// would keep driving a device the operator just released.
	assertControllerError(t, c.Dispatch("entity-1", "home", nil), "COMMAND_UNRESOLVED")
	select {
	case got := <-recv:
		t.Fatalf("server received %+v after un-adoption — the target must not be cached", got)
	default:
	}
}

// TestWithTargetSourceIgnoresNil pins the documented convention: a nil option
// value leaves the constructed default in place rather than installing a nil
// resolver that would panic on the first dispatch.
func TestWithTargetSourceIgnoresNil(t *testing.T) {
	srv, recv := newFakeECP(t, http.StatusOK)
	c := New(map[string]Target{"entity-1": targetFromServer(t, srv)}, WithTargetSource(nil))

	if err := c.Dispatch("entity-1", "home", nil); err != nil {
		t.Fatalf("Dispatch = %v, want nil (the map given to New must survive a nil WithTargetSource)", err)
	}
	select {
	case <-recv:
	default:
		t.Fatal("server never received the command")
	}
}

// TestNewCopiesTargetMap proves New defends against a caller mutating the map
// it passed. A dispatch resolving through a map the caller still holds would
// make "where does this command go" depend on unrelated code's timing.
func TestNewCopiesTargetMap(t *testing.T) {
	srv, recv := newFakeECP(t, http.StatusOK)
	targets := map[string]Target{"entity-1": targetFromServer(t, srv)}
	c := New(targets)

	delete(targets, "entity-1")

	if err := c.Dispatch("entity-1", "home", nil); err != nil {
		t.Fatalf("Dispatch = %v, want nil (New must copy its targets)", err)
	}
	select {
	case <-recv:
	default:
		t.Fatal("server never received the command")
	}
}

// TestKeypressParityVocabulary walks the remote-control keys legacy parity
// actually requires — the D-pad, the transport row, and volume — and asserts
// each reaches the device as its own ECP keypress path.
//
// They all share one code path, which is exactly why they are worth listing:
// "keypress works" was already true of a single key, and the parity question is
// whether the surface an operator's remote needs is reachable through it at
// all. A key that needed escaping, or that the controller special-cased, would
// show up here and nowhere else.
func TestKeypressParityVocabulary(t *testing.T) {
	keys := []string{
		// D-pad + navigation
		"Up", "Down", "Left", "Right", "Select", "Back", "Home", "Info",
		// Transport
		"Play", "Rev", "Fwd", "InstantReplay",
		// Volume (Roku TV / supported devices)
		"VolumeUp", "VolumeDown", "VolumeMute",
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			srv, recv := newFakeECP(t, http.StatusOK)
			c := New(map[string]Target{"entity-1": targetFromServer(t, srv)})

			if err := c.Dispatch("entity-1", "keypress", map[string]any{"key": key}); err != nil {
				t.Fatalf("Dispatch keypress %q = %v, want nil", key, err)
			}
			select {
			case got := <-recv:
				if got.method != http.MethodPost || got.path != "/keypress/"+key {
					t.Errorf("server saw %s %s, want POST /keypress/%s", got.method, got.path, key)
				}
			default:
				t.Fatalf("server never received keypress %q", key)
			}
		})
	}
}
