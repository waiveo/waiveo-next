package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/maaxton/waiveo-next/internal/relay/relaystatus"
)

func cmdRelay(ctx context.Context, args []string, e env) (int, error) {
	if len(args) == 0 || args[0] != "status" {
		return exitFailure, fmt.Errorf("usage: waiveo relay status [--store PATH] [--relay URL] [--json]")
	}
	return cmdRelayStatus(ctx, args[1:], e)
}

// cmdRelayStatus reports a relay's operational state.
//
// The relay has no management API and this does not invent one. It reads the
// relay's own durable store READ-ONLY — the file REL-142 already scopes a
// relay's on-disk state to — and, if pointed at one, the relay's existing
// unauthenticated /healthz. Both already exist; neither is a new surface.
//
// What that cannot see is printed too, under "not visible from here". A
// diagnostic that showed six green lines and stayed silent about the connection
// state it never checked would be worse than no diagnostic: the operator would
// conclude the relay was connected.
func cmdRelayStatus(ctx context.Context, args []string, e env) (int, error) {
	fs := flag.NewFlagSet("relay status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	store := fs.String("store", relaystatus.DefaultStorePath, "the relay's operational SQLite store")
	relayURL := fs.String("relay", "", "the relay's player listener, e.g. https://192.168.50.31:7421 — probes its /healthz")
	caFile := fs.String("ca-file", "", "PEM bundle to verify the relay's certificate against")
	insecure := fs.Bool("insecure-tls", false, "skip TLS verification when probing /healthz (a relay serves its own enrollment leaf, which no public root signed)")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	if err := fs.Parse(args); err != nil {
		return exitFailure, err
	}

	rep, err := relaystatus.Read(expandHome(*store))
	if err != nil {
		return exitFailure, err
	}
	if *relayURL != "" {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: *insecure} //nolint:gosec // operator-selected; a relay leaf has no public root
		if *caFile != "" {
			pem, err := os.ReadFile(expandHome(*caFile))
			if err != nil {
				return exitFailure, fmt.Errorf("read --ca-file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return exitFailure, fmt.Errorf("--ca-file %s holds no PEM certificate", *caFile)
			}
			tlsCfg.RootCAs = pool
		}
		rep.Healthz = relaystatus.Healthz(ctx, strings.TrimRight(*relayURL, "/")+"/healthz", tlsCfg)
	}

	if *asJSON {
		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return exitFailure, err
		}
		return relayExit(rep), nil
	}
	printRelay(e, rep)
	return relayExit(rep), nil
}

// relayExit fails on the conditions an operator would want a script to catch: no
// readable identity at all, an expired enrollment certificate, or a /healthz
// probe that was asked for and did not answer.
func relayExit(rep *relaystatus.Report) int {
	switch {
	case rep.Identity == nil:
		return exitFailure
	case rep.Identity.Expired:
		return exitFailure
	case rep.Healthz != nil && (rep.Healthz.Error != "" || rep.Healthz.Status != 200):
		return exitFailure
	case len(rep.Problems) > 0:
		return exitDegraded
	}
	return exitOK
}

func printRelay(e env, rep *relaystatus.Report) {
	fmt.Fprintf(e.out, "waiveo relay status  (store %s)\n", rep.StorePath)
	if id := rep.Identity; id != nil {
		fmt.Fprintf(e.out, "  relay_id      %s\n", id.RelayID)
		expiry := fmt.Sprintf("%s (%d days)", id.NotAfter, id.DaysRemaining)
		if id.Expired {
			expiry += "  EXPIRED — this relay can no longer connect"
		}
		fmt.Fprintf(e.out, "  certificate   %s\n", expiry)
		fmt.Fprintf(e.out, "  spki          sha256/%s\n", id.SPKISHA256)
	} else {
		fmt.Fprintln(e.out, "  relay_id      (none — not enrolled)")
	}
	fmt.Fprintf(e.out, "  trust         desired-state key %s, app-peer pin %s\n", yesNo(rep.Trust.DesiredStateKey), yesNo(rep.Trust.AppPeerPin))

	if a := rep.Applied; a != nil {
		fmt.Fprintf(e.out, "  applied       generation %d  hash %s\n", a.Generation, short(a.Hash))
		fmt.Fprintf(e.out, "  serving       %d screen program(s) %v, %d content ref(s)\n", a.ScreenPrograms, a.ScreenIDs, a.ContentRefs)
		fmt.Fprintf(e.out, "  enforcing     %d revoked screen(s), %d adopted device(s), %d pack pattern(s)\n", a.RevokedScreens, a.AdoptedDevices, a.PackPatterns)
		for _, p := range a.DecodeProblems {
			fmt.Fprintf(e.out, "    decode problem: %s\n", p)
		}
	} else {
		fmt.Fprintln(e.out, "  applied       (never applied desired state)")
	}
	if c := rep.Clock; c != nil {
		note := ""
		if c.AheadOfHost {
			note = "  (ahead of THIS machine's clock)"
		}
		fmt.Fprintf(e.out, "  clock floor   %s%s\n", c.FloorTime, note)
	}
	fmt.Fprintf(e.out, "  telemetry     %d queued (seq %d..%d, high water %d), %d loss marker(s)\n",
		rep.Telemetry.Queued, rep.Telemetry.OldestSeq, rep.Telemetry.NewestSeq, rep.Telemetry.HighWaterSeq, rep.Telemetry.LossMarkers)
	fmt.Fprintf(e.out, "  sessions      %d screen(s): %d live, %d expired, %d terminated; %d grant(s) redeemed, %d report(s) owed upstream\n",
		rep.Sessions.Screens, rep.Sessions.Live, rep.Sessions.Expired, rep.Sessions.Terminated, rep.Sessions.RedeemedGrants, rep.Sessions.ReportsOwed)

	if h := rep.Healthz; h != nil {
		if h.Error != "" {
			fmt.Fprintf(e.out, "  healthz       UNREACHABLE %s: %s\n", h.URL, h.Error)
		} else {
			fmt.Fprintf(e.out, "  healthz       HTTP %d in %dms — %s reports %q\n", h.Status, h.LatencyMs, h.Component, h.Reported)
			keys := make([]string, 0, len(h.Vitals))
			for k := range h.Vitals {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(e.out, "    vital %-20s %s\n", k, vitalValue(h.Vitals[k]))
			}
			if len(h.VitalsUnavailable) > 0 {
				fmt.Fprintf(e.out, "    unavailable: %s\n", strings.Join(h.VitalsUnavailable, ", "))
			}
		}
	} else {
		fmt.Fprintln(e.out, "  healthz       (not probed — pass --relay https://host:7421)")
	}

	for _, p := range rep.Problems {
		fmt.Fprintf(e.out, "  PROBLEM       %s\n", p)
	}
	fmt.Fprintln(e.out, "\nNot visible from here (process memory, no durable trace):")
	for _, b := range rep.Blind {
		fmt.Fprintf(e.out, "  - %s\n", b)
	}
}

// vitalValue renders one vital.
//
// JSON has one number type, so every integer arrives as a float64 and
// `%v` prints a disk headroom of 319 GB as `3.19790821376e+11` — a number an
// operator has to decode before they can read it.
func vitalValue(v any) string {
	if f, ok := v.(float64); ok && f == math.Trunc(f) && math.Abs(f) < 1e18 {
		return strconv.FormatInt(int64(f), 10)
	}
	return fmt.Sprint(v)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "NO"
}

func short(s string) string {
	if len(s) > 16 {
		return s[:16] + "…"
	}
	return s
}
