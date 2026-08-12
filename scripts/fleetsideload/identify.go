package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// ecpPort is Roku's ECP surface — the query/control side, NOT the :80
// /plugin_install side this tool writes to. devices.go's own note explains why
// the two must never be conflated; this file only ever READS from it.
const ecpPort = 8060

// identifyTimeout is deliberately short. Naming the screen is a courtesy before
// a write, not the work: a screen that will not answer in a couple of seconds
// should slow the report down, not the fleet.
const identifyTimeout = 3 * time.Second

var userDeviceName = regexp.MustCompile(`<user-device-name>([^<]*)</user-device-name>`)

// identifyFunc is the seam a test substitutes for the ECP name lookup, matching
// installFunc's role for the installer.
type identifyFunc func(ctx context.Context, host string) (string, error)

// identifyOne routes through the injected identifier and is nil-safe. A caller
// that configured none does not silently appear to have identified the screen —
// the report says it did not, which is the same discipline describe() applies
// to a lookup that failed.
func (o runOptions) identifyOne(ctx context.Context, host string) (string, error) {
	if o.Identifier == nil {
		return "", errors.New("no identifier configured")
	}
	return o.Identifier(ctx, host)
}

// identify asks the screen what IT calls itself, so a sideload report names the
// thing it is about to overwrite rather than only the address it was aimed at.
//
// The roster's label is a CLAIM. `-devices hanger=192.168.50.31` asserts that
// the hanger is at that address; ROKU_DEV_IP asserts it about one host with no
// label at all. Neither is checked against the screen, and an address is
// exactly the part that goes stale — DHCP moves a screen, an env file records
// where it used to be, and the tool writes a channel to whatever answers now.
// That is not hypothetical: this repo's own dev-lab env still names an address
// the fleet left, and the standing rule for this lab is that exactly one screen
// may be sideloaded. A tool that cannot say which screen it reached cannot help
// anyone honour that rule.
//
// Read-only and best-effort by design: it never blocks an install, because a
// screen that ignores ECP may still accept a channel, and refusing to ship to
// it would be a worse failure than shipping without a name. The caller prints
// whatever comes back.
func identify(ctx context.Context, host string) (string, error) {
	return identifyAt(ctx, host, ecpPort)
}

// identifyAt is identify with the port exposed, so a test can drive the REAL
// lookup — request, status handling, capped read, parse — against a stub server
// on an arbitrary port, rather than re-implementing the parse in the test and
// leaving the function itself uncovered.
func identifyAt(ctx context.Context, host string, port int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, identifyTimeout)
	defer cancel()

	url := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/query/device-info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ECP device-info: HTTP %d", resp.StatusCode)
	}
	// Capped: this is a courtesy read of a small XML document, and an
	// unbounded ReadAll against an arbitrary host that answered :8060 is a way
	// to hang a fleet tool on something that is not a Roku at all.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	m := userDeviceName.FindSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("ECP device-info carried no <user-device-name>")
	}
	return string(m[1]), nil
}

// describe renders what identify found for one line of the report.
func describe(name string, err error) string {
	if err != nil {
		return "(unidentified: " + err.Error() + ")"
	}
	if name == "" {
		return "(unnamed screen)"
	}
	return name
}
