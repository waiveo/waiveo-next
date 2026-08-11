package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/keepalive"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
)

// offlineadoption_test.go is the OFFLINE BOOT half of the device plane: what a
// relay may drive after it restarts with no app peer reachable.
//
// revocation_test.go pins the same boot for credentials. This pins it for
// adoption, and the two failure modes are not symmetric. An unrestored
// revocation set is loud in the direction that matters (a credential the app
// peer voided gets served, and the app peer notices as soon as it reconnects).
// An unrestored ADOPTED set is silent in both: every consumer of it fails
// closed, so the relay comes up connected to nothing, driving nothing, keeping
// no screen alive — and reports nothing wrong while doing it.
//
// Which is precisely the power-cut boot. The relay and its app peer come back
// at the same moment, the relay's pull loses that race, and screen keep-alive
// (player/1 PLY-150-157) — the ONE capability whose entire justification is
// that a screen idling at the Roku Home screen shows NOTHING until a human
// walks past — consults an empty gate and relaunches nothing.

// newHomeScreenECP starts an httptest server answering the two ECP queries
// keepalive's poller reads, as a screen permanently parked at Home with the
// power on: the state rules 2 and 3 fire a recovery launch for. It mirrors
// internal/relay/keepalive's own newECPFixture, which this package cannot
// reach.
func newHomeScreenECP(t *testing.T) keepalive.Target {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/query/active-app", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<active-app><app id="1" type="home">Home</app></active-app>`))
	})
	mux.HandleFunc("/query/device-info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<device-info><power-mode>PowerOn</power-mode></device-info>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse ECP fixture URL: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split ECP fixture host/port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse ECP fixture port: %v", err)
	}
	return keepalive.Target{Host: host, Port: port}
}

// TestOfflineBootKeepsAnAdoptedScreenAlive is the defect's oracle: after a
// restart with no app peer in existence, keep-alive still re-launches a screen
// this deployment adopted.
//
// Everything durable is real. One on-disk store, written by the same atomic
// primitive desiredstate.VerifyAndApply commits with, closed as a process exit
// closes it and reopened from the same file. The second boot then runs main's
// own offline path verbatim — installPersistedServingState over a ZERO
// desiredstate.Applied, which is exactly what main holds when its pull fails or
// never happened — and seeds the keep-alive adoption gate exactly as main seeds
// it, from that same Applied. Nothing simulates the outage: the second boot has
// no app peer of any kind, because none is constructed.
//
// The assertion is made twice on purpose, and the two can fail independently.
// The direct one pins that the persisted set REACHES the gate; the behavioural
// one pins that a launch actually goes out, without which the gate could be
// right for the wrong reason (a target set that is empty, a rule that never
// fires). The controller is a bare recordingController rather than the wrapped
// command surface production uses, matching keepalive's own tests — what is
// under test here is whether the launch is DECIDED, not how it is dispatched.
func TestOfflineBootKeepsAnAdoptedScreenAlive(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	store, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	certPEM, priv := newTestRelayIdentity(t)

	// ---- first process: an online apply adopts the screen under generation 12
	persistAppliedGeneration(t, store, desiredstate.Applied{
		Generation:      12,
		DeviceInventory: adoptionFor(t, dcNativeID, true),
	})
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close(): %v", err)
	}

	// ---- restart, with no app peer in existence ----
	reopened, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open (reopen): %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	srv, err := playerserver.NewServer(certPEM, nil, playerserver.WallClockMs)
	if err != nil {
		t.Fatalf("NewServer (restarted): %v", err)
	}
	srv.SetSigningKey(priv)
	srv.EnablePersistence(reopened)

	// main's offline boot, verbatim: no connection, so `applied` is the zero
	// value and everything installed comes from the durable row.
	offline := desiredstate.Applied{}
	offline, err = installPersistedServingState(reopened, srv, offline)
	if err != nil {
		t.Fatalf("installPersistedServingState (restart): %v", err)
	}
	if got := len(offline.DeviceInventory.Devices); got != 1 {
		t.Fatalf("offline boot carried %d adopted device(s), want 1 — the relay came back with an EMPTY adopted set, so nothing it had been told to drive is drivable and keep-alive has nothing to watch (REL-055/061/063)", got)
	}

	// The keep-alive adoption gate, seeded exactly as main seeds it from the
	// generation this boot applied.
	adoption := keepalive.NewAdoptionSet()
	adoption.Apply(offline.Generation, offline.DeviceInventory)

	entityID := entityIDOf(dcNativeID)
	if !adoption.IsAdopted(entityID) {
		t.Fatalf("the adoption gate does not adopt %s after an offline boot; keep-alive may drive nothing", entityID)
	}

	// ---- and a screen stuck at Home is actually recovered ----
	ctrl := &recordingController{}
	k := keepalive.New(keepalive.Config{
		Adopted:      adoption.IsAdopted,
		Targets:      map[string]keepalive.Target{entityID: newHomeScreenECP(t)},
		PollInterval: 20 * time.Millisecond,
		LaunchDelay:  time.Millisecond,
		Controller:   ctrl,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = k.Run(ctx) }()

	want := entityID + "/launch"
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if containsCall(ctrl.calls(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("keep-alive dispatched %v within the deadline, want a %q — after a power cut the relay boots, the screens are already sitting at Home, and this is the relaunch that gets a picture back on them without a human walking past",
		ctrl.calls(), want)
}

// containsCall reports whether calls holds want.
func containsCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}
