package ecp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// deviceinfo_test.go drives the identification probe against real HTTP servers
// (httptest), not a mocked client: the thing under test is what happens when a
// host on the LAN answers in a particular way, and a mock would let the parser
// pass against responses no device produces.

// rokuDeviceInfoXML is a real Roku `/query/device-info` document, trimmed to the
// elements this probe reads plus a couple it deliberately ignores — so the test
// proves the decode is tolerant of the elements it does not know rather than
// only of a document shaped exactly like its own struct.
const rokuDeviceInfoXML = `<?xml version="1.0" encoding="UTF-8" ?>
<device-info>
	<udn>29b9b02c-1b0c-5a76-93cf-0025b6df1b8a</udn>
	<serial-number>X00500ABC123</serial-number>
	<device-id>S01234567890</device-id>
	<vendor-name>Roku</vendor-name>
	<model-name>Roku Ultra</model-name>
	<model-number>4800X</model-number>
	<friendly-device-name>Roku Ultra - Living Room</friendly-device-name>
	<default-device-name>Roku Ultra - X00500ABC123</default-device-name>
	<user-device-name>Lobby TV</user-device-name>
	<power-mode>PowerOn</power-mode>
	<supports-suspend>true</supports-suspend>
</device-info>`

// hangerDeviceInfoXML is the document the lab's only Roku actually answers —
// 192.168.50.31, an onn-branded Roku TV on firmware 15.3.4 — captured 2026-08-20
// and trimmed from the 80 child elements it really sends to the ones this probe
// reads plus several it ignores. Only the MACs and the advertising id were
// dropped rather than trimmed for brevity: they identify a specific household on
// a public repo and prove nothing.
//
// It is here because rokuDeviceInfoXML above is the ONE document shape in which
// #207 is invisible: its `model-name` is already human-readable, so reading the
// wrong element costs nothing and the gate stays green over the defect. This is
// the only document in the tree captured from a device anyone has actually read
// — one device, one vendor, one firmware — and on it `model-name` is a retail
// part number while the only human-readable model string is
// `friendly-model-name`.
const hangerDeviceInfoXML = `<?xml version="1.0" encoding="UTF-8" ?>
<device-info>
	<udn>28002240-0000-1000-8009-c48b66682125</udn>
	<serial-number>X029009JC6LF</serial-number>
	<device-id>S188Y4CJC6LF</device-id>
	<vendor-name>onn</vendor-name>
	<model-name>100012587</model-name>
	<model-number>L810X</model-number>
	<model-region>US</model-region>
	<is-tv>true</is-tv>
	<screen-size>65</screen-size>
	<friendly-device-name>The Hanger</friendly-device-name>
	<friendly-model-name>onn•Roku TV</friendly-model-name>
	<default-device-name>onn•Roku TV - X029009JC6LF</default-device-name>
	<user-device-name>The Hanger</user-device-name>
	<software-version>15.3.4</software-version>
	<power-mode>PowerOn</power-mode>
</device-info>`

// serveDeviceInfo stands up an httptest server answering GET on the ECP
// device-info path with body, and returns its host:port authority.
func serveDeviceInfo(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != deviceInfoPath {
			t.Errorf("probe requested %s %s, want GET %s", r.Method, r.URL.Path, deviceInfoPath)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestQueryDeviceInfoReadsARealRokuDocument(t *testing.T) {
	addr := serveDeviceInfo(t, http.StatusOK, rokuDeviceInfoXML)

	info, err := QueryDeviceInfo(context.Background(), NewIdentifyClient(), addr)
	if err != nil {
		t.Fatalf("QueryDeviceInfo: %v", err)
	}
	// user-device-name wins: it is what the OWNER named the device, which is
	// what an operator matching a list row to a physical TV recognizes.
	if got, want := info.Name, "Lobby TV"; got != want {
		t.Errorf("Name = %q, want %q (the owner-set name outranks the vendor's)", got, want)
	}
	if got, want := info.Model, "Roku Ultra"; got != want {
		t.Errorf("Model = %q, want %q", got, want)
	}
	if got, want := info.SerialNumber, "X00500ABC123"; got != want {
		t.Errorf("SerialNumber = %q, want %q", got, want)
	}
}

func TestQueryDeviceInfoFallsBackThroughTheNameCandidates(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "no user name falls back to the friendly name",
			doc:  `<device-info><friendly-device-name>Roku Ultra - Bar</friendly-device-name><default-device-name>Roku Ultra - X1</default-device-name></device-info>`,
			want: "Roku Ultra - Bar",
		},
		{
			name: "neither falls back to the factory default",
			doc:  `<device-info><default-device-name>Roku Ultra - X1</default-device-name></device-info>`,
			want: "Roku Ultra - X1",
		},
		{
			// Roku pretty-prints some elements onto their own lines; an
			// untrimmed read turns a present name into one that renders blank.
			name: "surrounding whitespace is trimmed",
			doc:  "<device-info><user-device-name>\n\t  Back Bar TV  \n</user-device-name></device-info>",
			want: "Back Bar TV",
		},
		{
			// A blank element is not a name. Falling through is what stops the
			// device being listed with an empty label when a real one exists.
			name: "an empty element falls through",
			doc:  `<device-info><user-device-name></user-device-name><friendly-device-name>Roku Ultra - Bar</friendly-device-name></device-info>`,
			want: "Roku Ultra - Bar",
		},
		{
			name: "no name at all is empty, not an error",
			doc:  `<device-info><serial-number>X1</serial-number></device-info>`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := serveDeviceInfo(t, http.StatusOK, tc.doc)
			info, err := QueryDeviceInfo(context.Background(), NewIdentifyClient(), addr)
			if err != nil {
				t.Fatalf("QueryDeviceInfo: %v", err)
			}
			if info.Name != tc.want {
				t.Errorf("Name = %q, want %q", info.Name, tc.want)
			}
		})
	}
}

func TestQueryDeviceInfoModelFallsBackToTheNumber(t *testing.T) {
	addr := serveDeviceInfo(t, http.StatusOK, `<device-info><model-number>4800X</model-number></device-info>`)

	info, err := QueryDeviceInfo(context.Background(), NewIdentifyClient(), addr)
	if err != nil {
		t.Fatalf("QueryDeviceInfo: %v", err)
	}
	if got, want := info.Model, "4800X"; got != want {
		t.Errorf("Model = %q, want %q — a number is a worse label than a name and a much better one than nothing", got, want)
	}
}

// TestQueryDeviceInfoReadsTheHumanReadableModel is #207 against the device that
// exposed it. The probe used to decode six elements out of the document's 79, and
// `friendly-model-name` — the only one of the three model elements a person can
// read — was in the 73 it ignored. An operator adopting this TV was shown
// "100012587", a Walmart part number, for a device whose model is "onn•Roku TV".
//
// The serial is asserted alongside deliberately. The same document carries
// `<device-id>S188Y4CJC6LF</device-id>`, which looks like a serial, sits two lines
// away from one, and is NOT one — it is Roku's own cloud identifier for the unit.
// Reading model precedence next door is exactly when someone would be tempted to
// give the serial a fallback candidate too, and this pins that it has none.
func TestQueryDeviceInfoReadsTheHumanReadableModel(t *testing.T) {
	addr := serveDeviceInfo(t, http.StatusOK, hangerDeviceInfoXML)

	info, err := QueryDeviceInfo(context.Background(), NewIdentifyClient(), addr)
	if err != nil {
		t.Fatalf("QueryDeviceInfo: %v", err)
	}
	if got, want := info.Model, "onn•Roku TV"; got != want {
		t.Errorf("Model = %q, want %q — friendly-model-name is the only human-readable model element this device sends", got, want)
	}
	if got, want := info.SerialNumber, "X029009JC6LF"; got != want {
		t.Errorf("SerialNumber = %q, want %q (serial-number, never the device-id beside it)", got, want)
	}
	if got, want := info.Name, "The Hanger"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got, want := info.NameSource, NameSourceUser; got != want {
		t.Errorf("NameSource = %d, want %d", got, want)
	}
}

// modelCase drives one document through the probe and asserts the resolved
// Model. Shared by the two tests below, which are split by a property the table
// alone cannot express: whether the case can detect #207 at all.
type modelCase struct {
	name string
	doc  string
	want string
}

func runModelCases(t *testing.T, cases []modelCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := serveDeviceInfo(t, http.StatusOK, tc.doc)
			info, err := QueryDeviceInfo(context.Background(), NewIdentifyClient(), addr)
			if err != nil {
				t.Fatalf("QueryDeviceInfo: %v", err)
			}
			if info.Model != tc.want {
				t.Errorf("Model = %q, want %q", info.Model, tc.want)
			}
		})
	}
}

// TestQueryDeviceInfoModelPrefersTheFriendlyModelName is the half of the ladder
// that DETECTS #207. Every case here contains a usable `friendly-model-name`, so
// every case fails against the pre-fix decoder — verified by reverting
// deviceinfo.go to HEAD and running: all four report the weaker element's value.
//
// That property is the point of the split. The rungs below the new one behave
// identically before and after the change by construction, so a table mixing
// them in reads as broad coverage while most of its rows would pass on a full
// revert. Those rows still earn their place — they are the argument that reading
// an undocumented element is safe — but they live in the test below, under a name
// that says what they can and cannot prove.
func TestQueryDeviceInfoModelPrefersTheFriendlyModelName(t *testing.T) {
	runModelCases(t, []modelCase{
		{
			// The defect in one line: both weaker elements are present and
			// neither may win while the readable one is there.
			name: "the friendly model outranks the part number and the hardware code",
			doc:  `<device-info><model-name>100012587</model-name><model-number>L810X</model-number><friendly-model-name>onn•Roku TV</friendly-model-name></device-info>`,
			want: "onn•Roku TV",
		},
		{
			// The second rung must lose too, not just the first: a document with
			// no model-name at all still may not fall to the catalog code.
			name: "the friendly model outranks the hardware code on its own",
			doc:  `<device-info><model-number>3930X</model-number><friendly-model-name>Roku Express</friendly-model-name></device-info>`,
			want: "Roku Express",
		},
		{
			// Roku pretty-prints elements onto their own lines. Pre-fix this
			// document resolved to "" — the element was not read at all.
			name: "surrounding whitespace is trimmed",
			doc:  "<device-info><friendly-model-name>\n\t  onn•Roku TV  \n</friendly-model-name></device-info>",
			want: "onn•Roku TV",
		},
		{
			// The cap's INCLUSIVE edge. pickField skips only what exceeds the cap,
			// so a value exactly at it is a legal model and must be used rather
			// than falling through to the weaker element sitting beside it. This
			// is the case that pins the boundary: an off-by-one to `>=` turns it
			// into "Roku Ultra".
			name: "a friendly model exactly at the byte cap is used",
			doc:  `<device-info><friendly-model-name>` + strings.Repeat("M", maxDeviceInfoFieldBytes) + `</friendly-model-name><model-name>Roku Ultra</model-name></device-info>`,
			want: strings.Repeat("M", maxDeviceInfoFieldBytes),
		},
	})
}

// TestQueryDeviceInfoModelDegradesToThePreviousBehaviour is the other half, and
// it is stated plainly: EVERY case here passes on a full revert of #207, and
// that is the property being asserted rather than a weakness in the test.
//
// Reading `friendly-model-name` is reading an element Roku's published
// device-info reference does not list, on the evidence of exactly one device.
// The risk that carries is a firmware that omits it or fills it with junk, and
// the mitigation is that it sits BEHIND a fall-through. These cases pin that
// mitigation: on every document where the new rung is unusable, the probe must
// return byte-for-byte what it returned before the rung existed. A future change
// that made the top rung mandatory, or that truncated an over-long value into a
// plausible-looking model, would break these and nothing in the test above.
//
// Do not "strengthen" these into failing on revert. They cannot, by
// construction, and a case rewritten until it does would be asserting something
// other than the degradation guarantee.
func TestQueryDeviceInfoModelDegradesToThePreviousBehaviour(t *testing.T) {
	runModelCases(t, []modelCase{
		{
			// Roku-branded hardware, and the shape rokuDeviceInfoXML has: no
			// friendly model at all, and model-name is already a real name.
			name: "no friendly model falls back to the model name",
			doc:  `<device-info><model-name>Roku Ultra</model-name><model-number>4800X</model-number></device-info>`,
			want: "Roku Ultra",
		},
		{
			name: "neither falls back to the model number",
			doc:  `<device-info><model-number>3930X</model-number></device-info>`,
			want: "3930X",
		},
		{
			// A blank element is not a model. Falling through is what stops a
			// device being listed with an empty Model column when a real one
			// exists two elements down.
			name: "an empty friendly model falls through",
			doc:  `<device-info><friendly-model-name></friendly-model-name><model-name>Roku Ultra</model-name></device-info>`,
			want: "Roku Ultra",
		},
		{
			// The cap SKIPS rather than truncates. A truncated model reads as a
			// real one, which is worse than falling to the honest weaker element.
			name: "an over-cap friendly model is skipped, not truncated",
			doc:  `<device-info><friendly-model-name>` + strings.Repeat("M", maxDeviceInfoFieldBytes+1) + `</friendly-model-name><model-name>Roku Ultra</model-name></device-info>`,
			want: "Roku Ultra",
		},
		{
			name: "no model at all is empty, not an error",
			doc:  `<device-info><serial-number>X1</serial-number></device-info>`,
			want: "",
		},
	})
}

// TestQueryDeviceInfoRefusesNonRokuAnswers is the CLASSIFICATION half: an error
// means "this address is not a confirmed Roku", and each of these is a way a
// LAN host can answer the search target without speaking ECP.
func TestQueryDeviceInfoRefusesNonRokuAnswers(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"non-2xx", http.StatusForbidden, rokuDeviceInfoXML},
		{"not xml at all", http.StatusOK, "<!doctype html><html><body>router admin</body></html>"},
		{"truncated xml", http.StatusOK, "<device-info><serial-number>X1"},
		{"empty body", http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := serveDeviceInfo(t, tc.status, tc.body)
			if info, err := QueryDeviceInfo(context.Background(), NewIdentifyClient(), addr); err == nil {
				t.Errorf("QueryDeviceInfo returned %+v, want an error — an unconfirmed address must not be reported as an identified Roku", info)
			}
		})
	}
}

// TestQueryDeviceInfoCapsTheBody proves the LimitReader bound is real: a host
// that streams forever must not be able to grow this relay's memory, and the
// truncated read fails the decode rather than yielding a half-parsed device.
func TestQueryDeviceInfoCapsTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Well over the cap, and never closed as a valid document.
		chunk := strings.Repeat("<pad>x</pad>", 4096)
		for range 8 {
			if _, err := fmt.Fprint(w, "<device-info>"+chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	if _, err := QueryDeviceInfo(context.Background(), NewIdentifyClient(), addr); err == nil {
		t.Error("QueryDeviceInfo accepted an unbounded body, want an error from the capped read")
	}
}

func TestQueryDeviceInfoRefusesUnreachableAndMissingInputs(t *testing.T) {
	if _, err := QueryDeviceInfo(context.Background(), nil, "192.0.2.1:8060"); err == nil {
		t.Error("QueryDeviceInfo(nil client) = nil error, want a refusal rather than a nil-client panic")
	}
	if _, err := QueryDeviceInfo(context.Background(), NewIdentifyClient(), ""); err == nil {
		t.Error("QueryDeviceInfo(no address) = nil error, want a refusal")
	}

	// A canceled context must return immediately rather than waiting out the
	// client timeout — a sweep that is shutting down must not be held open.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// TEST-NET-1 (RFC 5737): guaranteed not to be a real host on any LAN.
	if _, err := QueryDeviceInfo(ctx, NewIdentifyClient(), "192.0.2.1:8060"); err == nil {
		t.Error("QueryDeviceInfo with a canceled context = nil error, want a refusal")
	}
}

// The name's SOURCE rides out beside the name, because a merge one layer up
// cannot otherwise tell "Lobby TV", which an owner typed, from
// "Roku Ultra - X00500ABC123", which a factory printed. The precedence used to
// be resolved and thrown away, and every downstream fact was equally trustworthy
// as a result (internal/relay/deviceplane.NameRank).
func TestQueryDeviceInfoReportsWhichElementNamedTheDevice(t *testing.T) {
	cases := []struct {
		name       string
		doc        string
		wantName   string
		wantSource NameSource
	}{
		{
			name:       "the owner's own name",
			doc:        rokuDeviceInfoXML,
			wantName:   "Lobby TV",
			wantSource: NameSourceUser,
		},
		{
			name:       "the vendor's friendly name",
			doc:        `<device-info><friendly-device-name>Roku Ultra - Bar</friendly-device-name><default-device-name>Roku Ultra - X1</default-device-name></device-info>`,
			wantName:   "Roku Ultra - Bar",
			wantSource: NameSourceFriendly,
		},
		{
			name:       "the factory default, which is a model string wearing a name's clothes",
			doc:        `<device-info><default-device-name>Roku Ultra - X1</default-device-name></device-info>`,
			wantName:   "Roku Ultra - X1",
			wantSource: NameSourceDefault,
		},
		{
			name:       "no name at all",
			doc:        `<device-info><serial-number>X1</serial-number></device-info>`,
			wantName:   "",
			wantSource: NameSourceNone,
		},
		{
			// The source must follow the value through the fall-through rules,
			// not be re-derived from which elements are merely PRESENT.
			name:       "an empty element falls through and the source follows",
			doc:        `<device-info><user-device-name>  </user-device-name><friendly-device-name>Roku Ultra - Bar</friendly-device-name></device-info>`,
			wantName:   "Roku Ultra - Bar",
			wantSource: NameSourceFriendly,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := serveDeviceInfo(t, http.StatusOK, tc.doc)
			info, err := QueryDeviceInfo(context.Background(), NewIdentifyClient(), addr)
			if err != nil {
				t.Fatalf("QueryDeviceInfo: %v", err)
			}
			if info.Name != tc.wantName || info.NameSource != tc.wantSource {
				t.Fatalf("(Name, NameSource) = (%q, %d), want (%q, %d)", info.Name, info.NameSource, tc.wantName, tc.wantSource)
			}
		})
	}
}
