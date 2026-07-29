// Command authoringdemo is the `make dev` live authoring-loop demonstration: it
// proves, over real HTTP against the running feeder + relay, that a schedule edit
// authored through the api/1 surface changes the program the relay resolves for a
// screen — the whole loop (api -> store -> desired-state -> relay re-pull ->
// schedulehost re-resolve) exercised end to end on the live stack.
//
// It is written in Go for the same reason scripts/devsmoke is: the feeder serves
// its api over an ed25519-leaf TLS cert some system curl builds (macOS LibreSSL)
// cannot handshake, so a curl-based edit would spuriously fail against a healthy
// server. Go's TLS stack handles ed25519, matching the all-Go, all-ed25519 stack.
//
// AUTHENTICATION: every /api/v1 route is authenticated (security-model/1
// SEC-005 — an unresolvable principal is refused, never default-permitted), so
// this probe presents the local dev API key through scripts/devcred. That
// package's doc carries the provisioning path (`make dev-key`) and the argument
// for the authority the key holds; nothing about it is repeated here.
//
// What it does:
//  1. Reads the relay's current resolved display off its log (the boot-time
//     "SCHEDULE RESOLVER OK (screen ...: display:X...)" line). That tells it which
//     of the seed demo's two dayparts is effective right now (content 06:00-22:00
//     vs the overnight blank), so the demo is robust to the wall-clock time it
//     runs at.
//  2. Authors a schedule edit over the api that is guaranteed to flip THAT
//     daypart's resolved display: PATCH the effective daypart's display_power
//     (content->blank when content is showing, blank->on when the overnight blank
//     is). The edit is an ordinary api/1 optimistic-concurrency write (GET the
//     ETag, PATCH under If-Match).
//  3. Waits up to a bounded window for the relay's re-pull loop to pick up the new
//     generation and log a fresh "SCHEDULE RESOLVER OK" line whose display flipped,
//     then prints AUTHORING LOOP OK.
//
// Exits 0 on "AUTHORING LOOP OK", non-zero (with a diagnostic) otherwise.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/scripts/devcred"
)

// Feeder api base + the seed demo's screen/daypart fixture ids (mirroring
// internal/app/store/seed.go — fixture ULIDs, no secrets). make dev seeds a fresh
// store each run, so these ids and the content/overnight daypart structure are
// known.
const (
	feederAPIBase       = "https://127.0.0.1:7420"
	contentDaypartID    = "01J8Z7DEM0DAYPARTC0NTENT01"
	overnightDaypartID  = "01J8Z7DEM0DAYPART0VERN1GHT"
	resolverLineMarker  = "SCHEDULE RESOLVER OK"
	displayToken        = "display:"
	observeWindow       = 25 * time.Second
	observePollInterval = 500 * time.Millisecond
)

func main() {
	logPath := os.Getenv("RELAY_LOG")
	if logPath == "" {
		logPath = ".dev/relay.log"
	}

	client, err := devcred.Client(5 * time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "AUTHORING LOOP FAIL: %v\n", err)
		os.Exit(1)
	}

	if err := run(client, logPath); err != nil {
		fmt.Fprintf(os.Stderr, "AUTHORING LOOP FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(client *http.Client, logPath string) error {
	// 1. The relay's current resolved display — wait for its boot resolver line.
	before, count, err := awaitDisplay(logPath, 15*time.Second, 0)
	if err != nil {
		return fmt.Errorf("read relay resolved display: %w", err)
	}

	// 2. Pick the edit that flips the currently-effective daypart's display.
	var targetDaypart, newPower, wantAfter string
	switch before {
	case "content":
		// The content daypart (06:00-22:00) is effective — blank it.
		targetDaypart, newPower, wantAfter = contentDaypartID, "blank", "blank"
	case "blank":
		// The overnight blank daypart (22:00-06:00) is effective — power it on
		// (display_power on projects to display:content; the daypart carries no
		// playlist, so the flip is content-with-no-asset, still an observable
		// display:content).
		targetDaypart, newPower, wantAfter = overnightDaypartID, "on", "content"
	default:
		return fmt.Errorf("unexpected current resolved display %q (want content|blank)", before)
	}

	// 3. Author the edit over the api/1 surface: GET the ETag, PATCH under If-Match.
	etag, err := getETag(client, feederAPIBase+"/api/v1/dayparts/"+targetDaypart)
	if err != nil {
		return fmt.Errorf("GET effective daypart %s: %w", targetDaypart, err)
	}
	if err := patchDisplayPower(client, feederAPIBase+"/api/v1/dayparts/"+targetDaypart, newPower, etag); err != nil {
		return fmt.Errorf("PATCH display_power=%s on %s: %w", newPower, targetDaypart, err)
	}

	// 4. Wait for the relay's re-pull loop to apply the new generation and log a
	// fresh resolver line whose display flipped.
	after, _, err := awaitDisplay(logPath, observeWindow, count)
	if err != nil {
		return fmt.Errorf("relay did not re-resolve after the authored edit: %w", err)
	}
	if after != wantAfter {
		return fmt.Errorf("relay re-resolved to display:%s, want display:%s after the edit", after, wantAfter)
	}
	if after == before {
		return fmt.Errorf("resolved display did not change (still %q) — the authored edit did not propagate", after)
	}

	fmt.Printf("AUTHORING LOOP OK (api edit flipped resolved display %s -> %s within the re-pull interval)\n", before, after)
	return nil
}

// awaitDisplay polls the relay log until it has MORE than minCount resolver lines
// carrying a display token, then returns the latest such line's display token and
// the new count. minCount=0 waits for the first resolver line; passing the
// pre-edit count waits specifically for a NEW (post-edit) resolve.
func awaitDisplay(logPath string, timeout time.Duration, minCount int) (display string, count int, err error) {
	deadline := time.Now().Add(timeout)
	for {
		displays := readDisplays(logPath)
		if len(displays) > minCount {
			return displays[len(displays)-1], len(displays), nil
		}
		if time.Now().After(deadline) {
			return "", len(displays), fmt.Errorf("no new %q line with a %q token in %s (have %d, want > %d)",
				resolverLineMarker, displayToken, logPath, len(displays), minCount)
		}
		time.Sleep(observePollInterval)
	}
}

// readDisplays returns the display token ("content"/"blank") of every relay log
// line that is a SCHEDULE RESOLVER OK line carrying a "display:" token, in order.
func readDisplays(logPath string) []string {
	b, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, resolverLineMarker) {
			continue
		}
		i := strings.Index(line, displayToken)
		if i < 0 {
			continue
		}
		rest := line[i+len(displayToken):]
		// The token runs until the next separator (',' before an asset, ';' before
		// the site tz, or ')' closing the line).
		end := strings.IndexAny(rest, ",;)")
		if end < 0 {
			end = len(rest)
		}
		out = append(out, strings.TrimSpace(rest[:end]))
	}
	return out
}

// getETag issues a GET and returns the resource's current ETag (the api/1
// optimistic-concurrency revision token).
func getETag(client *http.Client, url string) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("no ETag header")
	}
	return etag, nil
}

// patchDisplayPower PATCHes a daypart's display_power under If-Match, expecting a
// 200 — the api/1 conditional-update convention.
func patchDisplayPower(client *http.Client, url, power, ifMatch string) error {
	body := []byte(fmt.Sprintf(`{"display_power":%q}`, power))
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", ifMatch)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, raw)
	}
	return nil
}
