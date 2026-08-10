package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// devInstallerPort is the port the Roku DEVELOPER INSTALLER listens on. It is
// plain HTTP :80, and it is a different surface from ECP (:8060, no auth):
// /plugin_install speaks multipart/form-data behind HTTP Digest, while ECP
// speaks unauthenticated XML/keypress. Confusing the two is the single easiest
// way to make this tool "fail to connect" against a perfectly healthy screen,
// which is why the two entry points below differ ONLY in what they do with a
// port they were handed.
const devInstallerPort = 80

// device is one sideload target: a human-facing label (whatever the roster
// called it — an entity id, a room name, or just the host) and the dev
// installer address to POST to.
type device struct {
	// Name is what the report prints. Never used to address anything.
	Name string
	// Host is the IP or hostname of the Roku.
	Host string
	// Port is the dev installer port on that Roku — devInstallerPort unless a
	// caller explicitly said otherwise.
	Port int
}

// Addr is the host:port this device's installer is dialed at.
func (d device) Addr() string {
	return net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
}

// parseDeviceList parses the -devices flag: a comma-separated list of
// `[name=]host[:port]` entries.
//
// A port here IS honoured, because the only way a human types one into this
// flag is deliberately (an SSH tunnel, a lab proxy, a non-standard image). An
// entry with no port gets devInstallerPort.
//
// A malformed entry fails the whole parse rather than being skipped. This tool
// exists to push a build to a wall of screens nobody is standing in front of;
// silently dropping one and reporting "6/6 succeeded" would leave a screen on
// the old build with nothing anywhere saying so.
func parseDeviceList(raw string) ([]device, error) {
	entries := splitList(raw)
	if len(entries) == 0 {
		return nil, nil
	}
	devices := make([]device, 0, len(entries))
	for _, entry := range entries {
		name, addr := "", entry
		if before, after, ok := strings.Cut(entry, "="); ok {
			name, addr = strings.TrimSpace(before), strings.TrimSpace(after)
		}
		if addr == "" {
			return nil, fmt.Errorf("device entry %q has no host", entry)
		}
		host, port, err := splitHostPort(addr, devInstallerPort)
		if err != nil {
			return nil, fmt.Errorf("device entry %q: %w", entry, err)
		}
		if name == "" {
			name = host
		}
		devices = append(devices, device{Name: name, Host: host, Port: port})
	}
	return devices, nil
}

// parseECPTargetDevices parses the relay's own WAIVEO_RELAY_ECP_TARGETS
// grammar — `entity=host[:port],...`, exactly as cmd/waiveo-relay's
// parseECPTargets reads it — into sideload targets, so "the fleet this relay
// drives" and "the fleet this tool updates" cannot drift apart by being typed
// out twice.
//
// It DISCARDS the port every entry carries, unlike parseDeviceList. That is
// the whole reason this is a separate entry point rather than a flag: a port
// in that variable is an ECP port (:8060), the relay's polling and command
// surface, and POSTing a sideload to it reaches a service that has never heard
// of /plugin_install. Only the host is transferable between the two surfaces.
//
// An entity id is used as the device's label, which is exactly what an
// operator wants in the report: the same name the relay, the console, and this
// tool all call that screen.
func parseECPTargetDevices(raw string) ([]device, error) {
	entries := splitList(raw)
	if len(entries) == 0 {
		return nil, nil
	}
	devices := make([]device, 0, len(entries))
	for _, entry := range entries {
		entityID, addr, ok := strings.Cut(entry, "=")
		entityID, addr = strings.TrimSpace(entityID), strings.TrimSpace(addr)
		if !ok || entityID == "" || addr == "" {
			return nil, fmt.Errorf("ECP target entry %q is not entity=host[:port]", entry)
		}
		host, _, err := splitHostPort(addr, devInstallerPort)
		if err != nil {
			return nil, fmt.Errorf("ECP target entry %q: %w", entry, err)
		}
		devices = append(devices, device{Name: entityID, Host: host, Port: devInstallerPort})
	}
	return devices, nil
}

// splitList splits a comma-separated list, trimming each entry and dropping
// empty ones — so a trailing comma or a line wrapped for readability is not an
// error a human has to hunt for.
func splitList(raw string) []string {
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// splitHostPort splits `host` / `host:port` / `[v6]:port`, defaulting the port
// to def. A bare IPv6 literal with no port (`::1`) is treated as a host, not as
// a malformed host:port — net.SplitHostPort cannot tell those apart, so the
// colon count is what decides.
func splitHostPort(addr string, def int) (string, int, error) {
	if !strings.Contains(addr, ":") || (strings.Count(addr, ":") > 1 && !strings.HasPrefix(addr, "[")) {
		return addr, def, nil
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("%q is not host[:port]: %w", addr, err)
	}
	if host == "" {
		return "", 0, fmt.Errorf("%q has no host", addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("%q has a bad port", addr)
	}
	return host, port, nil
}
