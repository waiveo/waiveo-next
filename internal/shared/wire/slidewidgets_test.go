package wire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The live-widget layer kinds (date/countdown/weather/entity) added on top of
// the v1 four. lease_test.go already pins the v1 kinds and the geometry/color
// rules they share; this file covers what is NEW — each new kind's own
// required-field rule, the closed-set boundary now that the set has eight
// members, and the wire shape of the fields those kinds introduced.

// baseLayer is a geometrically valid layer of the given kind — every test below
// starts from one so a failure is unambiguously about the per-kind rule under
// test and not about geometry.
func baseLayer(kind string) Layer {
	return Layer{Kind: kind, X: 10, Y: 10, W: 100, H: 50}
}

func TestValidateSlideLayersAcceptsEveryLiveWidgetKind(t *testing.T) {
	date := baseLayer(LayerKindDate)
	date.Text = "Monday, January 2"

	countdown := baseLayer(LayerKindCountdown)
	countdown.TargetMS = 1798761600000

	weather := baseLayer(LayerKindWeather)
	weather.Text = "{temp}° {cond}"

	entity := baseLayer(LayerKindEntity)
	entity.EntityID = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"

	if err := ValidateSlideLayers([]Layer{date, countdown, weather, entity}); err != nil {
		t.Fatalf("a well-formed stack of all four live-widget kinds must validate, got %v", err)
	}
}

func TestValidateSlideLayersRejectsMissingPerKindFields(t *testing.T) {
	// Each case is a layer of a live-widget kind with its OWN required field
	// absent and nothing else wrong, plus the substring the message must name so
	// a producer reading a rejection knows which field to author.
	cases := []struct {
		name  string
		layer func() Layer
		want  string
	}{
		{
			name: "date without a format",
			// A date with no layout draws nothing at all, which is a producer
			// bug and not "the default date format" — there is no default.
			layer: func() Layer { return baseLayer(LayerKindDate) },
			want:  "date format",
		},
		{
			name: "countdown without a target",
			// The zero value of an absent int64 field, NOT the Unix epoch as an
			// authored target: a countdown to it would render as finished
			// forever, silently.
			layer: func() Layer { return baseLayer(LayerKindCountdown) },
			want:  "target_ms",
		},
		{
			name: "countdown with a negative target",
			layer: func() Layer {
				l := baseLayer(LayerKindCountdown)
				l.TargetMS = -1
				return l
			},
			want: "target_ms",
		},
		{
			name:  "weather without a template",
			layer: func() Layer { return baseLayer(LayerKindWeather) },
			want:  "display template",
		},
		{
			name:  "entity without a subject",
			layer: func() Layer { return baseLayer(LayerKindEntity) },
			want:  "entity_id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSlideLayers([]Layer{tc.layer()})
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rejection must name the missing field %q, got %q", tc.want, err.Error())
			}
		})
	}
}

func TestValidateSlideLayersAcceptsAPastCountdownTarget(t *testing.T) {
	// A slide outliving its own event is a SCHEDULING matter, not a malformed
	// layer: the player renders a finished countdown as zeroes. Rejecting it
	// here would blank the whole slide the moment the event passed.
	l := baseLayer(LayerKindCountdown)
	l.TargetMS = 1
	if err := ValidateSlideLayers([]Layer{l}); err != nil {
		t.Fatalf("a target in the past is valid data, got %v", err)
	}
}

func TestValidateSlideLayersDoesNotRequireResolvedValue(t *testing.T) {
	// The authoring-time shape: a weather and an entity layer with NO Value,
	// exactly as they sit in an authored row and in both upstream projections.
	// Requiring Value here would mean an unreachable forecast service drops the
	// whole slide — title, image, clock and all.
	weather := baseLayer(LayerKindWeather)
	weather.Text = "{temp}"
	entity := baseLayer(LayerKindEntity)
	entity.EntityID = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"

	if err := ValidateSlideLayers([]Layer{weather, entity}); err != nil {
		t.Fatalf("an unresolved live widget must validate, got %v", err)
	}
}

func TestValidateSlideLayersKeepsTheKindSetClosed(t *testing.T) {
	// The set grew from four to eight; it is still CLOSED. A kind no player has
	// a draw path for must not reach the wire, and the rejection must enumerate
	// what IS legal so the message stays useful as the set grows.
	l := baseLayer("stocks")
	err := ValidateSlideLayers([]Layer{l})
	if err == nil {
		t.Fatalf("an unknown kind must be rejected")
	}
	for _, kind := range slideLayerKinds {
		if !strings.Contains(err.Error(), kind) {
			t.Fatalf("rejection must enumerate every legal kind; %q missing from %q", kind, err.Error())
		}
	}
}

func TestLiveWidgetFieldsAreOmittedWhenUnused(t *testing.T) {
	// The additive fields must not appear on a layer that does not use them:
	// every pre-existing v1 layer has to marshal byte-identically to before
	// these kinds existed, because those bytes ride the Lease signature.
	l := baseLayer(LayerKindText)
	l.Text = "hello"
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, key := range []string{"target_ms", "entity_id", "value"} {
		if strings.Contains(got, key) {
			t.Fatalf("a text layer must carry no %q key, got %s", key, got)
		}
	}
	if got != `{"kind":"text","x":10,"y":10,"w":100,"h":50,"text":"hello"}` {
		t.Fatalf("a v1 text layer's bytes changed: %s", got)
	}
}

func TestLiveWidgetFieldsRoundTrip(t *testing.T) {
	// The names on the wire are the contract with the player's renderer, which
	// reads them by string. A rename here is a silently-blank widget there.
	in := Layer{
		Kind: LayerKindEntity, X: 1, Y: 2, W: 3, H: 4,
		Text: "Screen: {state}", EntityID: "01J8Z3K4N5P6Q7R8S9T0V1SCRN", Value: "on",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `"entity_id":"01J8Z3K4N5P6Q7R8S9T0V1SCRN"`; !strings.Contains(string(b), want) {
		t.Fatalf("missing %s in %s", want, b)
	}
	if want := `"value":"on"`; !strings.Contains(string(b), want) {
		t.Fatalf("missing %s in %s", want, b)
	}

	var out Layer
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// reflect.DeepEqual rather than ==: Layer gained a slice member (Items, the
	// nav menu) and is no longer a comparable struct. The assertion is the same
	// one — every member round-trips unchanged.
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip changed the layer: %+v vs %+v", out, in)
	}
}

func TestLiveWidgetLayersRideTheLeaseSignature(t *testing.T) {
	// A resolved value must be INSIDE the signed scope: a Lease whose weather
	// text could be swapped without breaking the signature would let anything on
	// the LAN rewrite what a screen displays. Two Leases differing only in a
	// layer's resolved value must therefore produce different signed bytes.
	mk := func(value string) Lease {
		return Lease{
			LeaseID: "L", ScreenID: "S", ProgramRevision: "R", Priority: "scheduled", Display: "content",
			Content: []LeaseContent{{
				Type: "slide",
				Layers: []Layer{{
					Kind: LayerKindWeather, X: 0, Y: 0, W: 100, H: 50,
					Text: "{temp}", Value: value,
				}},
			}},
			IssuedAt: 1, ValidUntil: 2,
		}
	}
	a, err := LeaseSignedBytes(mk("72"))
	if err != nil {
		t.Fatalf("signed bytes: %v", err)
	}
	b, err := LeaseSignedBytes(mk("31"))
	if err != nil {
		t.Fatalf("signed bytes: %v", err)
	}
	if string(a) == string(b) {
		t.Fatalf("a resolved widget value must be covered by the signature; both leases signed %s", a)
	}
}
