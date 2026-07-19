package deviceplane

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/registry"
)

// rel114Credential is a distinctive per-dispatch credential value a
// device.command carries in its params (REL-114). A test asserts this exact
// string never reaches a log sink or a durable store — only the controller,
// in memory, for the dispatch's duration.
const rel114Credential = "s3cr3t-channel-token-DO-NOT-LOG"

// spyCommandLog captures every redacted CommandRecord the surface logs, so a
// test can prove the credential value never reaches the log output (REL-114).
type spyCommandLog struct{ records []CommandRecord }

func (l *spyCommandLog) LogCommand(rec CommandRecord) { l.records = append(l.records, rec) }

// spyCommandJournal captures every record the surface persists to its durable
// operational store / persisted desired state, so a test can prove the
// credential value never reaches durable storage (REL-114).
type spyCommandJournal struct{ records []CommandRecord }

func (j *spyCommandJournal) PersistCommand(rec CommandRecord) { j.records = append(j.records, rec) }

// assertNoCredential scans each captured record — both its JSON encoding and
// its Go-rendered form — for the credential value, failing if it appears
// anywhere in what a sink was handed (REL-114).
func assertNoCredential(t *testing.T, sink string, recs []CommandRecord) {
	t.Helper()
	for i, rec := range recs {
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal %s record[%d]: %v", sink, i, err)
		}
		if strings.Contains(string(raw), rel114Credential) {
			t.Errorf("%s sink record[%d] leaked the credential (JSON): %s", sink, i, raw)
		}
		if strings.Contains(fmt.Sprintf("%+v", rec), rel114Credential) {
			t.Errorf("%s sink record[%d] leaked the credential (Go form): %+v", sink, i, rec)
		}
	}
}

// TestCredentialReachesDeviceButNeverLogOrDurableStore is the REL-114 oracle: a
// resolved device.command whose params carry credential material dispatches
// that credential to the physical device (in memory, allowed), but the credential
// value MUST NOT reach the surface's log sink or its durable store — those sinks
// receive only a redacted, credential-free descriptor of the command.
func TestCredentialReachesDeviceButNeverLogOrDurableStore(t *testing.T) {
	controller := &recordingController{}
	logSpy := &spyCommandLog{}
	journalSpy := &spyCommandJournal{}
	surface := NewCommandSurface(controller, registry.FixtureRegistry{},
		func(entityID string) (string, string, bool) { return entityID, "media-player", true },
		WithCommandLog(logSpy), WithCommandJournal(journalSpy))

	res := surface.Execute(DeviceCommand{
		Type: "device.command", ID: "cmd-cred", RelayID: "relay-1",
		Body: CommandBody{
			EntityID: "e-secret",
			Command:  "launch",
			Params:   map[string]any{"channel": "dev", "auth_token": rel114Credential},
		},
	})
	if !res.Body.OK {
		t.Fatalf("resolved launch should succeed, got %+v", res.Body)
	}

	// The credential MUST reach the physical device for the dispatch (in memory,
	// allowed by REL-114).
	if len(controller.calls) != 1 {
		t.Fatalf("controller dispatch count = %d, want 1", len(controller.calls))
	}
	if controller.calls[0].params["auth_token"] != rel114Credential {
		t.Fatalf("credential must be carried through to the controller, got params=%+v", controller.calls[0].params)
	}

	// ...but MUST NOT reach the log sink or the durable store (REL-114).
	if len(logSpy.records) != 1 || len(journalSpy.records) != 1 {
		t.Fatalf("expected exactly 1 record in each sink, got log=%d journal=%d",
			len(logSpy.records), len(journalSpy.records))
	}
	assertNoCredential(t, "log", logSpy.records)
	assertNoCredential(t, "journal", journalSpy.records)

	// The redacted record still carries the non-credential descriptor.
	rec := logSpy.records[0]
	if rec.EntityID != "e-secret" || rec.Command != "launch" || !rec.OK {
		t.Errorf("redacted record = %+v, want entity/command/ok preserved", rec)
	}
}

// TestCredentialInRejectedCommandNeverLogged asserts REL-114 holds even on the
// reject path: an unresolved command's params (which a caller may still have
// filled with credential material) never reach the log, even though the device
// is never touched.
func TestCredentialInRejectedCommandNeverLogged(t *testing.T) {
	controller := &recordingController{}
	logSpy := &spyCommandLog{}
	surface := NewCommandSurface(controller, registry.FixtureRegistry{},
		func(entityID string) (string, string, bool) { return entityID, "media-player", true },
		WithCommandLog(logSpy))

	res := surface.Execute(DeviceCommand{
		ID: "c", RelayID: "r",
		Body: CommandBody{
			EntityID: "e",
			Command:  "blast", // not in the media-player vocab → COMMAND_UNRESOLVED
			Params:   map[string]any{"auth_token": rel114Credential},
		},
	})
	if res.Body.OK {
		t.Fatal("blast should be rejected as unresolved")
	}
	if len(controller.calls) != 0 {
		t.Fatalf("unresolved command must not touch the device, got %d call(s)", len(controller.calls))
	}
	assertNoCredential(t, "log", logSpy.records)
}
