package auth

import "sync"

// Revocations is the live-stream teardown registry EVT-114 requires: "A session
// or API key credential's revocation MUST terminate every open events/1
// connection authenticated by it within a bounded delay, not merely block future
// connections."
//
// The distinction that requirement draws is the whole point. Refusing the NEXT
// connect is trivial and is what a naive implementation stops at; an SSE stream
// opened five minutes ago is a long-lived, already-authenticated pipe that
// happily keeps delivering platform state to a credential an operator has just
// revoked. This registry is what closes it.
//
// The bound achieved here is immediate rather than merely bounded: revocation
// runs in-process (Store.OnRevoke fires inside the revoking request), so a
// watcher's channel closes before the revoking request returns — comfortably
// inside events/1's own draft-note proposal of 60 seconds. The interface is
// written in terms of a closed channel rather than a poll interval so a future
// out-of-process revoker changes only where Revoked is called, not how a stream
// waits.
//
// The zero value is not usable; construct with NewRevocations.
type Revocations struct {
	mu sync.Mutex
	// watchers maps a session id to the set of channels to close when that
	// session is revoked. A channel is the signal rather than a callback so a
	// waiting stream can select on it alongside its other wake sources.
	watchers map[string]map[chan struct{}]struct{}
}

// NewRevocations returns an empty registry.
func NewRevocations() *Revocations {
	return &Revocations{watchers: make(map[string]map[chan struct{}]struct{})}
}

// Watch registers interest in sessionID's revocation and returns the channel
// that CLOSES when it is revoked, plus the cancel func the caller MUST defer to
// deregister on normal disconnect (otherwise a long-lived server accumulates one
// dead entry per stream that ever connected).
//
// A nil Revocations returns a nil channel and a no-op cancel: a component
// constructed without a registry (a unit test, a binding with no live streams)
// selects on a nil channel, which blocks forever — exactly the "never fires"
// behavior wanted, rather than a nil dereference.
func (rv *Revocations) Watch(sessionID string) (<-chan struct{}, func()) {
	if rv == nil || sessionID == "" {
		return nil, func() {}
	}
	ch := make(chan struct{})
	rv.mu.Lock()
	set, ok := rv.watchers[sessionID]
	if !ok {
		set = make(map[chan struct{}]struct{})
		rv.watchers[sessionID] = set
	}
	set[ch] = struct{}{}
	rv.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			rv.mu.Lock()
			defer rv.mu.Unlock()
			if set, ok := rv.watchers[sessionID]; ok {
				delete(set, ch)
				if len(set) == 0 {
					delete(rv.watchers, sessionID)
				}
			}
		})
	}
	return ch, cancel
}

// Revoked closes every channel watching sessionID and forgets them. It is
// idempotent: a second revocation of the same session finds no watchers and does
// nothing, so a double-revoke can never close an already-closed channel.
func (rv *Revocations) Revoked(sessionID string) {
	if rv == nil {
		return
	}
	rv.mu.Lock()
	set := rv.watchers[sessionID]
	delete(rv.watchers, sessionID)
	rv.mu.Unlock()
	for ch := range set {
		close(ch)
	}
}

// Watching reports how many open streams are currently watching sessionID —
// exposed for tests to assert that a disconnected stream deregistered rather
// than leaking an entry.
func (rv *Revocations) Watching(sessionID string) int {
	if rv == nil {
		return 0
	}
	rv.mu.Lock()
	defer rv.mu.Unlock()
	return len(rv.watchers[sessionID])
}
