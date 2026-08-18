package derive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// browser.go is the Chromium half of the rasterizer: launch a headless browser,
// drive it over the DevTools protocol, capture the layer, kill the whole process
// tree.
//
// # One browser per job, on purpose
//
// Legacy kept a long-lived browser with a pool of reused pages, and spent a
// large fraction of its render-worker on the consequences: a persistent context
// that got POISONED by one screenshot timeout and then failed every subsequent
// job on that slot, a consecutive-inner-timeout detector to notice it, a
// pool-starvation detector to notice THAT, and a whole context-rebuild path.
// Every one of those exists to recover shared state that only exists because the
// browser is shared.
//
// This renderer launches a browser per job and kills it afterwards. It is the
// right trade HERE and would have been the wrong one there: legacy re-rendered
// widgets on a timer, on the appliance, forever, so process startup was a
// per-second cost. This tool runs out of band over a handful of layers when an
// operator asks it to, so ~half a second of startup per layer buys the complete
// elimination of every shared-state failure mode above. What is left to guard is
// the process itself, which is what killTree is.
//
// # Determinism, and the honest edge of it
//
// Content-addressing dedupes better when the same spec yields the same bytes, so
// the launch flags pin what they CAN: the colour profile, the device scale
// factor, compositor staging, Skia's SIMD dispatch, and the field-trial groups a
// fresh profile dir would otherwise re-roll per launch. The PNG is then
// re-encoded through Go's encoder (png.go) so Chromium's own encoder settings
// cannot leak into the digest either.
//
// What the flags do NOT pin is glyph rasterization, and the comment here used to
// claim otherwise. --font-render-hinting and --disable-font-subpixel-positioning
// are headless-shell switches: they are live when WAIVEO_DERIVE_CHROMIUM points
// at a headless_shell binary, and --headless=new — what a normal chromium or
// chrome install runs — does not consume them. Forcing hinting to `full` under
// --headless=new was measured to produce byte-identical output, which full
// hinting cannot do if the switch reached the font renderer. On those builds
// hinting and subpixel glyph positioning come from the host's fontconfig, and
// the typeface itself comes from the host whenever a spec names no FontFamily.
//
// The consequence is measurable and worth stating plainly, because a test used
// to assert the opposite: two renders of a TEXT spec in two fresh processes can
// differ. A quarter-pixel difference in glyph origin — the finest wobble this
// axis can produce — moves ~1% of the pixels on a 96px "Hello" by up to 52/255.
// It is rare (one CI run in 105) but it is real, and no flag available here
// closes it.
//
// So the property the renderer actually offers is: same spec + same Chromium
// build + same host font stack -> same bytes, with glyph antialiasing as a
// residual. Missing it costs one duplicate object in the content store — never
// correctness, because staleness is keyed on the SPEC digest (wire.DeriveDigest)
// and never on the pixels, and contentgc reclaims the duplicate.

// BrowserOptions configures the Chromium renderer.
type BrowserOptions struct {
	// ExecPath is the Chromium/Chrome binary. Empty means FindChromium.
	ExecPath string
	// NoSandbox disables Chromium's sandbox. Off by default and deliberately
	// opt-in: it is required inside most containers and as root, and it is a
	// real reduction in isolation everywhere else.
	NoSandbox bool
	// LaunchTimeout bounds the wait for the browser to print its DevTools
	// endpoint. Separate from the per-job timeout because a browser that never
	// starts is a different diagnosis from a page that never paints.
	LaunchTimeout time.Duration
}

// Browser is a Renderer backed by headless Chromium.
type Browser struct {
	opt BrowserOptions
}

// NewBrowser builds a Chromium-backed Renderer, resolving the executable up
// front so a missing browser is reported once at startup rather than as N
// identical render failures.
func NewBrowser(opt BrowserOptions) (*Browser, error) {
	if opt.ExecPath == "" {
		p, err := FindChromium()
		if err != nil {
			return nil, err
		}
		opt.ExecPath = p
	}
	if _, err := os.Stat(opt.ExecPath); err != nil {
		return nil, fmt.Errorf("derive: chromium executable %q: %w", opt.ExecPath, err)
	}
	if opt.LaunchTimeout <= 0 {
		opt.LaunchTimeout = 30 * time.Second
	}
	return &Browser{opt: opt}, nil
}

// ExecPath reports the resolved browser binary, so the tool can say which one it
// is about to use — the difference between two Chromium builds is a difference
// in output bytes.
func (b *Browser) ExecPath() string { return b.opt.ExecPath }

var devtoolsWS = regexp.MustCompile(`ws://[^\s]+`)

// Render launches a browser on the page and returns normalised PNG bytes.
func (b *Browser) Render(ctx context.Context, page Page) ([]byte, error) {
	dir, err := os.MkdirTemp("", "waiveo-derive-*")
	if err != nil {
		return nil, fmt.Errorf("derive: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	pagePath := filepath.Join(dir, "page.html")
	if err := os.WriteFile(pagePath, []byte(page.HTML), 0o600); err != nil {
		return nil, fmt.Errorf("derive: write page: %w", err)
	}

	proc, wsURL, err := b.launch(ctx, dir, page)
	if err != nil {
		return nil, err
	}
	// The kill is deferred UNCONDITIONALLY and it kills the TREE, not the
	// launcher. This is the guard, and it is the one that cannot be skipped on
	// any path: `chromium` forks renderer, GPU and zygote children that do not
	// die with their parent, so a plain Process.Kill on a timed-out job leaves a
	// tree behind holding its profile directory — and the tool's own
	// os.RemoveAll above then races those survivors. Repeat that per timed-out
	// layer and the render host runs out of memory with no single process
	// looking guilty.
	defer killTree(proc)

	shot, err := b.capture(ctx, wsURL, pagePath, page)
	if err != nil {
		return nil, err
	}
	return shot, nil
}

// readStderrTail reports what Chromium printed before it gave up, for an error
// message. It NEVER blocks the caller: on the launch-timeout path the reader
// goroutine is still running and will not have sent anything, and an error
// report is not worth hanging a render on — so a brief wait, then say so.
//
// The tail is capped because Chromium is capable of thousands of lines of GPU
// and font chatter, and the useful part ("Failed to move to new namespace",
// "error while loading shared libraries") is always at the END, immediately
// before it exits.
func readStderrTail(ch <-chan string) string {
	var out string
	select {
	case out = <-ch:
	case <-time.After(2 * time.Second):
		return "(chromium is still running; no exit output to report)"
	}
	if out == "" {
		return "(empty)"
	}
	const max = 1500
	if len(out) > max {
		out = "…" + out[len(out)-max:]
	}
	return out
}

// launch starts Chromium and waits for its DevTools endpoint.
func (b *Browser) launch(ctx context.Context, profileDir string, page Page) (*os.Process, string, error) {
	args := []string{
		"--headless=new",
		"--remote-debugging-port=0",
		"--user-data-dir=" + profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-background-networking",
		"--disable-sync",
		"--disable-default-apps",
		"--disable-dev-shm-usage",
		"--disable-gpu",
		"--hide-scrollbars",
		"--mute-audio",
		// --- determinism ---------------------------------------------------
		// Colour: without this the capture is converted through whatever
		// profile the host advertises. Measured live — display-p3 moves the
		// bytes.
		"--force-color-profile=srgb",
		// Rasterization scale. Emulation.setDeviceMetricsOverride pins the
		// PAGE's device scale factor (see capture), but the LAUNCH-time factor
		// is a separate raster input and was previously unpinned: forcing it to
		// 2 changes the pixels while NormalizePNG still measures the expected
		// geometry, so the mismatch guard would not have caught it.
		"--force-device-scale-factor=1",
		// Skia's SIMD dispatch is chosen from the CPU's feature set, so two
		// machines with different CPUs rasterize the same page differently.
		// Irrelevant between two renders on ONE host; it is what makes the
		// output comparable across a fleet.
		"--disable-skia-runtime-opts",
		// Each Render gets a fresh --user-data-dir, which mints a fresh
		// low-entropy source for field-trial assignment — so two launches on one
		// machine can land in different experiment groups and take different
		// rendering paths. These stop the trials being rolled at all.
		"--disable-field-trial-config",
		"--disable-variations-seed-fetch",
		// Text antialiasing. Live on headless_shell builds (which
		// WAIVEO_DERIVE_CHROMIUM may select) and inert under --headless=new,
		// where hinting comes from the host's fontconfig instead. Kept for the
		// builds where they work; see the package comment for why they are NOT
		// a guarantee of glyph stability.
		"--disable-lcd-text",
		"--font-render-hinting=none",
		"--disable-font-subpixel-positioning",
		// Also headless_shell-only. Its two useful implications for this
		// renderer are passed explicitly below, so nothing depends on it.
		"--deterministic-mode",
		// Compositor staging: draw only once every stage has run, and never
		// substitute a placeholder frame because content was slow. These are the
		// switches that actually hold the capture stable on a loaded machine.
		"--run-all-compositor-stages-before-draw",
		"--disable-new-content-rendering-timeout",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		fmt.Sprintf("--window-size=%d,%d", page.ClipW, page.ClipH),
		"about:blank",
	}
	if b.opt.NoSandbox {
		args = append([]string{"--no-sandbox", "--disable-setuid-sandbox"}, args...)
	}

	cmd := exec.Command(b.opt.ExecPath, args...)
	// Setpgid puts the browser and everything it forks into ONE process group,
	// which is what makes killTree's negative-pid signal reach the whole tree.
	// Without it there is no handle on the children at all.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", fmt.Errorf("derive: chromium stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("derive: start chromium: %w", err)
	}

	found := make(chan string, 1)
	// Chromium says WHY it died on the very stream we are scanning, and this
	// used to drop it: the reader accumulated stderr into `buf` purely to find
	// the DevTools banner, then on pipe close returned and let `buf` go out of
	// scope. The caller reported "chromium exited before announcing a DevTools
	// endpoint" — a sentence that is true of a missing shared library, a
	// sandbox that cannot initialize, a bad flag and a killed process alike.
	// Twelve consecutive CI failures produced no evidence for that reason.
	// Handing the tail back costs one channel.
	stderrTail := make(chan string, 1)
	go func() {
		defer func() { _, _ = io.Copy(io.Discard, stderr) }()
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 512)
		for {
			n, rerr := stderr.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
				if i := strings.Index(string(buf), "DevTools listening on "); i >= 0 {
					if m := devtoolsWS.FindString(string(buf[i:])); m != "" {
						found <- strings.TrimSpace(m)
						return
					}
				}
				if len(buf) > 1<<16 {
					buf = buf[len(buf)-4096:]
				}
			}
			if rerr != nil {
				stderrTail <- strings.TrimSpace(string(buf))
				close(found)
				return
			}
		}
	}()

	select {
	case url, ok := <-found:
		if !ok || url == "" {
			killTree(cmd.Process)
			return nil, "", fmt.Errorf("derive: chromium exited before announcing a DevTools endpoint; chromium stderr: %s", readStderrTail(stderrTail))
		}
		return cmd.Process, url, nil
	case <-time.After(b.opt.LaunchTimeout):
		killTree(cmd.Process)
		return nil, "", fmt.Errorf("derive: chromium did not announce a DevTools endpoint within %s; chromium stderr: %s", b.opt.LaunchTimeout, readStderrTail(stderrTail))
	case <-ctx.Done():
		killTree(cmd.Process)
		return nil, "", ctx.Err()
	}
}

// killTree terminates the browser's whole process group.
//
// Signalling the NEGATIVE pid is the entire point: the group was created by
// Setpgid at launch, so -pid reaches the renderer, GPU and zygote processes
// Chromium forked, which a signal to the launcher alone does not. SIGTERM first
// so a healthy browser can flush and exit, then SIGKILL after a short grace so a
// wedged one cannot ignore it — a wedged browser is exactly the case this exists
// for, and it is precisely the case that ignores SIGTERM.
func killTree(p *os.Process) {
	if p == nil {
		return
	}
	pgid := -p.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-done
	}
}

// capture drives the DevTools session: navigate, force a painted frame, clip the
// layer, and normalise the bytes.
func (b *Browser) capture(ctx context.Context, wsURL, pagePath string, page Page) ([]byte, error) {
	c, err := dialCDP(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	defer c.close()

	target, err := c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"})
	if err != nil {
		return nil, err
	}
	var tgt struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(target, &tgt); err != nil {
		return nil, fmt.Errorf("derive: decode createTarget: %w", err)
	}
	attach, err := c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": tgt.TargetID, "flatten": true})
	if err != nil {
		return nil, err
	}
	var att struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(attach, &att); err != nil {
		return nil, fmt.Errorf("derive: decode attachToTarget: %w", err)
	}
	sid := att.SessionID

	for _, m := range []string{"Page.enable", "Runtime.enable"} {
		if _, err := c.call(ctx, sid, m, map[string]any{}); err != nil {
			return nil, err
		}
	}
	// The viewport must be at least the clip, and the device scale factor must
	// be exactly 1: a host with a HiDPI default would silently render every
	// layer at 2x and produce a PNG twice the authored geometry.
	if _, err := c.call(ctx, sid, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": page.ClipW, "height": page.ClipH, "deviceScaleFactor": 1, "mobile": false,
	}); err != nil {
		return nil, err
	}
	// A TRANSPARENT default background, so a layer with no fill composites over
	// the native layers beneath it on the panel instead of punching an opaque
	// rectangle through the slide.
	if _, err := c.call(ctx, sid, "Emulation.setDefaultBackgroundColorOverride", map[string]any{
		"color": map[string]int{"r": 0, "g": 0, "b": 0, "a": 0},
	}); err != nil {
		return nil, err
	}

	if _, err := c.call(ctx, sid, "Page.navigate", map[string]any{"url": "file://" + pagePath}); err != nil {
		return nil, err
	}
	if err := c.waitReady(ctx, sid); err != nil {
		return nil, err
	}

	shot, err := b.shoot(ctx, c, sid, page)
	if err != nil {
		return nil, err
	}
	return shot, nil
}

// forcePaintJS is the PORTED IDLE-FRAME GUARD, and it is the single most
// expensive lesson in legacy's render worker.
//
// By capture time the page has loaded and gone completely idle — zero script,
// zero layout. Headless Chromium's Page.captureScreenshot on a fresh surface
// then intermittently BLOCKS FOR ITS ENTIRE TIMEOUT waiting for a compositor
// frame that an idle page has no reason to commit; the immediate retry
// afterwards succeeds in tens of milliseconds. It reads as a hang, not an error,
// and it starves everything behind it.
//
// The three parts each do a job. Awaiting document.fonts.ready is what stops a
// custom face being captured before it has been applied (the transparent-capture
// signature). Reading offsetHeight forces a synchronous layout so freshly
// applied font metrics flush. Two NESTED requestAnimationFrames guarantee a
// paint AND its commit — one is not enough, because the first fires before the
// frame it schedules is composited. The setTimeout bounds the whole thing so the
// guard can never itself become the hang.
const forcePaintJS = `(async () => {
  try { await document.fonts.ready; } catch (e) {}
  await new Promise((resolve) => {
    let settled = false;
    const finish = () => { if (!settled) { settled = true; resolve(); } };
    void document.body.offsetHeight;
    requestAnimationFrame(() => requestAnimationFrame(finish));
    setTimeout(finish, 1000);
  });
  return true;
})()`

// shoot forces a frame, captures, and recaptures once if the result is blank.
//
// The blank recapture is the second half of the same lesson: a fully transparent
// PNG at the correct dimensions is what a capture taken during a font race looks
// like, and it is indistinguishable at the pixel level from a real empty layer.
// Legacy could not fail on it — a thrown error there dropped the WHOLE slide, so
// it shipped the transparent layer with a warning. This renderer can, and does:
// a spec that reaches here is guaranteed by ValidateDeriveSpec to draw something
// (a rect has a fill, a text has text, a qr has a payload), the failure costs
// only that one layer, and the layer simply stays pending — the slide around it
// keeps playing. Silently uploading a blank PNG would instead mint an asset_ref
// for nothing and hang an invisible rectangle on a wall.
func (b *Browser) shoot(ctx context.Context, c *cdpConn, sid string, page Page) ([]byte, error) {
	var last []byte
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := c.call(ctx, sid, "Runtime.evaluate", map[string]any{
			"expression": forcePaintJS, "awaitPromise": true, "returnByValue": true,
		}); err != nil {
			return nil, err
		}
		raw, err := c.call(ctx, sid, "Page.captureScreenshot", map[string]any{
			"format": "png",
			"clip": map[string]any{
				"x": 0, "y": 0, "width": page.ClipW, "height": page.ClipH, "scale": 1,
			},
			"captureBeyondViewport": true,
		})
		if err != nil {
			return nil, err
		}
		var out struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("derive: decode captureScreenshot: %w", err)
		}
		buf, err := base64.StdEncoding.DecodeString(out.Data)
		if err != nil {
			return nil, fmt.Errorf("derive: decode screenshot payload: %w", err)
		}
		normalised, blank, err := NormalizePNG(buf, page.ClipW, page.ClipH)
		if err != nil {
			return nil, err
		}
		if !blank {
			return normalised, nil
		}
		last = normalised
	}
	_ = last
	return nil, errors.New("derive: the capture was fully transparent twice — the layer drew nothing (a font that never applied, or content that renders to nothing)")
}

// ---- CDP transport ---------------------------------------------------------

// cdpConn is a minimal DevTools-protocol client: id-correlated request/response
// over one websocket, with a single reader goroutine.
type cdpConn struct {
	ws *websocket.Conn

	mu      sync.Mutex
	nextID  int
	pending map[int]chan cdpReply
	closed  bool

	loaded  chan struct{}
	loadFn  sync.Once
	readErr chan error
}

type cdpReply struct {
	result json.RawMessage
	err    error
}

func dialCDP(ctx context.Context, url string) (*cdpConn, error) {
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("derive: connect to chromium devtools: %w", err)
	}
	// A full-canvas PNG is megabytes and arrives base64-encoded in one frame;
	// the library's default read limit is 32 KiB, which would truncate every
	// capture into a protocol error.
	ws.SetReadLimit(256 << 20)
	c := &cdpConn{
		ws:      ws,
		pending: map[int]chan cdpReply{},
		loaded:  make(chan struct{}),
		readErr: make(chan error, 1),
	}
	go c.readLoop()
	return c, nil
}

func (c *cdpConn) readLoop() {
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.failAll(err)
			return
		}
		var msg struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Method == "Page.loadEventFired" {
			c.loadFn.Do(func() { close(c.loaded) })
			continue
		}
		if msg.ID == 0 {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[msg.ID]
		delete(c.pending, msg.ID)
		c.mu.Unlock()
		if !ok {
			continue
		}
		if msg.Error != nil {
			ch <- cdpReply{err: fmt.Errorf("derive: devtools error %d: %s", msg.Error.Code, msg.Error.Message)}
			continue
		}
		ch <- cdpReply{result: msg.Result}
	}
}

func (c *cdpConn) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for id, ch := range c.pending {
		ch <- cdpReply{err: fmt.Errorf("derive: devtools connection lost: %w", err)}
		delete(c.pending, id)
	}
	select {
	case c.readErr <- err:
	default:
	}
}

func (c *cdpConn) call(ctx context.Context, sessionID, method string, params map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("derive: devtools connection is closed")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan cdpReply, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("derive: encode %s: %w", method, err)
	}
	if err := c.ws.Write(ctx, websocket.MessageText, body); err != nil {
		return nil, fmt.Errorf("derive: send %s: %w", method, err)
	}
	select {
	case reply := <-ch:
		if reply.err != nil {
			return nil, fmt.Errorf("%s: %w", method, reply.err)
		}
		return reply.result, nil
	case <-ctx.Done():
		// The job deadline fired. Returning here lets Render's deferred killTree
		// take the browser down rather than leaving this goroutine parked on a
		// reply that is not coming.
		return nil, fmt.Errorf("derive: %s: %w", method, ctx.Err())
	}
}

// waitReady waits for the load event, then confirms readyState — belt and
// braces, because a document that finished loading before Page.enable took
// effect never fires the event this would otherwise wait forever for.
func (c *cdpConn) waitReady(ctx context.Context, sid string) error {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-c.loaded:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("derive: the page never finished loading")
		case <-time.After(25 * time.Millisecond):
			raw, err := c.call(ctx, sid, "Runtime.evaluate", map[string]any{
				"expression": "document.readyState", "returnByValue": true,
			})
			if err != nil {
				return err
			}
			var out struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			}
			if err := json.Unmarshal(raw, &out); err == nil && out.Result.Value == "complete" {
				return nil
			}
		}
	}
}

func (c *cdpConn) close() {
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
}

// ---- executable discovery --------------------------------------------------

// FindChromium locates a usable browser.
//
// WAIVEO_DERIVE_CHROMIUM wins outright, because "which Chromium" is a
// determinism input: two builds antialias differently, so an operator who needs
// byte-stable output across machines pins it explicitly. Failing that, the
// search covers the ordinary system installs and the Playwright cache, since a
// contributor who has run the console's own click-through gate already has a
// Chromium on disk and should not be asked to install a second.
func FindChromium() (string, error) {
	if p := os.Getenv("WAIVEO_DERIVE_CHROMIUM"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("derive: WAIVEO_DERIVE_CHROMIUM=%q: %w", p, err)
		}
		return p, nil
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	for _, p := range wellKnownChromiumPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	for _, p := range playwrightChromiumPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("derive: no Chromium found — install one, or point WAIVEO_DERIVE_CHROMIUM at a Chromium/Chrome binary. This tool is deliberately OFF-appliance: the box never needs one")
}

func wellKnownChromiumPaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		}
	}
	return []string{
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/usr/bin/google-chrome",
		"/snap/bin/chromium",
	}
}

// playwrightChromiumPaths enumerates the browsers the web click-through gate's
// Playwright install leaves in the OS cache, newest last so the most recent is
// preferred.
func playwrightChromiumPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var root string
	switch runtime.GOOS {
	case "darwin":
		root = filepath.Join(home, "Library", "Caches", "ms-playwright")
	default:
		root = filepath.Join(home, ".cache", "ms-playwright")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "chromium-") {
			continue
		}
		if runtime.GOOS == "darwin" {
			out = append(out, filepath.Join(root, e.Name(), "chrome-mac", "Chromium.app", "Contents", "MacOS", "Chromium"))
		} else {
			out = append(out, filepath.Join(root, e.Name(), "chrome-linux", "chrome"))
		}
	}
	// Reverse so the highest build number is tried first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
