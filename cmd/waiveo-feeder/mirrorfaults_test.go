package main

import (
	"testing"
	"time"
)

// mirrorfaults_test.go pins the reporting discipline that replaced 1017
// identical log lines. The claims are about WHAT GETS SAID, so they are made
// against the tracker directly rather than by scraping a log.

// TestMirrorFaultsReportsTheFirstFailureImmediately: whatever the throttle does
// later, the first failure must never be held back. An operator watching a
// deploy has to see it at once.
func TestMirrorFaultsReportsTheFirstFailureImmediately(t *testing.T) {
	var f mirrorFaults
	report, consecutive, lasted := f.failed(1_000)
	if !report {
		t.Fatalf("the first failure must be reported immediately")
	}
	if consecutive != 1 {
		t.Fatalf("first failure should count 1, got %d", consecutive)
	}
	if lasted != 0 {
		t.Fatalf("the first failure has lasted no time yet, got %s", lasted)
	}
}

// TestMirrorFaultsCollapsesARepeatingFailure is the actual defect this closes.
// Box .12 logged 1017 identical lines over seven days. The same run must now
// produce a handful, each one carrying the count and the duration.
func TestMirrorFaultsCollapsesARepeatingFailure(t *testing.T) {
	var f mirrorFaults

	// Box .12's actual numbers: 1017 failed mirror writes spread over seven days.
	const (
		reports  = 1017
		outage   = 7 * 24 * time.Hour
		startsAt = 1_752_800_000_000
	)
	everyMs := int64(outage/time.Millisecond) / reports

	reported := 0
	var lastCount int
	var lastLasted time.Duration
	for i := 0; i < reports; i++ {
		report, consecutive, lasted := f.failed(startsAt + int64(i)*everyMs)
		if report {
			reported++
			lastCount, lastLasted = consecutive, lasted
		}
	}

	if reported > 40 {
		t.Fatalf("a week-long fault must not produce a line per report; got %d lines for %d failures", reported, reports)
	}
	if reported < 5 {
		t.Fatalf("a week-long fault must not go effectively silent either; got only %d lines", reported)
	}
	// Every line has to carry the scale of the problem, which is exactly what the
	// old line could not express: how many, and for how long.
	if lastCount < reports/2 {
		t.Fatalf("the last line must report the true count, got %d of %d", lastCount, reports)
	}
	// And the TAIL must not be silent: doubling alone would put the last line
	// hours or days before the end, so anyone reading recent logs during a long
	// outage would see a healthy-looking mirror.
	if outage-lastLasted > 12*time.Hour {
		t.Fatalf("the last line came %s before the fault was still ongoing; a long outage must keep saying so",
			outage-lastLasted)
	}
}

// TestMirrorFaultsReportsRecovery: a fault that stops has to say so, once.
// "No issues" and "the reporting stopped" are different facts and only one of
// them deserves silence.
func TestMirrorFaultsReportsRecovery(t *testing.T) {
	var f mirrorFaults
	for i := 0; i < 10; i++ {
		f.failed(1_000 + int64(i)*1_000)
	}

	report, consecutive, lasted := f.recovered(11_000)
	if !report {
		t.Fatalf("a mirror that starts working again must say so")
	}
	if consecutive != 10 {
		t.Fatalf("recovery must report how many failed, got %d", consecutive)
	}
	if lasted != 10*time.Second {
		t.Fatalf("recovery must report how long it lasted, got %s", lasted)
	}

	// Only once: a healthy mirror is silent.
	if report, _, _ := f.recovered(12_000); report {
		t.Fatalf("a mirror that was never failing must say nothing")
	}

	// And the next fault is a fresh run, reported immediately again.
	if report, consecutive, _ := f.failed(13_000); !report || consecutive != 1 {
		t.Fatalf("a new run must start over; report=%t consecutive=%d", report, consecutive)
	}
}

// TestMirrorFaultsNilReportsEverything: a sink built without a tracker must
// degrade to the OLD noisy behaviour, never to silence. Losing a log line is a
// worse failure than repeating one.
func TestMirrorFaultsNilReportsEverything(t *testing.T) {
	var f *mirrorFaults
	for i := 0; i < 3; i++ {
		if report, _, _ := f.failed(int64(i)); !report {
			t.Fatalf("a nil tracker must report every failure")
		}
	}
	if report, _, _ := f.recovered(9); report {
		t.Fatalf("a nil tracker has no run to recover from")
	}
}
