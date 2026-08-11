package main

import (
	"encoding/json"

	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
)

// wireInteractionRecorder installs the RETURN PATH: an accepted viewer press on
// an interactive slide layer (POST /player/v1/interaction) is recorded into the
// SAME durable telemetry buffer a fired rule's automation.run goes into, as an
// events/1 `screen.interaction` (EVT-055). The telemetry Channel this relay
// builds pushes that buffer to the app peer, so a press reaches the app's
// durable event log — and any automation with a matching `event` trigger — over
// the one path that already handles buffering, retry, acking and loss marking.
// A press made while the app peer is unreachable is retained and delivered when
// it returns, with nothing here that knows about outages.
//
// It MUST be installed before srv.Register mounts the player routes, for the
// reason SetSigningKey and EnablePersistence are: with no recorder installed
// interaction.go answers 503, and refusing a real press is exactly what this
// ordering avoids.
//
// # Why this is a function rather than four lines inside main
//
// Because those four lines were the whole feature's only link to a real box,
// and nothing could say anything about them. Deleting them left the entire
// suite green: every player-side test installs its own recorder, so the
// mechanism was covered from every angle while "does the shipped relay install
// one?" was answered by nobody — the identical hole
// signingkeywiring_test.go was written for after it had already cost us once.
//
// As a named function it can be driven for real (interactionwiring_test.go
// posts a press at the live route and reads the buffer), and main's call to it
// can be asserted on. Those are two independent failure modes, and the argument
// pass-through in the middle — five parameters in one order, silently wrong if
// two of the same type are swapped — is exactly the part a source-reading guard
// could never see.
func wireInteractionRecorder(srv *playerserver.Server, buf *telemetry.Buffer) {
	srv.SetInteractionRecorder(func(schema string, payload json.RawMessage, subject string, atMs int64, traceID string) {
		buf.RecordTraced(schema, payload, subject, atMs, traceID)
	})
}
