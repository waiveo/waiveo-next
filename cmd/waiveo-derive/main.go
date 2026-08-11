// Command waiveo-derive is the RASTERIZED FALLBACK's renderer (parity row 2.4):
// the off-appliance tool that turns a `derive` slide layer into a
// content-addressed PNG.
//
// # It does not run on the appliance, and that is the design
//
// A signage slide can want five things no Roku SceneGraph node draws: a
// gradient, a drop shadow, a rounded or styled border, a font the device does
// not ship, and a QR code. Legacy drew all five by rasterizing with a headless
// Chromium living inside the signage service. waiveo-next will not do that: the
// feeder and relay are deliberately Go-only, and the Pi this replaces already
// carries the whole legacy stack — a second browser there is out of the
// question.
//
// So the browser lives HERE, in a binary an operator or a dev box runs out of
// band, and the appliance's whole share of the loop is one read-only listing.
// The rendered PNG then enters through the SAME POST /content every other asset
// uses and is referenced by the SAME image path the player already fetches,
// verifies and caches — so nothing about this feature requires a player change.
//
//	waiveo-derive render -spec gradient.json -w 900 -h 300 -out card.png
//	    Render one spec to a file. No server needed; the fastest way to iterate
//	    on a design.
//
//	waiveo-derive sync -api https://box:7420
//	    One pass over everything a feeder reports outstanding: render, upload,
//	    write the reference back. Not a daemon — run it after authoring, or from
//	    a CI step.
//
//	waiveo-derive pending -api https://box:7420
//	    What is outstanding, without rendering anything. Also the answer to "is
//	    my browser even found?" when paired with -chromium.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/maaxton/waiveo-next/internal/derive"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
	"github.com/maaxton/waiveo-next/scripts/devcred"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// SIGINT/SIGTERM cancels the context, which cancels the in-flight render,
	// which trips the deferred process-tree kill. Without this, interrupting the
	// tool mid-render leaves a headless Chromium and its whole child tree
	// running with nothing left that knows about them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "render":
		err = cmdRender(ctx, os.Args[2:])
	case "sync":
		err = cmdSync(ctx, os.Args[2:])
	case "pending":
		err = cmdPending(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "waiveo-derive: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `waiveo-derive — the OFF-APPLIANCE rasterizer for un-native slide layers.

  render   -spec FILE -w N -h N -out FILE [-font FILE]
           Render one derive spec to a PNG. No server involved.

  sync     -api URL [-token-file F] [-insecure-tls] [-concurrency N] [-timeout D]
           Render everything the feeder reports outstanding, upload each PNG,
           and write the reference back onto its layer.

  pending  -api URL [-token-file F] [-insecure-tls]
           List what is outstanding. Renders nothing.

The appliance never runs this and needs no browser. Point
WAIVEO_DERIVE_CHROMIUM at a specific Chromium to pin the output bytes.
`)
}

// browserFlags are the flags every subcommand that rasterizes shares.
type browserFlags struct {
	chromium    string
	noSandbox   bool
	concurrency int
	timeout     time.Duration
}

func (b *browserFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&b.chromium, "chromium", "", "Chromium/Chrome binary (default: WAIVEO_DERIVE_CHROMIUM, then a search of the usual locations)")
	fs.BoolVar(&b.noSandbox, "no-sandbox", false, "disable Chromium's sandbox (needed as root and in most containers)")
	fs.IntVar(&b.concurrency, "concurrency", derive.DefaultConcurrency, "concurrent renders; each one is a whole browser")
	fs.DurationVar(&b.timeout, "timeout", derive.DefaultJobTimeout, "wall-clock ceiling on a single render")
}

func (b *browserFlags) runner() (*derive.Runner, *derive.Browser, error) {
	br, err := derive.NewBrowser(derive.BrowserOptions{ExecPath: b.chromium, NoSandbox: b.noSandbox})
	if err != nil {
		return nil, nil, err
	}
	return derive.NewRunner(br, derive.RunnerOptions{Concurrency: b.concurrency, JobTimeout: b.timeout}), br, nil
}

// apiFlags are the flags every subcommand that talks to a feeder shares.
type apiFlags struct {
	base      string
	tokenFile string
	insecure  bool
}

func (a *apiFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&a.base, "api", "", "feeder base URL, e.g. https://192.168.50.12:7420")
	fs.StringVar(&a.tokenFile, "token-file", "", "file holding the api-key bearer (default: the dev key file)")
	fs.BoolVar(&a.insecure, "insecure-tls", false, "skip TLS verification (a dev feeder serves a self-signed ed25519 leaf)")
}

// client resolves the credential and builds the api client.
//
// The token is read from a file, never a flag: an argument is visible in `ps`
// and lands in shell history, and this one is a bearer credential for the whole
// authoring surface.
func (a *apiFlags) client() (*derive.Client, error) {
	if a.base == "" {
		return nil, fmt.Errorf("-api is required (the feeder this tool renders for)")
	}
	var token string
	if a.tokenFile != "" {
		raw, err := os.ReadFile(a.tokenFile)
		if err != nil {
			return nil, fmt.Errorf("read -token-file: %w", err)
		}
		token = strings.TrimSpace(string(raw))
	} else {
		// devcred owns the dev key file's location, its 0600 requirement and the
		// refusal messages that name `make dev-key`. Re-deriving any of that here
		// would be a second, weaker copy of a rule about a secret.
		t, err := devcred.Load()
		if err != nil {
			return nil, fmt.Errorf("%w (or pass -token-file)", err)
		}
		token = t
	}
	return derive.NewClient(derive.ClientOptions{BaseURL: a.base, Token: token, InsecureTLS: a.insecure})
}

func cmdRender(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	var bf browserFlags
	bf.bind(fs)
	specPath := fs.String("spec", "", "JSON file holding a derive spec")
	fontPath := fs.String("font", "", "font file to embed (the local stand-in for a spec's font_asset_ref)")
	out := fs.String("out", "", "PNG output path")
	w := fs.Int("w", 0, "layer width in canvas pixels")
	h := fs.Int("h", 0, "layer height in canvas pixels")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *specPath == "" || *out == "" || *w <= 0 || *h <= 0 {
		return fmt.Errorf("render needs -spec, -out, -w and -h")
	}

	raw, err := os.ReadFile(*specPath)
	if err != nil {
		return fmt.Errorf("read -spec: %w", err)
	}
	var spec wire.DeriveSpec
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	// An unknown member is refused rather than ignored: a spec with `shaddow`
	// spelled wrong would otherwise render silently without the shadow, and the
	// author would be left staring at the picture wondering why.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return fmt.Errorf("decode -spec: %w", err)
	}
	if err := wire.ValidateDeriveSpec(&spec); err != nil {
		return err
	}

	var font []byte
	if *fontPath != "" {
		font, err = os.ReadFile(*fontPath)
		if err != nil {
			return fmt.Errorf("read -font: %w", err)
		}
	}

	runner, browser, err := bf.runner()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "chromium: %s\n", browser.ExecPath())

	png, err := derive.RenderOne(ctx, runner, &spec, *w, *h, font)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, png, 0o644); err != nil { //nolint:gosec // an image for a human to look at
		return fmt.Errorf("write -out: %w", err)
	}
	fmt.Printf("rendered %s: %dx%d, %d bytes\n", *out, *w, *h, len(png))
	return nil
}

func cmdPending(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pending", flag.ExitOnError)
	var af apiFlags
	af.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := af.client()
	if err != nil {
		return err
	}
	jobs, err := c.ListPending(ctx)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Println("nothing outstanding")
		return nil
	}
	for _, j := range jobs {
		kind := "?"
		if j.Spec != nil {
			kind = j.Spec.Kind
		}
		// The row's NAME as well as its id: an operator scanning the queue knows
		// "Lobby loop", not 01J8CAST0000000000000000AA.
		fmt.Printf("%-7s %s %s (%s) / %s / layer %d — %s %dx%d\n",
			j.State, j.Source, j.ResourceID, j.ResourceName, j.Where(), j.LayerIndex, kind, j.W, j.H)
	}
	fmt.Printf("%d outstanding\n", len(jobs))
	return nil
}

func cmdSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	var af apiFlags
	var bf browserFlags
	af.bind(fs)
	bf.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := af.client()
	if err != nil {
		return err
	}
	runner, browser, err := bf.runner()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "chromium: %s (concurrency %d)\n", browser.ExecPath(), runner.Concurrency())

	rep, err := derive.Sync(ctx, c, runner)
	if err != nil {
		return err
	}
	for _, o := range rep.Rendered {
		fmt.Printf("ok    %s %s (%s) / %s / layer %d -> %s (%d bytes)\n",
			o.Source, o.ResourceID, o.ResourceName, o.Where, o.LayerIndex, o.AssetRef, o.Bytes)
	}
	for _, o := range rep.Failed {
		fmt.Printf("FAIL  %s %s (%s) / %s / layer %d: %v\n",
			o.Source, o.ResourceID, o.ResourceName, o.Where, o.LayerIndex, o.Err)
	}
	// A row-level failure is a read that failed or a write-back that was refused
	// — never named as one when it was the other.
	for _, re := range rep.RowErrs {
		fmt.Printf("FAIL  %s %s: %v\n", re.Source, re.ResourceID, re.Err)
	}
	fmt.Printf("%d listed across %d row(s) read, %d rendered, %d layer(s) failed, %d row(s) failed\n",
		rep.Listed, rep.RowsRead, len(rep.Rendered), len(rep.Failed), len(rep.RowErrs))
	// A non-zero exit on any failure is what makes this usable from a script:
	// a run that rendered nothing must not look like a run with nothing to do.
	if len(rep.Failed) > 0 || len(rep.RowErrs) > 0 {
		return fmt.Errorf("%d layer(s) and %d row(s) failed", len(rep.Failed), len(rep.RowErrs))
	}
	return nil
}
