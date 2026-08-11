package platformlog

import "testing"

// The heuristics, exercised on lines this tree actually writes. Every fixture
// below is a real (or realistically shaped) log line from cmd/waiveo-feeder,
// cmd/waiveo-relay, or net/http's own error logger — a classifier tuned against
// invented text is tuned against nothing.

func TestStripStdlibPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026/08/11 00:26:07 http: TLS handshake error from 127.0.0.1:49951", "http: TLS handshake error from 127.0.0.1:49951"},
		{"2026/08/11 00:26:07.123456 waiveo-feeder: ready", "waiveo-feeder: ready"},
		// No prefix (a logger built with flags 0, or a raw Fprint).
		{"waiveo-feeder: ready", "waiveo-feeder: ready"},
		// Shapes that must NOT be mistaken for a date, or text is eaten off the
		// front of the message — which is where the subject of a log line lives.
		{"2026/08/11 not a timestamped line", "2026/08/11 not a timestamped line"},
		{"12345678901234567890 plain", "12345678901234567890 plain"},
		{"", ""},
		{"short", "short"},
	}
	for _, tc := range cases {
		if got := stripStdlibPrefix(tc.in); got != tc.want {
			t.Errorf("stripStdlibPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSplitSource(t *testing.T) {
	cases := []struct {
		in         string
		wantSource string
		wantMsg    string
	}{
		{"waiveo-feeder: listening on :7420", "waiveo-feeder", "listening on :7420"},
		{"http: TLS handshake error from 127.0.0.1:49951", "http", "TLS handshake error from 127.0.0.1:49951"},
		// A component-plus-detail prefix: only the COMPONENT becomes the source,
		// so one binary is one entry in a filter control rather than four. The
		// detail is not discarded — it stays at the front of the message.
		{"waiveo-relay discovery: reported 3 device candidate(s)", "waiveo-relay", "discovery: reported 3 device candidate(s)"},
		{"waiveo-feeder starting: version=dev channel=dev", "waiveo-feeder", "starting: version=dev channel=dev"},
		{"waiveo-relay dispatch [01ABC]: sent", "waiveo-relay", "dispatch [01ABC]: sent"},
		// A sentence that merely contains a colon is not a component name, and
		// splitting on it would drop the first clause of the message.
		{"the relay said: no", DefaultSource, "the relay said: no"},
		{"a very long run of words indeed that goes on and on and on: yes", DefaultSource, "a very long run of words indeed that goes on and on and on: yes"},
		{"no colon here at all", DefaultSource, "no colon here at all"},
		{": leading colon", DefaultSource, ": leading colon"},
		{"", DefaultSource, ""},
	}
	for _, tc := range cases {
		gotSrc, gotMsg := splitSource(tc.in)
		if gotSrc != tc.wantSource || gotMsg != tc.wantMsg {
			t.Errorf("splitSource(%q) = (%q, %q), want (%q, %q)", tc.in, gotSrc, gotMsg, tc.wantSource, tc.wantMsg)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		in   string
		want Level
	}{
		// Real failures an operator is looking for.
		{"waiveo-relay: reporting 3 screen status entr(ies) failed (retried on the next report): write: broken pipe", LevelError},
		{"http: TLS handshake error from 127.0.0.1:49951", LevelError},
		{"waiveo-feeder: the archive could not be written", LevelError},
		{"waiveo-relay: ECP command timed out after 12s", LevelError},
		{"waiveo-feeder: pack install refused: PACK_UNSIGNED", LevelError},
		// Retries are how this platform works, not a fault. Classifying them as
		// errors would fill the error filter with the system working correctly,
		// which is how an error filter stops being read.
		{"waiveo-relay: program poll failed (keeping current content, never-wipe)", LevelError}, // "failed" wins: it did fail
		{"waiveo-relay: retrying the app peer connection in 2s", LevelWarn},
		{"waiveo-feeder: an optional pack override is stale, skipping", LevelWarn},
		// Ordinary operation.
		{"waiveo-feeder: listening on :7420", LevelInfo},
		{"waiveo-relay discovery: reported 3 device candidate(s) to the app peer", LevelInfo},
		{"waiveo-feeder: pack install trust anchors: /var/lib/waiveo/anchors", LevelInfo},
		{"", LevelInfo},
	}
	for _, tc := range cases {
		if got := classify(tc.in); got != tc.want {
			t.Errorf("classify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestClassifyIsCaseInsensitive — a component that capitalises its messages must
// not vanish from the error filter.
func TestClassifyIsCaseInsensitive(t *testing.T) {
	for _, s := range []string{"ERROR: boom", "Failed to open the store", "PANIC"} {
		if got := classify(s); got != LevelError {
			t.Errorf("classify(%q) = %q, want error", s, got)
		}
	}
}

// TestErrorBeatsWarnWhenALineCarriesBoth. The rule the whole file follows: a
// warning shown among the errors costs a glance; an error hidden among the
// warnings costs the investigation.
func TestErrorBeatsWarnWhenALineCarriesBoth(t *testing.T) {
	const line = "waiveo-relay: degraded — the ECP probe failed, retrying"
	if got := classify(line); got != LevelError {
		t.Errorf("classify(%q) = %q, want error: a line carrying both markers must be classified at the HIGHER severity", line, got)
	}
}

func TestIsComponentName(t *testing.T) {
	good := []string{
		// Every single-word prefix this tree really writes.
		"http", "store", "enroll", "keepalive", "playerserver", "automationhost",
		// Every multi-word one: the first token carries a separator.
		"waiveo-feeder", "waiveo-relay discovery", "waiveo-relay automation engine loaded",
		"waiveo-relay telemetry push", "waiveo-relay dispatch [01ABC]", "app/store", "a.b_c-d",
	}
	bad := []string{
		"", " leading", "trailing ", "double  space", "has,comma", "has:colon", "has(paren)",
		// The discriminating case: an English clause whose words are all valid
		// characters. Its first token carries no separator, so it is not a
		// component name and the line keeps its whole message.
		"the relay said", "a b c d e f g",
	}
	for _, s := range good {
		if !isComponentName(s) {
			t.Errorf("isComponentName(%q) = false", s)
		}
	}
	for _, s := range bad {
		if isComponentName(s) {
			t.Errorf("isComponentName(%q) = true", s)
		}
	}
}
