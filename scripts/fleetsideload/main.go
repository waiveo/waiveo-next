// Command fleetsideload pushes a player-v3 build onto a wall of Roku screens.
//
// # Why this exists
//
// Every player change in this repo — the boot-retry fix, a new native layer
// renderer, anything at all under player-v3/ — is inert until the bytes are on
// the TVs, and until this command there was NO path to put them there. The
// screens are unattended, mounted, and often not physically reachable; the
// only remote install surface Roku offers is the developer web installer, one
// device at a time, behind Digest auth, answering in HTML. Doing that by hand
// across the fleet is how a wall ends up running four different builds.
//
// # What it does
//
// Packages player-v3 (or takes a prebuilt zip), then walks the roster SERIALLY
// and sideloads each device, bounded by a per-device timeout, printing one
// line per device and exiting non-zero if any device did not report "Install
// Success".
//
//	go run ./scripts/fleetsideload -devices hanger=192.168.50.21,lobby=192.168.50.22
//	go run ./scripts/fleetsideload -dry-run
//	go run ./scripts/fleetsideload -zip build/player.zip -timeout 3m
//
// # What happens on the TV
//
// /plugin_install AUTO-LAUNCHES the channel with NO launch args as soon as the
// install lands. For an already-paired screen that is exactly right — the
// player reads its persisted pairing and reconnects on its own. A screen that
// still needs a pairing code needs a deep-link launch afterward
// (`/launch/dev?pairingCode=…`); this tool does not do that, because handing
// one code to a fleet is not a thing that makes sense.
//
// Serial, not parallel, and deliberately: a Roku reboots its channel at the
// end of an install, several of these screens share a modest AP, and a fleet
// update that browns out the network mid-walk cannot report which devices it
// actually finished. Seven devices at a few seconds each is not a wait worth
// optimising into an ambiguity.
//
// # Roster
//
// -devices wins. With no -devices, the roster is $WAIVEO_RELAY_ECP_TARGETS —
// the same entity=host list the relay itself drives, so "the fleet" means one
// thing in both places (parseECPTargetDevices explains why its ports are
// dropped). With neither, ROKU_DEV_IP from the dev-lab env file is used as the
// single-device developer default.
//
// # Credentials
//
// Never hardcoded and never printed: the dev password comes from the process
// environment or the dev-lab env file, resolved by credentials.go.
//
// Dev tooling for the lab fleet. Not a contract surface, not a served route.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// exit codes: 0 every device installed, 1 at least one device failed, 2 the
// run could not start (bad roster, no credential, unbuildable zip). Separated
// so a wrapper can tell "the fleet has a problem" from "you invoked me wrong".
const (
	exitDeviceFailure = 1
	exitSetupFailure  = 2
)

func main() {
	var (
		devicesFlag = flag.String("devices", "", "comma-separated `[name=]host[:port]` roster (default: $WAIVEO_RELAY_ECP_TARGETS, then ROKU_DEV_IP)")
		srcFlag     = flag.String("src", "player-v3", "channel source dir to package when -zip is not given")
		zipFlag     = flag.String("zip", "", "prebuilt channel zip to sideload instead of packaging -src")
		timeoutFlag = flag.Duration("timeout", 90*time.Second, "per-device timeout for the whole install exchange")
		userFlag    = flag.String("user", "", "Roku dev username (default: $ROKU_DEV_USER, then \"rokudev\")")
		envFileFlag = flag.String("env-file", "", "dev-lab env file to read credentials from (default: $WAIVEO_ENV_FILE, then ~/.config/waiveo/dev-lab.env)")
		dryRun      = flag.Bool("dry-run", false, "resolve everything and print the plan, but issue no requests")
	)
	flag.Parse()

	opts := runOptions{
		Devices: *devicesFlag,
		Src:     *srcFlag,
		Zip:     *zipFlag,
		Timeout: *timeoutFlag,
		User:    *userFlag,
		EnvFile: *envFileFlag,
		DryRun:  *dryRun,
		Getenv:  os.Getenv,
		Client:  &http.Client{},
		Out:     os.Stdout,
		Installer: func(ctx context.Context, client *http.Client, dev device, creds credentials, zip []byte) (installOutcome, error) {
			return installChannel(ctx, client, dev, creds, zip)
		},
	}

	failures, err := run(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleetsideload: %v\n", err)
		os.Exit(exitSetupFailure)
	}
	if failures > 0 {
		os.Exit(exitDeviceFailure)
	}
}

// installFunc is the seam a test substitutes for the real dev installer, so
// the roster walk, the per-device timeout, the failure isolation, and the
// reporting are all exercised without any Roku in the room.
type installFunc func(ctx context.Context, client *http.Client, dev device, creds credentials, zip []byte) (installOutcome, error)

// runOptions is everything one fleet walk needs. Every ambient dependency —
// the environment, the HTTP client, the output stream, the installer itself —
// is a field rather than a package-level lookup, which is what makes run()
// testable end to end.
type runOptions struct {
	Devices   string
	Src       string
	Zip       string
	Timeout   time.Duration
	User      string
	EnvFile   string
	DryRun    bool
	Getenv    func(string) string
	Client    *http.Client
	Out       io.Writer
	Installer installFunc
}

// run resolves the roster, the credential, and the channel bytes, then walks
// the fleet. It returns the number of devices that did NOT install; an error
// means the walk never started.
//
// Everything is resolved BEFORE the first device is touched — roster, then
// credential, then zip — so an invocation that cannot possibly work fails in
// under a second with nothing half-applied, instead of updating three screens
// and then discovering the password is missing. That ordering is the reason
// -dry-run is worth having: it runs exactly this resolution and stops.
func run(ctx context.Context, opts runOptions) (int, error) {
	devices, source, err := resolveRoster(opts)
	if err != nil {
		return 0, err
	}
	if len(devices) == 0 {
		return 0, fmt.Errorf("no devices: pass -devices, or set WAIVEO_RELAY_ECP_TARGETS / ROKU_DEV_IP")
	}

	envFilePath := opts.EnvFile
	if envFilePath == "" {
		envFilePath = defaultEnvFilePath(opts.Getenv)
	}
	fileValues, err := loadEnvFile(envFilePath)
	if err != nil {
		return 0, err
	}
	creds, err := resolveCredentials(opts.Getenv, fileValues, opts.User, envFilePath)
	if err != nil {
		return 0, err
	}

	zipBytes, fileCount, origin, err := resolveChannel(opts)
	if err != nil {
		return 0, err
	}

	fmt.Fprintf(opts.Out, "fleetsideload: %s — %d file(s), %s\n", origin, fileCount, humanBytes(len(zipBytes)))
	fmt.Fprintf(opts.Out, "fleetsideload: %d device(s) from %s, serial, %s timeout each, user %q\n",
		len(devices), source, opts.Timeout, creds.User)

	if opts.DryRun {
		for _, dev := range devices {
			fmt.Fprintf(opts.Out, "  %-16s %-24s DRY-RUN  would POST /plugin_install\n", dev.Name, dev.Addr())
		}
		fmt.Fprintf(opts.Out, "fleetsideload: dry run — nothing was installed\n")
		return 0, nil
	}

	failures := 0
	for _, dev := range devices {
		started := time.Now()
		outcome, err := installOne(ctx, opts, dev, creds, zipBytes)
		elapsed := time.Since(started).Round(100 * time.Millisecond)

		switch {
		case err != nil:
			// A device that cannot be reached, times out, or rejects the
			// credential is reported and the walk CONTINUES. One dark screen
			// behind a dead switch must not stop the other six from getting
			// the build — the whole reason a fleet tool exists.
			failures++
			fmt.Fprintf(opts.Out, "  %-16s %-24s FAILED   %v (%s)\n", dev.Name, dev.Addr(), err, elapsed)
		case !outcome.OK:
			failures++
			fmt.Fprintf(opts.Out, "  %-16s %-24s FAILED   %s (%s)\n", dev.Name, dev.Addr(), outcome.Detail, elapsed)
		default:
			fmt.Fprintf(opts.Out, "  %-16s %-24s OK       %s (%s)\n", dev.Name, dev.Addr(), outcome.Detail, elapsed)
		}
	}

	installed := len(devices) - failures
	if failures > 0 {
		fmt.Fprintf(opts.Out, "fleetsideload: %d/%d installed — %d FAILED\n", installed, len(devices), failures)
	} else {
		fmt.Fprintf(opts.Out, "fleetsideload: %d/%d installed\n", installed, len(devices))
	}
	return failures, nil
}

// installOne runs one device's install under its own timeout, derived from the
// caller's ctx so a Ctrl-C still stops the walk immediately. The per-device
// deadline is what keeps a wedged screen — ECP-alive but with a hung :80 — from
// holding the whole fleet update open indefinitely.
func installOne(ctx context.Context, opts runOptions, dev device, creds credentials, zipBytes []byte) (installOutcome, error) {
	devCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	return opts.Installer(devCtx, opts.Client, dev, creds, zipBytes)
}

// resolveRoster picks the device list and names where it came from, so the
// report says which roster was used — the commonest confusion with a tool that
// has three sources is not knowing which one answered.
func resolveRoster(opts runOptions) ([]device, string, error) {
	if opts.Devices != "" {
		devices, err := parseDeviceList(opts.Devices)
		return devices, "-devices", err
	}
	if targets := opts.Getenv("WAIVEO_RELAY_ECP_TARGETS"); targets != "" {
		devices, err := parseECPTargetDevices(targets)
		return devices, "WAIVEO_RELAY_ECP_TARGETS", err
	}
	if ip := opts.Getenv("ROKU_DEV_IP"); ip != "" {
		devices, err := parseDeviceList(ip)
		return devices, "ROKU_DEV_IP", err
	}
	return nil, "", nil
}

// resolveChannel produces the bytes to install and a human description of
// where they came from.
func resolveChannel(opts runOptions) ([]byte, int, string, error) {
	if opts.Zip != "" {
		data, count, err := loadChannelZip(opts.Zip)
		return data, count, "channel zip " + opts.Zip, err
	}
	data, count, err := buildChannelZip(opts.Src)
	return data, count, "packaged " + opts.Src, err
}

// humanBytes formats a size for the one line an operator actually reads. A
// wrong-looking number here (a 2 KiB "channel") is often the first sign the
// build step did not do what was expected.
func humanBytes(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := int64(n) / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
