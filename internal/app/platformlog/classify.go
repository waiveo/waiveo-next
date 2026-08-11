package platformlog

import "strings"

// classify.go reads a level and a source out of a plain log line.
//
// Everything here is a HEURISTIC over text nobody wrote for a machine, and it is
// kept in one small file of pure functions for exactly that reason: the rules
// are the part most likely to need tuning against real output, and they are
// testable without a buffer, a clock, or a server.
//
// The design rule the rules follow: over-classify toward `error`, never under.
// A warning shown in the error filter costs an operator one glance. An error
// hidden in the info filter costs them the failure they were looking for.

// stdlibPrefixLen is the width of the standard logger's default date+time
// prefix: "2006/01/02 15:04:05 " — ten, one, eight, one.
const stdlibPrefixLen = 20

// stripStdlibPrefix removes the standard logger's own "YYYY/MM/DD HH:MM:SS "
// prefix when present, including the optional ".ffffff" microseconds the
// Lmicroseconds flag adds.
//
// The prefix is removed rather than parsed. It is written in the process's LOCAL
// time with no zone attached, and every other instant this platform publishes is
// epoch-millis UTC; a page that mixed the two would show two clocks and let an
// operator correlate the wrong pair of events. The capture instant is used
// instead, which is within microseconds of when the line was written and is in
// the same clock as everything else on the page.
func stripStdlibPrefix(line string) string {
	if !looksLikeStdlibDate(line) {
		return line
	}
	rest := line[stdlibPrefixLen:]
	// Lmicroseconds: the seconds field is followed by ".ffffff" and then the
	// space. The check above matched through the space at index 19, so this only
	// fires for the flagged form where index 19 is '.'.
	if len(line) > stdlibPrefixLen && line[19] == '.' {
		if i := strings.IndexByte(line[19:], ' '); i >= 0 {
			rest = line[19+i+1:]
		}
	}
	return rest
}

// looksLikeStdlibDate reports whether line opens with "dddd/dd/dd dd:dd:dd"
// followed by a space or a '.'. Positional rather than a regexp: this runs on
// every captured line, and the shape is fixed by the standard library.
func looksLikeStdlibDate(line string) bool {
	if len(line) < stdlibPrefixLen {
		return false
	}
	digits := []int{0, 1, 2, 3, 5, 6, 8, 9, 11, 12, 14, 15, 17, 18}
	for _, i := range digits {
		if line[i] < '0' || line[i] > '9' {
			return false
		}
	}
	return line[4] == '/' && line[7] == '/' && line[10] == ' ' &&
		line[13] == ':' && line[16] == ':' && (line[19] == ' ' || line[19] == '.')
}

// maxSourceLen bounds what may be taken as a source prefix. Log lines in this
// tree open with a short component name ("waiveo-feeder: ", "waiveo-relay
// discovery: ", "http: "); a long run of text before the first colon is a
// sentence that happens to contain one, not a component.
const maxSourceLen = 40

// splitSource takes the component prefix off a line, returning the source and
// the remaining message.
//
// The prefix is accepted only when it looks like a component name: at most
// maxSourceLen bytes, made of lowercase letters, digits, and the few separators
// this tree's prefixes use, with single spaces allowed between words
// ("waiveo-relay discovery"). Anything else leaves the line intact under
// DefaultSource, which is the failure mode to prefer — a mis-split line loses
// text off the front of the message, and the front is where the subject is.
func splitSource(line string) (source, message string) {
	i := strings.Index(line, ": ")
	if i <= 0 || i > maxSourceLen {
		return DefaultSource, line
	}
	candidate := line[:i]
	if !isComponentName(candidate) {
		return DefaultSource, line
	}
	return candidate, strings.TrimSpace(line[i+2:])
}

// maxSourceWords bounds a multi-word prefix. This tree's longest real one is
// "waiveo-relay automation engine loaded" (four); six leaves room without
// admitting a clause.
const maxSourceWords = 6

// isComponentName reports whether s reads as a log component prefix.
//
// Character set first: letters, digits, and the separators component names in
// this tree use. Then the rule that does the actual discriminating —
//
//	a prefix containing a SPACE must have a first token that carries one of
//	`-._/`
//
// — which is what separates a real multi-word prefix ("waiveo-relay discovery",
// "waiveo-relay automation engine loaded") from an ordinary English clause that
// happens to precede a colon ("the relay said: no"). Both match the character
// set and both are short; only the first opens with something shaped like a
// component identifier. Without this rule, "the relay said: no" is captured with
// source "the relay said" and message "no", and the operator loses the subject
// of the sentence off the front of the line — the exact failure splitSource's
// doc says to prefer avoiding.
func isComponentName(s string) bool {
	if s == "" {
		return false
	}
	prevSpace := true // leading space disallowed
	words := 1
	firstTokenSeparated := false
	inFirstToken := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			prevSpace = false
		case c == '-', c == '_', c == '.', c == '/':
			if inFirstToken {
				firstTokenSeparated = true
			}
			prevSpace = false
		case c == ' ':
			if prevSpace {
				return false // no double spaces, no leading space
			}
			inFirstToken = false
			words++
			prevSpace = true
		default:
			return false
		}
	}
	// A trailing space before the colon would mean the "prefix" was the tail of
	// a sentence.
	if prevSpace || words > maxSourceWords {
		return false
	}
	return words == 1 || firstTokenSeparated
}

// errorMarkers are substrings that make a line an error. They are matched
// case-insensitively against the WHOLE line, including its source prefix.
//
// The list is phrase-level rather than word-level where it can be ("could not",
// "unable to"), because those are how this tree's log lines actually spell a
// failure and a bare "not" would match everything.
var errorMarkers = []string{
	"panic", "fatal", "error", "failed", "failure",
	"refused", "rejected", "unable to", "could not", "cannot ",
	"crash", "timed out", "timeout",
}

// warnMarkers are substrings that make a line a warning, checked only after
// every error marker has missed.
//
// "retrying" and "backing off" are here rather than in errorMarkers on purpose:
// this platform retries by design (a player that beat its relay to power-on, a
// relay redialling its app peer), and classifying every retry as an error would
// fill the error filter with the system working correctly — which is how an
// error filter stops being read.
var warnMarkers = []string{
	"warn", "deprecat", "retrying", "backing off", "degraded",
	"stale", "quarantin", "skipping", "ignored",
}

// classify derives a level from a line.
func classify(line string) Level {
	l := strings.ToLower(line)
	for _, m := range errorMarkers {
		if strings.Contains(l, m) {
			return LevelError
		}
	}
	for _, m := range warnMarkers {
		if strings.Contains(l, m) {
			return LevelWarn
		}
	}
	return LevelInfo
}
