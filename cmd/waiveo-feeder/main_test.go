package main

import "testing"

func TestLoadConfigDefaultsAreLoopback(t *testing.T) {
	// No env set -> the exact Wave-1 loopback behavior, so make dev / CI are
	// unchanged by the config plumbing.
	cfg := loadConfig(func(string) string { return "" })
	if cfg.listen != "127.0.0.1:7420" {
		t.Errorf("default listen = %q, want 127.0.0.1:7420", cfg.listen)
	}
	if cfg.contentBaseURL != "https://127.0.0.1:7420" {
		t.Errorf("default contentBaseURL = %q, want https://127.0.0.1:7420", cfg.contentBaseURL)
	}
}

func TestLoadConfigContentURLDefaultsToListen(t *testing.T) {
	// Overriding only the listen address carries into the content base URL, so
	// a screen's direct fetch targets the same host the feeder binds — unless
	// an explicit content URL says otherwise (next test).
	env := map[string]string{"WAIVEO_FEEDER_LISTEN": "0.0.0.0:7420"}
	cfg := loadConfig(func(k string) string { return env[k] })
	if cfg.listen != "0.0.0.0:7420" {
		t.Errorf("listen = %q, want 0.0.0.0:7420", cfg.listen)
	}
	if cfg.contentBaseURL != "https://0.0.0.0:7420" {
		t.Errorf("contentBaseURL = %q, want https://0.0.0.0:7420", cfg.contentBaseURL)
	}
}

func TestLoadConfigExplicitContentURLWins(t *testing.T) {
	// The real on-box shape: bind all interfaces, but advertise the LAN IP the
	// Roku can actually reach for the direct content fetch.
	env := map[string]string{
		"WAIVEO_FEEDER_LISTEN":      "0.0.0.0:7420",
		"WAIVEO_FEEDER_CONTENT_URL": "https://192.0.2.12:7420",
	}
	cfg := loadConfig(func(k string) string { return env[k] })
	if cfg.contentBaseURL != "https://192.0.2.12:7420" {
		t.Errorf("contentBaseURL = %q, want the explicit LAN URL", cfg.contentBaseURL)
	}
}
