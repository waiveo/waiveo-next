package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	appdevices "github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/devicetargets"
	"github.com/maaxton/waiveo-next/internal/relay/ecppoll"
	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
)

// entitystatechain_test.go drives the WHOLE path a Roku's observed state takes
// to reach an operator's screen, in one test, against one set of fixtures:
//
//	a real ECP response over HTTP
//	  -> ecppoll derives a media-player state.Entity (REG-061/064)
//	  -> devicePlaneSync copies it onto the candidate store (REL-110a)
//	  -> toWireCandidates projects it onto the device.candidates report
//	  -> the app peer's devices.Registry accepts the report (REL-110b)
//	  -> GET /api/v1/entities' own read model reports state + attributes
//
// Every one of those steps already had a test of its own, and the CHAIN had
// none — the shape this repo keeps shipping (see deviceplanesync.go's header,
// which says the same thing about the smaller join inside it). A chain test is
// what catches the failures that live between correct halves: the two sides
// deriving an entity id from different site values, an attribute map that is
// carried but never projected, a report whose `state` is dropped at intake.
//
// It runs entirely in-process against an httptest ECP stand-in, so it needs no
// hardware and no network.

// chainSite / chainDriver / chainNativeID / chainEntityKey are this test's own
// REL-153 identity fixtures. The site is shared between the relay's candidate
// store and the app's registry deliberately: both derive the entity id from it
// (REL-110b), and a chain that agreed on everything except the site would
// deliver a state onto an id nothing ever reads.
const (
	chainSite      = "01J8Z3K4N5P6Q7R8S9T0V1SITE"
	chainDriver    = "roku-ecp"
	chainNativeID  = "uuid:roku:ecp:CHAIN1"
	chainEntityKey = "main"
	chainRelayID   = "relay-chain-1"
)

// ecpStandIn serves the two queries a poll makes, with a Roku that is powered
// on and sitting in a named app — the reading an operator's entity list has to
// be able to show.
func ecpStandIn(t *testing.T) (host string, port int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/query/active-app", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<active-app><app id="12" type="appl" version="4.2.1">Netflix</app></active-app>`))
	})
	mux.HandleFunc("/query/device-info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<device-info><power-mode>PowerOn</power-mode></device-info>`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse stand-in URL: %v", err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse stand-in port: %v", err)
	}
	return u.Hostname(), p
}

// TestObservedEntityStateReachesTheAppsEntityList is the chain.
func TestObservedEntityStateReachesTheAppsEntityList(t *testing.T) {
	host, port := ecpStandIn(t)
	entityID := deviceid.Entity(chainSite, chainDriver, chainNativeID, chainEntityKey)

	// 1. The relay's candidate store, holding the device discovery found.
	candStore := deviceplane.NewStore(chainRelayID)
	candStore.SetSite(chainSite)
	candStore.Observe(deviceplane.Observation{
		Match:       deviceplane.Match{SSDP: "roku:ecp"},
		Provenance:  deviceplane.ProvenanceDiscovered,
		Driver:      chainDriver,
		NativeID:    chainNativeID,
		DeviceClass: "media-player",
		Name:        "Hanger TV",
		Address:     host + ":" + strconv.Itoa(port),
		Entities:    []deviceplane.CandidateEntity{{Key: chainEntityKey, DeviceClass: "media-player"}},
	}, 1000)

	// 2. The real poller, pointed at the stand-in, driven until it has produced
	//    its first observation for this entity. Next() blocking on the poller's
	//    own channel is the completion signal — no sleep, no polling loop.
	poller := ecppoll.New(map[string]ecppoll.Target{entityID: {Host: host, Port: port}}, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go poller.Run(ctx)
	if _, ok := poller.Next(); !ok {
		t.Fatal("the poller's stream closed before it produced an observation")
	}

	// 3. The join the running binary drives on a ticker: re-derive the drivable
	//    set and copy what the poller observed onto the candidate store. The
	//    gate is given the entity as an explicit override, which is what the
	//    WAIVEO_RELAY_ECP_TARGETS escape hatch does — adoption is proven
	//    elsewhere and is not what this test is about.
	gate := devicetargets.New(map[string]devicetargets.Endpoint{entityID: {Host: host, Port: port}}, candStore)
	devicePlaneSync{gate: gate, poller: poller, states: candStore}.tick()

	// 4. The report the relay would send upward, in its wire shape.
	report := toWireCandidates(candStore.Report().Body.Candidates)
	if len(report) != 1 {
		t.Fatalf("report carries %d candidate(s), want 1", len(report))
	}

	// 5. The app peer accepting it into the read model GET /api/v1/entities
	//    serves. relayID is the connection's AUTHENTICATED identity on the real
	//    path; here it is simply the relay that produced the report.
	appReg := appdevices.New(chainSite, func() int64 { return 2000 })
	if err := appReg.ApplyCandidates(chainRelayID, report); err != nil {
		t.Fatalf("the app peer refused the relay's report: %v", err)
	}

	// 6. What an operator reads.
	entity, found := appReg.Entity(entityID)
	if !found {
		t.Fatalf("the app peer's entity list has no %s; the two sides derived different ids from the same identity (REL-110b)", entityID)
	}
	if entity.State != "on" {
		t.Fatalf("entity state = %q, want on — a powered Roku in a named app (REG-061)", entity.State)
	}
	// The attributes are the half that makes the console useful: "on" does not
	// tell an operator their screen is showing Netflix instead of the slidecast.
	if got := entity.Attributes["power_mode"]; got != "PowerOn" {
		t.Errorf("attributes[power_mode] = %q, want PowerOn", got)
	}
	if got := entity.Attributes["active_app"]; got != "Netflix" {
		t.Errorf("attributes[active_app] = %q, want Netflix", got)
	}
	if got := entity.Attributes["app_type"]; got != "app" {
		t.Errorf("attributes[app_type] = %q, want app", got)
	}
	// A bool attribute crosses the wire as text (wire.CandidateEntity's own
	// doc), and a nil one is dropped rather than rendered as "<nil>".
	if got := entity.Attributes["is_screensaver"]; got != "false" {
		t.Errorf("attributes[is_screensaver] = %q, want the string false", got)
	}
	if _, present := entity.Attributes["active_app_id"]; !present {
		t.Errorf("attributes = %+v, want the active app's id carried too", entity.Attributes)
	}
}

// TestUnreachableDeviceReachesTheAppAsUnavailable is the same chain with the
// device off the LAN. It matters because the honest answer — "unavailable" —
// is what an operator needs in order to tell "the screen is showing the wrong
// thing" from "the screen is not answering at all", and because the whole
// attribute map is nil'd on that path (ecppoll's unavailableEntity), which is
// the case where a projection that assumed a non-empty map would panic.
func TestUnreachableDeviceReachesTheAppAsUnavailable(t *testing.T) {
	// A stand-in that is created and immediately closed: the port is bound to
	// nothing, so every ECP GET fails to connect — a real unreachable device
	// rather than one faked by a flag.
	ts := httptest.NewServer(http.NewServeMux())
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	host := u.Hostname()
	ts.Close()

	entityID := deviceid.Entity(chainSite, chainDriver, chainNativeID, chainEntityKey)

	candStore := deviceplane.NewStore(chainRelayID)
	candStore.SetSite(chainSite)
	candStore.Observe(deviceplane.Observation{
		Match:       deviceplane.Match{SSDP: "roku:ecp"},
		Provenance:  deviceplane.ProvenanceDiscovered,
		Driver:      chainDriver,
		NativeID:    chainNativeID,
		DeviceClass: "media-player",
		Name:        "Hanger TV",
		Entities:    []deviceplane.CandidateEntity{{Key: chainEntityKey, DeviceClass: "media-player"}},
	}, 1000)

	poller := ecppoll.New(map[string]ecppoll.Target{entityID: {Host: host, Port: port}}, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go poller.Run(ctx)
	if _, ok := poller.Next(); !ok {
		t.Fatal("the poller's stream closed before it produced an observation")
	}

	gate := devicetargets.New(map[string]devicetargets.Endpoint{entityID: {Host: host, Port: port}}, candStore)
	devicePlaneSync{gate: gate, poller: poller, states: candStore}.tick()

	appReg := appdevices.New(chainSite, func() int64 { return 2000 })
	if err := appReg.ApplyCandidates(chainRelayID, toWireCandidates(candStore.Report().Body.Candidates)); err != nil {
		t.Fatalf("the app peer refused the relay's report: %v", err)
	}
	entity, found := appReg.Entity(entityID)
	if !found {
		t.Fatalf("the app peer's entity list has no %s", entityID)
	}
	if entity.State != "unavailable" {
		t.Fatalf("entity state = %q, want unavailable — a device that did not answer must not read as off", entity.State)
	}
	if got := entity.Attributes["is_screensaver"]; got != "false" {
		t.Errorf("attributes[is_screensaver] = %q, want the string false — the one attribute REG-064 says is never nil", got)
	}
}
