package portscan

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
)

// portscan_test.go pins what the lane reports and what it refuses to invent. An
// open-port list feeds classification, so a port claimed open that is not would
// mislabel a device — and a port silently dropped would leave one unidentified.

// dialer records every address probed and opens only the addresses in `open`.
func dialer(open map[string]bool) (func(context.Context, string) (net.Conn, error), func() []string) {
	var mu sync.Mutex
	var tried []string
	return func(_ context.Context, addr string) (net.Conn, error) {
			mu.Lock()
			tried = append(tried, addr)
			mu.Unlock()
			if open[addr] {
				c1, c2 := net.Pipe()
				_ = c2.Close()
				return c1, nil
			}
			return nil, errors.New("connection refused")
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			out := make([]string, len(tried))
			copy(out, tried)
			return out
		}
}

func TestReportsOnlyPortsThatAccepted(t *testing.T) {
	dial, _ := dialer(map[string]bool{
		"192.168.50.31:8060": true,
		"192.168.50.31:80":   true,
	})
	got := Scan(context.Background(), []string{"192.168.50.31"}, Config{
		Dial: dial, Ports: []int{22, 80, 8060, 9100},
	})
	ports := got["192.168.50.31"]
	if len(ports) != 2 || ports[0] != 80 || ports[1] != 8060 {
		t.Fatalf("open ports = %v, want [80 8060] — sorted, and only what accepted", ports)
	}
}

// A host with nothing open is ABSENT, not present-with-an-empty-list: "we looked
// and found nothing" and "we did not look" are different facts.
func TestAHostWithNothingOpenIsAbsent(t *testing.T) {
	dial, _ := dialer(nil)
	got := Scan(context.Background(), []string{"192.168.50.99"}, Config{Dial: dial, Ports: []int{22, 80}})
	if _, present := got["192.168.50.99"]; present {
		t.Fatalf("a host with no open port appears in the result: %v", got)
	}
}

func TestProbesEveryHostAndPort(t *testing.T) {
	dial, tried := dialer(nil)
	Scan(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, Config{Dial: dial, Ports: []int{22, 443}})
	if n := len(tried()); n != 4 {
		t.Fatalf("probed %d address(es), want 4 (2 hosts x 2 ports): %v", n, tried())
	}
}

// A cancelled scan stops putting connections on the segment.
func TestACancelledScanStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dial, tried := dialer(nil)
	Scan(ctx, []string{"10.0.0.1", "10.0.0.2"}, Config{Dial: dial, Ports: Ports})
	if n := len(tried()); n != 0 {
		t.Fatalf("a cancelled scan probed %d address(es), want none", n)
	}
}

// TestOnePortIsNotRetried: a scan reports what it saw at one moment. Retrying
// would make an intermittently-listening host look reliably open.
func TestOnePortIsNotRetried(t *testing.T) {
	dial, tried := dialer(nil)
	Scan(context.Background(), []string{"10.0.0.1"}, Config{Dial: dial, Ports: []int{22}})
	if n := len(tried()); n != 1 {
		t.Fatalf("probed %d times, want exactly 1 — no retries", n)
	}
}

// The curated set stays small and service-identifying: this is a classification
// aid, not a security scanner, and a long list would look like an attack while
// telling Discovery nothing it acts on.
func TestTheCuratedSetStaysSmallAndMeaningful(t *testing.T) {
	if len(Ports) > 16 {
		t.Fatalf("the curated set has grown to %d ports — it is a classification aid, not a scanner", len(Ports))
	}
	for _, want := range []int{8060, 9100, 631, 445} {
		found := false
		for _, p := range Ports {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("port %d is not probed, so its device kind cannot be identified", want)
		}
	}
}
