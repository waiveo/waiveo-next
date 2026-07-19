// Package deviceplane is the relay-side surface of relay/1's device plane
// (contracts/relay-1.md, REL-110–115): the candidate set the relay's own
// discovery observes, and the device-command surface the app peer dispatches
// resolved operations through. This file (Task 1) carries only the discovery
// `match` model manifest/1 MAN-071 defines and REL-111 dedups candidates by.
package deviceplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// ErrUnknownDeviceMatchForm is the sentinel a caller checks with errors.Is
// to detect UNKNOWN_DEVICE_MATCH_FORM (manifest/1 MAN-071, Error taxonomy):
// a match that is not a JSON object, that sets zero or more than one of
// {ssdp, mdns, macOui}, that carries an unrecognized field, or whose
// macOui value is not a 6-hex-digit prefix.
var ErrUnknownDeviceMatchForm = errors.New("deviceplane: match is not exactly one of the three recognized forms {ssdp|mdns|macOui} (UNKNOWN_DEVICE_MATCH_FORM)")

// macOuiPattern is MAN-071's "6-hex-digit prefix": exactly six hex digits,
// upper or lower case.
var macOuiPattern = regexp.MustCompile(`^[0-9A-Fa-f]{6}$`)

// Match is one manifest/1 MAN-071 discovery-match pattern: exactly one of
// SSDP (a search-target string), MDNS (a service-type string), or MacOui (a
// 6-hex-digit MAC-OUI prefix) is non-empty; the other two are the zero
// value. Construct a Match via ParseMatch rather than the struct literal
// directly, so the exactly-one-form invariant is enforced.
type Match struct {
	SSDP   string
	MDNS   string
	MacOui string
}

// matchWireForm is the wire object {ssdp?, mdns?, macOui?} MAN-071 defines —
// used both to decode an incoming raw match (ParseMatch) and, via Match's
// MarshalJSON, to re-encode a Match back to that same shape (consumed by
// device.candidates, REL-110).
type matchWireForm struct {
	SSDP   *string `json:"ssdp,omitempty"`
	MDNS   *string `json:"mdns,omitempty"`
	MacOui *string `json:"macOui,omitempty"`
}

// ParseMatch parses a raw JSON match object into a Match, enforcing
// MAN-071: the value MUST be a JSON object setting exactly one of
// ssdp/mdns/macOui (no unrecognized fields, no zero or multi-field forms),
// and a macOui value MUST be a 6-hex-digit prefix. Any violation errors
// ErrUnknownDeviceMatchForm (UNKNOWN_DEVICE_MATCH_FORM).
func ParseMatch(raw json.RawMessage) (Match, error) {
	var w matchWireForm
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Match{}, fmt.Errorf("%w: %v", ErrUnknownDeviceMatchForm, err)
	}

	set := 0
	if w.SSDP != nil {
		set++
	}
	if w.MDNS != nil {
		set++
	}
	if w.MacOui != nil {
		set++
	}
	if set != 1 {
		return Match{}, fmt.Errorf("%w: exactly one of ssdp/mdns/macOui MUST be set, found %d", ErrUnknownDeviceMatchForm, set)
	}

	switch {
	case w.SSDP != nil:
		if *w.SSDP == "" {
			return Match{}, fmt.Errorf("%w: ssdp value MUST be a non-empty search-target string", ErrUnknownDeviceMatchForm)
		}
		return Match{SSDP: *w.SSDP}, nil
	case w.MDNS != nil:
		if *w.MDNS == "" {
			return Match{}, fmt.Errorf("%w: mdns value MUST be a non-empty service-type string", ErrUnknownDeviceMatchForm)
		}
		return Match{MDNS: *w.MDNS}, nil
	default:
		if !macOuiPattern.MatchString(*w.MacOui) {
			return Match{}, fmt.Errorf("%w: macOui %q is not a 6-hex-digit prefix", ErrUnknownDeviceMatchForm, *w.MacOui)
		}
		return Match{MacOui: *w.MacOui}, nil
	}
}

// MarshalJSON encodes m back to MAN-071's wire shape: exactly one of
// {"ssdp":…}/{"mdns":…}/{"macOui":…}, chosen by whichever of m's three
// fields is non-empty (SSDP, then MDNS, then MacOui, in that precedence —
// a Match built by ParseMatch always has exactly one set).
func (m Match) MarshalJSON() ([]byte, error) {
	switch {
	case m.SSDP != "":
		return json.Marshal(matchWireForm{SSDP: &m.SSDP})
	case m.MDNS != "":
		return json.Marshal(matchWireForm{MDNS: &m.MDNS})
	case m.MacOui != "":
		return json.Marshal(matchWireForm{MacOui: &m.MacOui})
	default:
		return nil, fmt.Errorf("%w: match has none of ssdp/mdns/macOui set", ErrUnknownDeviceMatchForm)
	}
}

// Key returns a canonical string key for m, stable for equal Match values
// and distinct across the three forms (even when two forms coincidentally
// share the same raw value) — REL-111's dedup-by-match key for the
// candidate store.
func (m Match) Key() string {
	switch {
	case m.SSDP != "":
		return "ssdp:" + m.SSDP
	case m.MDNS != "":
		return "mdns:" + m.MDNS
	case m.MacOui != "":
		return "macOui:" + m.MacOui
	default:
		return ""
	}
}
