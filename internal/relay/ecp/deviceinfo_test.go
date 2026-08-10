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
