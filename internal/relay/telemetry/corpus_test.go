package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// corpus_test.go (Task 5) is the end-to-end oracle the plan names: it wires the
// automation.run emitter (AutomationRunEntry) into the real Buffer + Channel and
// replays the frozen relay/1 corpus cases through the whole durable pipeline —
// REL-090 (bounded-buffer overflow → drop-oldest → exact loss marker, no silent
// loss) and REL-094 (latest-only heartbeat superseded, never pushed, not counted
// as loss) — alongside the events/1 EVT-040/041 automation.run payload shape the
// engine's RunDisposition maps into. buffer_test.go / overflow_test.go /
// channel_test.go already gate each stage in isolation; this driver is the one
// place the emitter-through-channel path is proven end-to-end against the corpus's
// own expected fields (silent_loss:false, the 4-field loss-marker shape,
// seq_pushed:false, dropped_counts_by_schema exclusion), never invented.

// rel090Expected is the subset of REL-090's expected block this driver asserts.
type rel090Expected struct {
	LossMarkerFieldCount                int      `json:"loss_marker_field_count"`
	LossMarkerFields                    []string `json:"loss_marker_fields"`
	DroppedCountsSumMatchesDroppedRange bool     `json:"dropped_counts_sum_matches_dropped_range"`
	SilentLoss                          bool     `json:"silent_loss"`
}

func loadREL090Expected(t *testing.T) rel090Expected {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(rel090Corpus))
	if err != nil {
		t.Fatalf("reading REL-090 corpus: %v", err)
	}
	var doc struct {
		Expected rel090Expected `json:"expected"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding REL-090 corpus: %v", err)
	}
	return doc.Expected
}

// rel094Expected is the subset of REL-094's expected block this driver asserts.
type rel094Expected struct {
	Seq2001Discarded                             bool            `json:"seq_2001_discarded"`
	Seq2001Pushed                                bool            `json:"seq_2001_pushed"`
	LossMarkerGenerated                          bool            `json:"loss_marker_generated"`
	DroppedCountsBySchemaIncludesDeviceHeartbeat bool            `json:"dropped_counts_by_schema_includes_device_heartbeat"`
	AppPeerFinalViewOfDevice                     json.RawMessage `json:"app_peer_final_view_of_device"`
}

func loadREL094Expected(t *testing.T) rel094Expected {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(rel094Corpus))
	if err != nil {
		t.Fatalf("reading REL-094 corpus: %v", err)
	}
	var doc struct {
		Expected rel094Expected `json:"expected"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding REL-094 corpus: %v", err)
	}
	return doc.Expected
}

// TestCorpusEVT040_041FlowsThroughDurableChannel drives every frozen events/1
// automation.run disposition (EVT-040 ran, EVT-041 misfire-caught/restarted/
// skipped) through the emitter → Buffer → Channel and proves each rides the
// push as a durable-class automation.run entry with its corpus mode_disposition,
// loss_markers present-but-empty (REL-090), and the buffer drains on the ack.
func TestCorpusEVT040_041FlowsThroughDurableChannel(t *testing.T) {
	cases := []evtRunCase{
		loadEvtRunCase(t, evt040Corpus),
		loadEvtRunCase(t, evt041MisfireCorpus),
		loadEvtRunCase(t, evt041RestartedCorpus),
		loadEvtRunCase(t, evt041SkippedCorpus),
	}

	buf := NewBuffer(500)
	for _, c := range cases {
		schema, payload, subject := AutomationRunEntry(dispositionFrom(c), 0)
		buf.Record(schema, payload, subject, 0)
	}

	highest := buf.Pending()[len(buf.Pending())-1].Seq
	up := &scriptedUpstream{responses: []upstreamResponse{{ack: Ack{AckThroughSeq: highest}}}}
	ch := NewChannel(buf, up, ExponentialBackoff{Base: time.Millisecond, MaxAttempts: 1})
	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	pushed := up.lastPush()
	if len(pushed.Entries) != len(cases) {
		t.Fatalf("pushed %d entries, want %d (every automation.run is durable, REL-093)", len(pushed.Entries), len(cases))
	}
	if pushed.LossMarkers == nil {
		t.Fatalf("loss_markers nil; REL-090 requires it present on every push")
	}
	if len(pushed.LossMarkers) != 0 {
		t.Fatalf("loss_markers = %d, want 0 (nothing dropped)", len(pushed.LossMarkers))
	}
	for i, c := range cases {
		e := pushed.Entries[i]
		if e.Schema != SchemaAutomationRun {
			t.Errorf("%s: pushed schema = %q, want automation.run", c.CaseID, e.Schema)
		}
		// The DELIVERED wire entry's payload must carry all seven EVT-040 mandatory
		// fields (REL-090 — payload = that schema's own field shape, unmodified;
		// EVT-043 observability). A payload dropping rule_revision/trigger_snapshot/
		// condition_results/action_outcomes en route to the wire is non-conformant.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(e.Payload, &raw); err != nil {
			t.Fatalf("%s: payload decode: %v", c.CaseID, err)
		}
		for _, k := range []string{"rule_id", "rule_revision", "trigger_snapshot", "condition_results", "action_outcomes", "mode_disposition", "misfire_caught"} {
			if _, ok := raw[k]; !ok {
				t.Errorf("%s: delivered payload missing EVT-040 mandatory field %q: %s", c.CaseID, k, e.Payload)
			}
		}
		var got struct {
			RuleID          string `json:"rule_id"`
			RuleRevision    int    `json:"rule_revision"`
			ModeDisposition string `json:"mode_disposition"`
			MisfireCaught   bool   `json:"misfire_caught"`
		}
		if err := json.Unmarshal(e.Payload, &got); err != nil {
			t.Fatalf("%s: payload decode: %v", c.CaseID, err)
		}
		if got.RuleID != c.Expected.Envelope.Payload.RuleID {
			t.Errorf("%s: pushed rule_id = %q, want %q", c.CaseID, got.RuleID, c.Expected.Envelope.Payload.RuleID)
		}
		if got.RuleRevision != c.Expected.Envelope.Payload.RuleRevision {
			t.Errorf("%s: pushed rule_revision = %d, want %d", c.CaseID, got.RuleRevision, c.Expected.Envelope.Payload.RuleRevision)
		}
		if got.ModeDisposition != c.Expected.Envelope.Payload.ModeDisposition {
			t.Errorf("%s: pushed mode_disposition = %q, want %q", c.CaseID, got.ModeDisposition, c.Expected.Envelope.Payload.ModeDisposition)
		}
		if got.MisfireCaught != c.Expected.Envelope.Payload.MisfireCaught {
			t.Errorf("%s: pushed misfire_caught = %v, want %v", c.CaseID, got.MisfireCaught, c.Expected.Envelope.Payload.MisfireCaught)
		}
		if !jsonEqual(t, raw["trigger_snapshot"], c.Expected.Envelope.Payload.TriggerSnapshot) {
			t.Errorf("%s: pushed trigger_snapshot = %s, want %s", c.CaseID, raw["trigger_snapshot"], c.Expected.Envelope.Payload.TriggerSnapshot)
		}
	}

	if len(buf.Pending()) != 0 {
		t.Fatalf("Pending after ack = %d, want 0 (cursor drained, REL-092)", len(buf.Pending()))
	}
}

// TestCorpusREL090AutomationRunSurvivesOverflowNoSilentLoss replays REL-090
// end-to-end with an automation.run survivor emitted through AutomationRunEntry:
// durable victims (the corpus content.played + entity.state_changed breakdown)
// overflow out under drop-oldest, the surviving automation.run entry rides the
// push, and the loss marker accounts for EXACTLY the drop — the corpus's own
// silent_loss:false + 4-field loss-marker shape (REL-096/100/103/105).
func TestCorpusREL090AutomationRunSurvivesOverflowNoSilentLoss(t *testing.T) {
	capacity, _, dropFrom, dropTo, breakdown := loadREL090AckAndMarker(t)
	exp := loadREL090Expected(t)
	if exp.SilentLoss {
		t.Fatal("corpus self-check: REL-090 expects silent_loss:false")
	}
	if exp.LossMarkerFieldCount != 4 {
		t.Fatalf("corpus self-check: loss_marker_field_count = %d, want 4", exp.LossMarkerFieldCount)
	}

	dropCount := int(dropTo-dropFrom) + 1
	if dropCount != sumCounts(breakdown) {
		t.Fatalf("corpus self-check: drop range width %d != breakdown sum %d", dropCount, sumCounts(breakdown))
	}

	buf := NewBuffer(capacity)
	// Victims first (durable content.played + entity.state_changed per breakdown).
	var victimSchemas []string
	for schema, n := range breakdown {
		for i := 0; i < n; i++ {
			victimSchemas = append(victimSchemas, schema)
		}
	}
	for _, s := range victimSchemas {
		buf.Record(s, json.RawMessage(`{"k":"v"}`), "", 0)
	}
	firstVictim := buf.Pending()[0].Seq
	lastVictim := buf.Pending()[len(buf.Pending())-1].Seq

	// Survivors overflow the victims out. The FIRST survivor is a genuine
	// automation.run emitted through the engine→emitter path (ties the emitter
	// into the overflow pipeline); the rest fill capacity.
	ranC := loadEvtRunCase(t, evt040Corpus)
	arSchema, arPayload, arSubject := AutomationRunEntry(dispositionFrom(ranC), 0)
	buf.Record(arSchema, arPayload, arSubject, 0)
	for i := 1; i < capacity; i++ {
		buf.Record(SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:x","screen_id":"s"}`), "", 0)
	}

	// Exactly one loss marker, accounting for exactly the corpus breakdown.
	markers := buf.PendingLossMarkers()
	if len(markers) != 1 {
		t.Fatalf("PendingLossMarkers = %d, want 1", len(markers))
	}
	m := markers[0]
	if m.FromSeq != firstVictim || m.ToSeq != lastVictim {
		t.Fatalf("loss marker range [%d,%d] != dropped victim range [%d,%d]", m.FromSeq, m.ToSeq, firstVictim, lastVictim)
	}
	if m.Reason != ReasonBufferExceeded {
		t.Fatalf("loss marker reason = %q, want %q (REL-101)", m.Reason, ReasonBufferExceeded)
	}
	for schema, want := range breakdown {
		if m.DroppedCountsBySchema[schema] != want {
			t.Fatalf("dropped_counts_by_schema[%q] = %d, want %d", schema, m.DroppedCountsBySchema[schema], want)
		}
	}
	// silent_loss:false — every dropped durable entry is accounted for (REL-103).
	sum := 0
	for _, n := range m.DroppedCountsBySchema {
		sum += n
	}
	if exp.DroppedCountsSumMatchesDroppedRange && sum != dropCount {
		t.Fatalf("dropped counts sum %d != range width %d (silent loss, REL-103)", sum, dropCount)
	}

	// The loss marker is exactly the 4 fields REL-100/105 name — no more, no less.
	wireM, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal loss marker: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wireM, &fields); err != nil {
		t.Fatalf("decode loss marker fields: %v", err)
	}
	if len(fields) != exp.LossMarkerFieldCount {
		t.Fatalf("loss marker field count = %d, want %d (REL-105): %s", len(fields), exp.LossMarkerFieldCount, wireM)
	}
	gotFields := make([]string, 0, len(fields))
	for k := range fields {
		gotFields = append(gotFields, k)
	}
	sort.Strings(gotFields)
	wantFields := append([]string(nil), exp.LossMarkerFields...)
	sort.Strings(wantFields)
	if !reflect.DeepEqual(gotFields, wantFields) {
		t.Fatalf("loss marker fields = %v, want %v (REL-100/105)", gotFields, wantFields)
	}

	// Push then ack: the automation.run survivor rode the push, and the ack
	// (highest survivor seq + the delivered marker's range) drains the buffer.
	pending := buf.Pending()
	highest := pending[len(pending)-1].Seq
	sawAutomationRun := false
	for _, e := range pending {
		if e.Schema == SchemaAutomationRun {
			sawAutomationRun = true
		}
	}
	if !sawAutomationRun {
		t.Fatal("automation.run survivor missing from pending set — the emitter's durable entry must ride the push")
	}
	up := &scriptedUpstream{responses: []upstreamResponse{{ack: Ack{
		AckThroughSeq:    highest,
		LossMarkersAcked: []SeqRange{{FromSeq: m.FromSeq, ToSeq: m.ToSeq}},
	}}}}
	ch := NewChannel(buf, up, ExponentialBackoff{Base: time.Millisecond, MaxAttempts: 1})
	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	pushed := up.lastPush()
	if len(pushed.LossMarkers) != 1 {
		t.Fatalf("push loss_markers = %d, want 1 (REL-102)", len(pushed.LossMarkers))
	}
	if len(buf.Pending()) != 0 || len(buf.PendingLossMarkers()) != 0 {
		t.Fatalf("after ack: pending=%d markers=%d, want 0/0 (REL-092)", len(buf.Pending()), len(buf.PendingLossMarkers()))
	}
}

// TestCorpusREL094HeartbeatSupersededDurableSurvives replays REL-094 end-to-end:
// an older heartbeat, a durable automation.run (emitted through the emitter), then
// a newer heartbeat for the same device. The older heartbeat is superseded before
// any push (REL-094) — never pushed, never a loss marker (REL-104) — while the
// durable automation.run and the newer heartbeat both ride the push; the app peer's
// final view of the device is the newer payload (the corpus's own expected fields).
func TestCorpusREL094HeartbeatSupersededDurableSurvives(t *testing.T) {
	older, newer, finalView := rel094Heartbeats(t)
	exp := loadREL094Expected(t)
	if exp.Seq2001Pushed {
		t.Fatal("corpus self-check: REL-094 expects seq_2001_pushed:false")
	}
	if exp.LossMarkerGenerated {
		t.Fatal("corpus self-check: REL-094 expects loss_marker_generated:false")
	}
	if exp.DroppedCountsBySchemaIncludesDeviceHeartbeat {
		t.Fatal("corpus self-check: REL-094 expects dropped_counts_by_schema_includes_device_heartbeat:false")
	}
	subj := deviceID(t, newer)

	buf := NewBuffer(500)
	buf.Record(SchemaDeviceHeartbeat, older, subj, 0) // superseded before push

	ranC := loadEvtRunCase(t, evt040Corpus)
	arSchema, arPayload, arSubject := AutomationRunEntry(dispositionFrom(ranC), 0)
	buf.Record(arSchema, arPayload, arSubject, 0) // durable — always survives

	buf.Record(SchemaDeviceHeartbeat, newer, subj, 0) // supersedes the older

	up := &scriptedUpstream{responses: []upstreamResponse{{ack: Ack{AckThroughSeq: buf.Pending()[len(buf.Pending())-1].Seq}}}}
	ch := NewChannel(buf, up, ExponentialBackoff{Base: time.Millisecond, MaxAttempts: 1})
	if _, err := ch.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	pushed := up.lastPush()
	// A superseded latest-only entry is never loss (REL-094/104).
	if pushed.LossMarkers == nil {
		t.Fatal("loss_markers nil; REL-090 requires it present")
	}
	if len(pushed.LossMarkers) != 0 {
		t.Fatalf("loss_markers = %d, want 0 (superseded heartbeat is not loss, REL-094/104)", len(pushed.LossMarkers))
	}

	var heartbeats, automationRuns int
	var finalHeartbeat json.RawMessage
	for _, e := range pushed.Entries {
		switch e.Schema {
		case SchemaDeviceHeartbeat:
			heartbeats++
			finalHeartbeat = e.Payload
		case SchemaAutomationRun:
			automationRuns++
		}
	}
	if heartbeats != 1 {
		t.Fatalf("pushed %d device.heartbeat entries, want 1 (only the newer survives, REL-094)", heartbeats)
	}
	if automationRuns != 1 {
		t.Fatalf("pushed %d automation.run entries, want 1 (durable survives supersession, REL-093)", automationRuns)
	}
	// The app peer's final view of the device is the newer payload.
	if !jsonTreeEqual(t, finalHeartbeat, finalView) {
		t.Fatalf("final heartbeat payload = %s, want the newer view %s", finalHeartbeat, finalView)
	}
	if !jsonTreeEqual(t, finalView, exp.AppPeerFinalViewOfDevice) {
		t.Fatalf("corpus final view %s != app_peer_final_view_of_device %s", finalView, exp.AppPeerFinalViewOfDevice)
	}
}

// jsonTreeEqual compares two JSON documents as generic trees (order/whitespace
// insensitive). For the heartbeat comparison the app-peer final view is a subset
// of the entry payload's fields, so this asserts every field of `want` is present
// and equal in `got` — the newer entry reports at least everything the view names.
func jsonTreeEqual(t *testing.T, got, want json.RawMessage) bool {
	t.Helper()
	var gt, wt any
	if err := json.Unmarshal(got, &gt); err != nil {
		t.Fatalf("decode got %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &wt); err != nil {
		t.Fatalf("decode want %s: %v", want, err)
	}
	wm, wok := wt.(map[string]any)
	gm, gok := gt.(map[string]any)
	if wok && gok {
		for k, wv := range wm {
			if !reflect.DeepEqual(gm[k], wv) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(gt, wt)
}
