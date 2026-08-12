package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// A real Roku's answer, trimmed to the shape that matters.
const deviceInfoXML = `<?xml version="1.0" encoding="UTF-8" ?>
<device-info>
	<serial-number>X029009JC6LF</serial-number>
	<model-name>100012587</model-name>
	<user-device-name>The Hanger</user-device-name>
</device-info>`

// hostPortOf splits a test server's address into the parts identifyAt takes.
func hostPortOf(t *testing.T, srv *httptest.Server) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split the test server address: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("test server port %q: %v", portStr, err)
	}
	return host, port
}

// Drives the REAL lookup against a server answering exactly as a Roku does.
func TestIdentifyReportsTheNameTheScreenGives(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(deviceInfoXML))
	}))
	defer srv.Close()

	host, port := hostPortOf(t, srv)
	name, err := identifyAt(context.Background(), host, port)
	if err != nil {
		t.Fatalf("identifyAt: %v", err)
	}
	if name != "The Hanger" {
		t.Errorf("name = %q, want %q", name, "The Hanger")
	}
	if gotPath != "/query/device-info" {
		t.Errorf("path = %q, want /query/device-info", gotPath)
	}
}

// A host that answers on the port but is not a Roku must not be reported as an
// identified screen -- it is precisely the "something else took that DHCP lease"
// case the lookup exists to catch.
func TestIdentifyRejectsAnAnswerThatIsNotAScreen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>some other service</body></html>"))
	}))
	defer srv.Close()

	host, port := hostPortOf(t, srv)
	if name, err := identifyAt(context.Background(), host, port); err == nil {
		t.Errorf("identifyAt reported %q for a host that is not a Roku", name)
	}
}

func TestIdentifyOnADeadAddressSaysSo(t *testing.T) {
	// A port nothing listens on: bind one, learn its number, release it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, _, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()

	name, err := identify(context.Background(), host)
	if err == nil {
		t.Fatalf("identify reported %q for an address with nothing on it", name)
	}
	if d := describe("", err); !strings.Contains(d, "unidentified") {
		t.Errorf("describe(%v) = %q, want it to mark the screen unidentified", err, d)
	}
}

func TestDescribeNamesTheThreeOutcomes(t *testing.T) {
	if got := describe("The Hanger", nil); got != "The Hanger" {
		t.Errorf("a named screen renders as %q", got)
	}
	if got := describe("", nil); got != "(unnamed screen)" {
		t.Errorf("a screen with an empty name renders as %q", got)
	}
	if got := describe("ignored", context.DeadlineExceeded); !strings.Contains(got, "unidentified") {
		t.Errorf("an error renders as %q, and must not present the stale name as fact", got)
	}
}

// okInstall is a stand-in for a screen that accepts the channel, so these tests
// exercise the real walk without a Roku.
func okInstall(context.Context, *http.Client, device, credentials, []byte) (installOutcome, error) {
	return installOutcome{OK: true, Detail: "Install Success"}, nil
}

// The report must NAME the screen, in both modes. This is the whole point of
// the lookup: a roster label is the operator's claim about an address, and the
// failure it guards — a successful sideload onto the wrong screen — leaves no
// other trace in the log.
func TestReportNamesTheScreenItReached(t *testing.T) {
	for _, mode := range []struct {
		name   string
		dryRun bool
	}{{"dry run", true}, {"real walk", false}} {
		t.Run(mode.name, func(t *testing.T) {
			h := newWalkHarness(t, "a=10.0.0.1", okInstall)
			h.opts.DryRun = mode.dryRun
			h.opts.Identifier = func(context.Context, string) (string, error) {
				return "Lobby North", nil
			}
			if _, err := run(context.Background(), h.opts); err != nil {
				t.Fatalf("run: %v", err)
			}
			if out := h.out.String(); !strings.Contains(out, "Lobby North") {
				t.Errorf("the report does not name the screen it reached:\n%s", out)
			}
		})
	}
}

// A screen that will not identify must not be reported as if it had. The dev-lab
// env still names an address the fleet left, and "(unidentified)" is the line
// that tells an operator the tool is aimed at nothing.
func TestReportDoesNotInventANameWhenTheLookupFails(t *testing.T) {
	h := newWalkHarness(t, "a=10.0.0.1", okInstall)
	h.opts.DryRun = true
	h.opts.Identifier = func(context.Context, string) (string, error) {
		return "", context.DeadlineExceeded
	}
	if _, err := run(context.Background(), h.opts); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out := h.out.String(); !strings.Contains(out, "unidentified") {
		t.Errorf("a failed lookup is not reported as unidentified:\n%s", out)
	}
}
