package main

import "testing"

func TestLoadConfigDefaultsAreLoopback(t *testing.T) {
	cfg, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("loadConfig(defaults): %v", err)
	}
	if cfg.listen != "127.0.0.1:7421" {
		t.Errorf("default listen = %q, want 127.0.0.1:7421", cfg.listen)
	}
	if cfg.feederURL != "https://127.0.0.1:7420" {
		t.Errorf("default feederURL = %q, want https://127.0.0.1:7420", cfg.feederURL)
	}
	if cfg.pairHost != "127.0.0.1" {
		t.Errorf("default pairHost = %q, want 127.0.0.1", cfg.pairHost)
	}
	if cfg.pairPort != 7421 {
		t.Errorf("default pairPort = %d, want 7421", cfg.pairPort)
	}
}

func TestLoadConfigOnBoxOverride(t *testing.T) {
	// The on-box first-photon shape: bind LAN-reachable, tell the screen to dial
	// the box's LAN IP, keep the feeder loopback (co-located).
	env := map[string]string{
		"WAIVEO_RELAY_LISTEN":    "0.0.0.0:7421",
		"WAIVEO_RELAY_PAIR_HOST": "192.0.2.12",
		"WAIVEO_RELAY_PAIR_PORT": "7421",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig(override): %v", err)
	}
	if cfg.listen != "0.0.0.0:7421" {
		t.Errorf("listen = %q, want 0.0.0.0:7421", cfg.listen)
	}
	if cfg.pairHost != "192.0.2.12" {
		t.Errorf("pairHost = %q, want the LAN IP a screen must dial", cfg.pairHost)
	}
	if cfg.feederURL != "https://127.0.0.1:7420" {
		t.Errorf("feederURL = %q, want the loopback default (feeder is co-located)", cfg.feederURL)
	}
}

func TestLoadConfigRejectsNonIntegerPairPort(t *testing.T) {
	// A bad port must fail fast at startup, not silently emit a pairing code no
	// screen can dial.
	env := map[string]string{"WAIVEO_RELAY_PAIR_PORT": "not-a-port"}
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("loadConfig accepted a non-integer WAIVEO_RELAY_PAIR_PORT, want error")
	}
}
