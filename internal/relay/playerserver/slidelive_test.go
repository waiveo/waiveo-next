package playerserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
	"github.com/maaxton/waiveo-next/internal/slidelive"
)

// These cases cover the ONE place a native slide's server-resolved widgets get
// their values: Lease issuance. They are here rather than in internal/slidelive
// because what they prove is the WIRING — that a served program's slide reaches
// a player with its weather and entity text filled, from the live sources this
// server was configured with, without the served program itself being altered.

// fixedWeather answers a constant reading, standing in for the real
// (cached, background-refreshing) Open-Meteo source.
type fixedWeather struct {
	conds slidelive.Conditions
	ok    bool
}

func (f fixedWeather) Current(context.Context, float64, float64) (slidelive.Conditions, bool) {
	return f.conds, f.ok
}

// fixedEntities answers from a map, standing in for the relay's automation host.
type fixedEntities map[string]string

func (f fixedEntities) EntityState(id string) (string, bool) {
	v, ok := f[id]
	return v, ok
}

// liveWidgetLayers is a slide carrying one of each server-resolved kind
// alongside a static text layer, so a case can assert that only the live ones
// are touched.
func liveWidgetLayers() []wire.Layer {
	return []wire.Layer{
		{Kind: wire.LayerKindText, X: 0, Y: 0, W: 800, H: 100, Text: "The Hanger"},
		{Kind: wire.LayerKindWeather, X: 0, Y: 120, W: 800, H: 100, Text: "{temp}° {cond}"},
		{Kind: wire.LayerKindEntity, X: 0, Y: 240, W: 800, H: 100, Text: "Lobby: {state}", EntityID: "01J8Z3K4N5P6Q7R8S9T0V1SCRN"},
	}
}

func TestLeaseResolvesLiveWidgetValuesAtIssuance(t *testing.T) {
	srv, token := newSlideServer(t, liveWidgetLayers())
	srv.SetSlideLive(slidelive.Sources{
		Weather: fixedWeather{conds: slidelive.Conditions{TempF: 84, TempC: 29, Text: "Clear"}, ok: true},
		Entity:  fixedEntities{"01J8Z3K4N5P6Q7R8S9T0V1SCRN": "playing"},
		Lat:     39.7392,
		Long:    -104.9903,
	})

	resp, body := doProgram(t, srv, token, []string{"image", "video", "slide"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	var lease LeaseResponse
	remarshal(t, body, &lease)

	layers := lease.Content[0].Layers
	if len(layers) != 3 {
		t.Fatalf("expected the three authored layers, got %+v", layers)
	}
	if got, want := layers[1].Value, "84° Clear"; got != want {
		t.Fatalf("weather value = %q, want %q", got, want)
	}
	if got, want := layers[2].Value, "Lobby: playing"; got != want {
		t.Fatalf("entity value = %q, want %q", got, want)
	}
	// A static layer must be handed over exactly as authored — resolution is
	// additive to the live kinds, not a rewrite of the stack.
	if layers[0].Value != "" || layers[0].Text != "The Hanger" {
		t.Fatalf("the static text layer was altered: %+v", layers[0])
	}
}

func TestLeaseResolvesLiveWidgetsFreshOnEveryPull(t *testing.T) {
	// This is the whole point of resolving at ISSUANCE rather than in a
	// projection: nothing upstream re-runs when the weather changes, so if the
	// value were baked in earlier a screen would show the reading from whenever
	// the generation was built, forever. Two pulls with a changed source must
	// therefore yield two different Leases with no program write in between.
	srv, token := newSlideServer(t, liveWidgetLayers())

	srv.SetSlideLive(slidelive.Sources{
		Weather: fixedWeather{conds: slidelive.Conditions{TempF: 84, Text: "Clear"}, ok: true},
		Entity:  fixedEntities{"01J8Z3K4N5P6Q7R8S9T0V1SCRN": "off"},
	})
	_, first := doProgram(t, srv, token, []string{"slide"})
	var leaseA LeaseResponse
	remarshal(t, first, &leaseA)

	srv.SetSlideLive(slidelive.Sources{
		Weather: fixedWeather{conds: slidelive.Conditions{TempF: 61, Text: "Light rain"}, ok: true},
		Entity:  fixedEntities{"01J8Z3K4N5P6Q7R8S9T0V1SCRN": "playing"},
	})
	_, second := doProgram(t, srv, token, []string{"slide"})
	var leaseB LeaseResponse
	remarshal(t, second, &leaseB)

	if got, want := leaseA.Content[0].Layers[1].Value, "84° Clear"; got != want {
		t.Fatalf("first pull weather = %q, want %q", got, want)
	}
	if got, want := leaseB.Content[0].Layers[1].Value, "61° Light rain"; got != want {
		t.Fatalf("second pull weather = %q, want %q", got, want)
	}
	if got, want := leaseB.Content[0].Layers[2].Value, "Lobby: playing"; got != want {
		t.Fatalf("second pull entity = %q, want %q", got, want)
	}
	// The signature covers the content, so a genuinely different Lease must
	// carry a genuinely different signature — a player verifies what it draws.
	if leaseA.Signature == leaseB.Signature {
		t.Fatalf("two Leases with different resolved values shared a signature")
	}
}

func TestLeaseIssuanceDoesNotMutateTheServedProgram(t *testing.T) {
	// prog.Content is the server's retained state, reused for every subsequent
	// poll. A resolved value written into it would permanently replace the
	// AUTHORED layer with one poll's answer, so the next pull could never
	// re-resolve — the widget would freeze at its first reading.
	srv, token := newSlideServer(t, liveWidgetLayers())
	srv.SetSlideLive(slidelive.Sources{
		Weather: fixedWeather{conds: slidelive.Conditions{TempF: 84, Text: "Clear"}, ok: true},
	})
	doProgram(t, srv, token, []string{"slide"})

	srv.mu.Lock()
	stored := srv.programs[slideScreenID].Content[0].Layers
	srv.mu.Unlock()
	for i, l := range stored {
		if l.Value != "" {
			t.Fatalf("served program layer %d retained a resolved value %q", i, l.Value)
		}
	}
}

func TestLeaseDegradesLiveWidgetsWithNoSourcesConfigured(t *testing.T) {
	// The default state of a Server: no SetSlideLive at all (every existing test
	// and conformance construction). A slide must still be served, and its live
	// widgets must show the unavailable placeholder rather than nothing — a
	// blank label reads on a wall as a rendering bug.
	srv, token := newSlideServer(t, liveWidgetLayers())

	resp, body := doProgram(t, srv, token, []string{"slide"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	var lease LeaseResponse
	remarshal(t, body, &lease)

	layers := lease.Content[0].Layers
	if got, want := layers[1].Value, slidelive.Unavailable+"° "+slidelive.Unavailable; got != want {
		t.Fatalf("degraded weather = %q, want %q", got, want)
	}
	if got, want := layers[2].Value, "Lobby: "+slidelive.Unavailable; got != want {
		t.Fatalf("degraded entity = %q, want %q", got, want)
	}
}

func TestLiveResolutionSkipsASlideThePlayerCannotDraw(t *testing.T) {
	// Resolution runs AFTER the capability filter, so a player that declares no
	// "slide" support costs no weather lookup at all — and, more visibly, is
	// still served no slide.
	srv, token := newSlideServer(t, liveWidgetLayers())
	asked := 0
	srv.SetSlideLive(slidelive.Sources{Weather: countingWeather{n: &asked}})

	_, body := doProgram(t, srv, token, []string{"image", "video"})
	var lease LeaseResponse
	remarshal(t, body, &lease)

	if len(lease.Content) != 0 {
		t.Fatalf("a player declaring no slide support must be served none, got %+v", lease.Content)
	}
	if asked != 0 {
		t.Fatalf("a filtered-out slide must cost no weather lookup, got %d", asked)
	}
}

func TestLiveResolutionCoversTheScheduleResolvedPathToo(t *testing.T) {
	// The two producers reach this server through DIFFERENT setters: the
	// app-signed baseline through SetServedProgram (every case above) and the
	// relay's own daypart re-resolver, internal/relay/schedulehost, through
	// SetProgram. Resolving at ISSUANCE is what makes one implementation cover
	// both — a screen governed by a carried schedule must not lose its live
	// widgets the moment a daypart boundary re-writes its program.
	srv, token := newSlideServer(t, liveWidgetLayers())
	srv.SetSlideLive(slidelive.Sources{
		Weather: fixedWeather{conds: slidelive.Conditions{TempF: 47, Text: "Fog"}, ok: true},
		Entity:  fixedEntities{"01J8Z3K4N5P6Q7R8S9T0V1SCRN": "off"},
	})

	// A schedulehost-shaped write: Lease content directly, not a ContentRef.
	srv.SetProgram(2, slideScreenID, "rev-daypart-2", "scheduled", "content",
		[]wire.LeaseContent{{Type: "slide", Layers: liveWidgetLayers()}})

	_, body := doProgram(t, srv, token, []string{"slide"})
	var lease LeaseResponse
	remarshal(t, body, &lease)

	if lease.ProgramRevision != "rev-daypart-2" {
		t.Fatalf("setup: expected the re-resolved program, got %q", lease.ProgramRevision)
	}
	if got, want := lease.Content[0].Layers[1].Value, "47° Fog"; got != want {
		t.Fatalf("weather on the schedule-resolved path = %q, want %q", got, want)
	}
	if got, want := lease.Content[0].Layers[2].Value, "Lobby: off"; got != want {
		t.Fatalf("entity on the schedule-resolved path = %q, want %q", got, want)
	}
}

// countingWeather records how many times it was consulted and never answers.
type countingWeather struct{ n *int }

func (c countingWeather) Current(context.Context, float64, float64) (slidelive.Conditions, bool) {
	*c.n++
	return slidelive.Conditions{}, false
}
