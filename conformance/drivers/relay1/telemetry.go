package relay1

import (
	"encoding/json"
	"fmt"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"reflect"
	"sort"
	"time"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
)

// This file drives REL-090 (bounded durable buffer overflow → drop-oldest →
// exact loss-marker accounting) and REL-094 (latest-only device.heartbeat
// supersession) directly against the committed internal/relay/telemetry
// package — Buffer, Channel, and the schema Class table — rather than
// re-implementing any part of the retention/overflow/supersession logic
// those own.

// fixedAckUpstream is a telemetry.Upstream fake that records the single
// batch it is asked to Push and answers with a pre-scripted Ack — the
// driver's stand-in for the live app peer's telemetry.ack response.
type fixedAckUpstream struct {
	ack    telemetry.Ack
	pushed *telemetry.PushBatch
}

func (u *fixedAckUpstream) Push(b telemetry.PushBatch) (telemetry.Ack, error) {
	cp := b
	u.pushed = &cp
	return u.ack, nil
}

// rel090Input decodes exactly the REL-090 corpus fields this stage reads:
// the bounded buffer's capacity, the durable-class drop-oldest overflow this
// scenario stages (a {from_seq,to_seq} range and its per-schema breakdown),
// and the telemetry.push the corpus illustrates the surviving entries with.
type rel090Input struct {
	BufferCapacity                          int `json:"buffer_capacity"`
	DurableEntriesRecordedWhileDisconnected int `json:"durable_entries_recorded_while_disconnected"`
	DroppedRange                            struct {
		FromSeq int64 `json:"from_seq"`
		ToSeq   int64 `json:"to_seq"`
	} `json:"dropped_range"`
	DroppedBreakdown map[string]int `json:"dropped_breakdown"`
	TelemetryPush    struct {
		Body struct {
			Entries []telemetry.Entry `json:"entries"`
		} `json:"body"`
	} `json:"telemetry_push"`
}

// rel090Expected is the subset of REL-090's expected block this stage
// diffs the live Buffer/Channel behavior against.
type rel090Expected struct {
	// The two POSITIVE assertions below are pointers so that ABSENT and FALSE are
	// distinguishable.
	//
	// As plain bools they gated their own checks — `if exp.Flag && mismatch` —
	// and Go's zero value is false, so deleting one key from the frozen case
	// deleted the assertion with it and the driver still reported PASS. An
	// assertion that vanishes when its expectation is removed is not an
	// assertion; it is a switch the corpus can turn off silently.
	//
	// The inverted self-checks in this same struct (SilentLoss and friends) are
	// deliberately NOT pointers: they assert the corpus declares the value FALSE,
	// so absence and false mean the same thing and the check still fires.
	EntryTraceIDCarriedUnmodified       *bool    `json:"entry_trace_id_carried_unmodified"`
	LossMarkerFieldCount                int      `json:"loss_marker_field_count"`
	LossMarkerFields                    []string `json:"loss_marker_fields"`
	DroppedCountsSumMatchesDroppedRange *bool    `json:"dropped_counts_sum_matches_dropped_range"`
	SilentLoss                          bool     `json:"silent_loss"`
}

// declaredBool reads a corpus expectation that MUST be present.
//
// It returns the declared value plus a diff when the field is absent, so a case
// that stops declaring an expectation fails loudly instead of quietly dropping
// the check that consumed it.
func declaredBool(field string, v *bool) (bool, *report.Diff) {
	if v == nil {
		return false, &report.Diff{
			Field:    field,
			Expected: "<declared in the corpus expected block>",
			Actual:   "absent — the assertion this gates would have been skipped silently",
		}
	}
	return *v, nil
}

func decodeRel090Input(c corpus.Case) (rel090Input, error) {
	raw, err := json.Marshal(c.Input)
	if err != nil {
		return rel090Input{}, fmt.Errorf("marshal corpus input: %w", err)
	}
	var in rel090Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return rel090Input{}, fmt.Errorf("unmarshal corpus input: %w", err)
	}
	return in, nil
}

func decodeRel090Expected(c corpus.Case) (rel090Expected, error) {
	raw, err := json.Marshal(c.Expected)
	if err != nil {
		return rel090Expected{}, fmt.Errorf("marshal corpus expected: %w", err)
	}
	var exp rel090Expected
	if err := json.Unmarshal(raw, &exp); err != nil {
		return rel090Expected{}, fmt.Errorf("unmarshal corpus expected: %w", err)
	}
	return exp, nil
}

// driveREL090 drives the bounded durable buffer's drop-oldest overflow
// (REL-096) and its exact loss-marker accounting (REL-090/100/103/105)
// directly against a real telemetry.Buffer: the corpus's dropped_breakdown
// durable-class victims are recorded first (so they hold the lowest seqs
// drop-oldest evicts), then the corpus's own telemetry.push survivor
// entries plus enough filler durable entries to reach buffer_capacity
// overflow them out — reproducing exactly the {capacity, dropped, total}
// arithmetic the corpus's own fields name. The resulting loss marker's
// shape, per-schema counts, and no-silent-loss sum are diffed against the
// corpus's expected block, and a push+ack cycle through a real
// telemetry.Channel proves the buffer drains (REL-092) carrying the
// corpus's own survivor entries unmodified.
func driveREL090(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-090")
	if !ok {
		rep.Fail("REL-090", contract, "case not found in frozen corpus")
		return
	}

	in, err := decodeRel090Input(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus input: %v", err))
		return
	}
	exp, err := decodeRel090Expected(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus expected: %v", err))
		return
	}

	var diffs []report.Diff

	dropWidth := int(in.DroppedRange.ToSeq-in.DroppedRange.FromSeq) + 1
	dropCount := sumCounts(in.DroppedBreakdown)
	if dropWidth != dropCount {
		diffs = append(diffs, report.Diff{Field: "corpus self-check: dropped_range width vs dropped_breakdown sum", Expected: dropWidth, Actual: dropCount})
	}
	if got := in.BufferCapacity + dropCount; got != in.DurableEntriesRecordedWhileDisconnected {
		diffs = append(diffs, report.Diff{Field: "corpus self-check: buffer_capacity + dropped total vs durable_entries_recorded_while_disconnected", Expected: in.DurableEntriesRecordedWhileDisconnected, Actual: got})
	}

	buf := telemetry.NewBuffer(in.BufferCapacity)

	// Victims: recorded first (lowest seqs), in a deterministic
	// (sorted-schema) order — their exact content is immaterial to REL-090's
	// accounting, only their count per schema (the corpus's dropped_breakdown).
	var schemas []string
	for schema := range in.DroppedBreakdown {
		schemas = append(schemas, schema)
	}
	sort.Strings(schemas)
	for _, schema := range schemas {
		for i := 0; i < in.DroppedBreakdown[schema]; i++ {
			buf.Record(schema, json.RawMessage(`{}`), "", 0)
		}
	}

	// Survivors: the corpus's own telemetry.push entries — real entries
	// flowing through the real Buffer/Channel unmodified — plus enough
	// filler durable entries to reach capacity, so drop-oldest evicts
	// exactly the victims recorded above.
	for _, e := range in.TelemetryPush.Body.Entries {
		// RecordTraced, not Record: the corpus's survivor entries carry the
		// per-entry trace_id REL-090a puts on the wire, and this stage asserts
		// below that the value rides the buffer and the push unmodified.
		buf.RecordTraced(e.Schema, e.Payload, "", 0, e.TraceID)
	}
	fillerCount := in.BufferCapacity - len(in.TelemetryPush.Body.Entries)
	for i := 0; i < fillerCount; i++ {
		buf.Record(telemetry.SchemaContentPlayed, json.RawMessage(`{"asset_ref":"sha256:0000000000000000000000000000000000000000000000000000000000000000","screen_id":"01J8Z3K4N5P6Q7R8S9T0V1W2X6"}`), "", 0)
	}

	markers := buf.PendingLossMarkers()
	if len(markers) != 1 {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("expected exactly 1 loss marker after overflow, got %d", len(markers)), diffs...)
		return
	}
	m := markers[0]

	// dropped_counts_by_schema (REL-104): exactly the corpus breakdown, no more.
	for schema, want := range in.DroppedBreakdown {
		if got := m.DroppedCountsBySchema[schema]; got != want {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("loss_marker.dropped_counts_by_schema[%s]", schema), Expected: want, Actual: got})
		}
	}
	if len(m.DroppedCountsBySchema) != len(in.DroppedBreakdown) {
		diffs = append(diffs, report.Diff{Field: "loss_marker.dropped_counts_by_schema (extra schemas)", Expected: in.DroppedBreakdown, Actual: m.DroppedCountsBySchema})
	}
	if m.Reason != telemetry.ReasonBufferExceeded {
		diffs = append(diffs, report.Diff{Field: "loss_marker.reason (REL-101)", Expected: telemetry.ReasonBufferExceeded, Actual: m.Reason})
	}

	sum := 0
	for _, n := range m.DroppedCountsBySchema {
		sum += n
	}
	rangeWidth := int(m.ToSeq-m.FromSeq) + 1
	if exp.SilentLoss {
		diffs = append(diffs, report.Diff{Field: "silent_loss (corpus self-check)", Expected: false, Actual: true})
	}
	if want, missing := declaredBool("dropped_counts_sum_matches_dropped_range", exp.DroppedCountsSumMatchesDroppedRange); missing != nil {
		diffs = append(diffs, *missing)
	} else if want != (sum == rangeWidth) {
		// Asserted in BOTH directions: a case declaring false and a run where the
		// sums happen to match is just as much a divergence as the reverse.
		diffs = append(diffs, report.Diff{Field: "dropped_counts_sum_matches_dropped_range (REL-103, no silent loss)", Expected: rangeWidth, Actual: sum})
	}

	// The loss marker's own field shape: exactly the REL-100/105 fields
	// (events/1 EVT-144's named exception), no more.
	wireM, err := json.Marshal(m)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal loss marker: %v", err), diffs...)
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wireM, &fields); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode loss marker fields: %v", err), diffs...)
		return
	}
	if len(fields) != exp.LossMarkerFieldCount {
		diffs = append(diffs, report.Diff{Field: "loss_marker_field_count (REL-105)", Expected: exp.LossMarkerFieldCount, Actual: len(fields)})
	}
	gotFields := make([]string, 0, len(fields))
	for k := range fields {
		gotFields = append(gotFields, k)
	}
	sort.Strings(gotFields)
	wantFields := append([]string(nil), exp.LossMarkerFields...)
	sort.Strings(wantFields)
	if !reflect.DeepEqual(gotFields, wantFields) {
		diffs = append(diffs, report.Diff{Field: "loss_marker_fields (REL-100/105, events/1 EVT-144 named exception)", Expected: wantFields, Actual: gotFields})
	}

	// Push + ack drains the buffer (REL-092), carrying the corpus's own
	// survivor entries unmodified.
	pending := buf.Pending()
	if len(pending) == 0 {
		rep.Fail(c.CaseID, contract, "no pending entries survived to push", diffs...)
		return
	}
	highest := pending[len(pending)-1].Seq
	up := &fixedAckUpstream{ack: telemetry.Ack{
		AckThroughSeq:    highest,
		LossMarkersAcked: []telemetry.SeqRange{{FromSeq: m.FromSeq, ToSeq: m.ToSeq}},
	}}
	ch := telemetry.NewChannel(buf, up, telemetry.ExponentialBackoff{Base: time.Millisecond, MaxAttempts: 1})
	ack, err := ch.Flush()
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("Flush: %v", err), diffs...)
		return
	}

	if up.pushed == nil {
		diffs = append(diffs, report.Diff{Field: "telemetry.push delivered", Expected: "one push", Actual: "none"})
	} else {
		if up.pushed.LossMarkers == nil {
			diffs = append(diffs, report.Diff{Field: "telemetry.push.loss_markers present (REL-090)", Expected: "non-nil", Actual: "nil"})
		} else if len(up.pushed.LossMarkers) != 1 {
			diffs = append(diffs, report.Diff{Field: "telemetry.push.loss_markers count", Expected: 1, Actual: len(up.pushed.LossMarkers)})
		}
		for _, want := range in.TelemetryPush.Body.Entries {
			var match *telemetry.Entry
			for i, got := range up.pushed.Entries {
				if got.Schema == want.Schema && string(got.Payload) == string(want.Payload) {
					match = &up.pushed.Entries[i]
					break
				}
			}
			if match == nil {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("telemetry.push.entries carries corpus survivor (schema=%s)", want.Schema), Expected: string(want.Payload), Actual: "not found in pushed entries"})
				continue
			}
			// REL-090a: the entry's own trace_id reaches the app peer
			// unmodified. It is asserted per ENTRY rather than per push: the two
			// survivors carry DIFFERENT trace ids, so a batch-level value — or a
			// value the buffer minted over the top of the recorded one — fails
			// here rather than passing on a single-entry coincidence.
			if carried, missing := declaredBool("entry_trace_id_carried_unmodified", exp.EntryTraceIDCarriedUnmodified); missing != nil {
				diffs = append(diffs, *missing)
			} else if carried != (match.TraceID == want.TraceID) {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("telemetry.push.entries[schema=%s].trace_id (REL-090a)", want.Schema), Expected: want.TraceID, Actual: match.TraceID})
			}
		}
	}
	if ack.AckThroughSeq != highest {
		diffs = append(diffs, report.Diff{Field: "telemetry.ack.body.ack_through_seq", Expected: highest, Actual: ack.AckThroughSeq})
	}
	if len(buf.Pending()) != 0 || len(buf.PendingLossMarkers()) != 0 {
		diffs = append(diffs, report.Diff{Field: "post-ack drain (REL-092)", Expected: "0 pending / 0 markers", Actual: fmt.Sprintf("%d pending / %d markers", len(buf.Pending()), len(buf.PendingLossMarkers()))})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "telemetry overflow / loss-marker diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"seq 980-999/1001-1002 and the ack's static ack_through_seq=1002 are the corpus's own illustrative fixture — this stage's Buffer assigns its own seqs from 1 (REL-091), so the marker's from_seq/to_seq and the delivered ack_through_seq asserted here are the LIVE Buffer's own values, not byte-equal to the corpus's illustrative ones; the counted/shape/self-check properties above are what REL-090/096/100/103/105 actually require.",
	)
}

// sumCounts totals a dropped_counts_by_schema-shaped map.
func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// rel094Input decodes exactly the REL-094 corpus fields this stage reads:
// the two same-device device.heartbeat entries staged while disconnected
// (older then newer) and the telemetry.push the corpus illustrates the
// surviving (newer-only) entry with.
type rel094Input struct {
	BufferedEntriesSameDevice []telemetry.Entry `json:"buffered_entries_same_device"`
	TelemetryPush             struct {
		Body struct {
			Entries []telemetry.Entry `json:"entries"`
		} `json:"body"`
	} `json:"telemetry_push"`
}

// rel094Expected is the subset of REL-094's expected block this stage
// diffs the live Buffer/Channel behavior against.
type rel094Expected struct {
	Seq2001Discarded                             bool            `json:"seq_2001_discarded"`
	Seq2001Pushed                                bool            `json:"seq_2001_pushed"`
	LossMarkerGenerated                          bool            `json:"loss_marker_generated"`
	DroppedCountsBySchemaIncludesDeviceHeartbeat bool            `json:"dropped_counts_by_schema_includes_device_heartbeat"`
	AppPeerFinalViewOfDevice                     json.RawMessage `json:"app_peer_final_view_of_device"`
}

func decodeRel094Input(c corpus.Case) (rel094Input, error) {
	raw, err := json.Marshal(c.Input)
	if err != nil {
		return rel094Input{}, fmt.Errorf("marshal corpus input: %w", err)
	}
	var in rel094Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return rel094Input{}, fmt.Errorf("unmarshal corpus input: %w", err)
	}
	return in, nil
}

func decodeRel094Expected(c corpus.Case) (rel094Expected, error) {
	raw, err := json.Marshal(c.Expected)
	if err != nil {
		return rel094Expected{}, fmt.Errorf("marshal corpus expected: %w", err)
	}
	var exp rel094Expected
	if err := json.Unmarshal(raw, &exp); err != nil {
		return rel094Expected{}, fmt.Errorf("unmarshal corpus expected: %w", err)
	}
	return exp, nil
}

// driveREL094 drives the latest-only device.heartbeat supersession
// (REL-094/104) directly against a real telemetry.Buffer: the corpus's
// older same-device heartbeat is recorded first, then the newer one — the
// Buffer's own ClassOf/supersede logic must discard the older before any
// push, generating no loss marker, and the newer entry alone rides a push
// through a real telemetry.Channel as the app peer's final view of the
// device.
func driveREL094(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-094")
	if !ok {
		rep.Fail("REL-094", contract, "case not found in frozen corpus")
		return
	}

	in, err := decodeRel094Input(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus input: %v", err))
		return
	}
	exp, err := decodeRel094Expected(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus expected: %v", err))
		return
	}
	if len(in.BufferedEntriesSameDevice) != 2 {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("corpus buffered_entries_same_device has %d entries, want 2", len(in.BufferedEntriesSameDevice)))
		return
	}
	older, newer := in.BufferedEntriesSameDevice[0], in.BufferedEntriesSameDevice[1]

	var diffs []report.Diff

	if !exp.Seq2001Discarded {
		diffs = append(diffs, report.Diff{Field: "seq_2001_discarded (corpus self-check)", Expected: true, Actual: false})
	}
	if exp.Seq2001Pushed {
		diffs = append(diffs, report.Diff{Field: "seq_2001_pushed (corpus self-check)", Expected: false, Actual: true})
	}
	if exp.LossMarkerGenerated {
		diffs = append(diffs, report.Diff{Field: "loss_marker_generated (corpus self-check)", Expected: false, Actual: true})
	}
	if exp.DroppedCountsBySchemaIncludesDeviceHeartbeat {
		diffs = append(diffs, report.Diff{Field: "dropped_counts_by_schema_includes_device_heartbeat (corpus self-check)", Expected: false, Actual: true})
	}

	subject := subjectDeviceID(older.Payload)
	if subject == "" || subject != subjectDeviceID(newer.Payload) {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("corpus older/newer heartbeat payloads must share device_id (older=%q, newer=%q)", subject, subjectDeviceID(newer.Payload)), diffs...)
		return
	}

	buf := telemetry.NewBuffer(500)
	buf.Record(older.Schema, older.Payload, subject, 0)
	buf.Record(newer.Schema, newer.Payload, subject, 0)

	pending := buf.Pending()
	if len(pending) != 1 {
		diffs = append(diffs, report.Diff{Field: "buffered entries after supersession (REL-094)", Expected: 1, Actual: len(pending)})
	} else if string(pending[0].Payload) != string(newer.Payload) {
		diffs = append(diffs, report.Diff{Field: "surviving buffered entry payload (older superseded before push)", Expected: string(newer.Payload), Actual: string(pending[0].Payload)})
	}
	if markers := buf.PendingLossMarkers(); len(markers) != 0 {
		diffs = append(diffs, report.Diff{Field: "loss markers generated by a latest-only supersession (REL-104)", Expected: 0, Actual: len(markers)})
	}
	if len(pending) == 0 {
		rep.Fail(c.CaseID, contract, "no entry survived supersession to push", diffs...)
		return
	}

	up := &fixedAckUpstream{ack: telemetry.Ack{AckThroughSeq: pending[len(pending)-1].Seq}}
	ch := telemetry.NewChannel(buf, up, telemetry.ExponentialBackoff{Base: time.Millisecond, MaxAttempts: 1})
	if _, err := ch.Flush(); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("Flush: %v", err), diffs...)
		return
	}

	if up.pushed == nil {
		diffs = append(diffs, report.Diff{Field: "telemetry.push delivered", Expected: "one push", Actual: "none"})
	} else {
		if up.pushed.LossMarkers == nil {
			diffs = append(diffs, report.Diff{Field: "telemetry.push.loss_markers present (REL-090)", Expected: "non-nil", Actual: "nil"})
		} else if len(up.pushed.LossMarkers) != 0 {
			diffs = append(diffs, report.Diff{Field: "telemetry.push.loss_markers count (REL-104)", Expected: 0, Actual: len(up.pushed.LossMarkers)})
		}

		var heartbeats int
		var finalPayload json.RawMessage
		for _, e := range up.pushed.Entries {
			if string(e.Payload) == string(older.Payload) {
				diffs = append(diffs, report.Diff{Field: "seq_2001_pushed (REL-094)", Expected: false, Actual: true})
			}
			if e.Schema == telemetry.SchemaDeviceHeartbeat {
				heartbeats++
				finalPayload = e.Payload
			}
		}
		if heartbeats != 1 {
			diffs = append(diffs, report.Diff{Field: "pushed device.heartbeat count (REL-094, only newer survives)", Expected: 1, Actual: heartbeats})
		} else if !jsonSubsetEqual(finalPayload, exp.AppPeerFinalViewOfDevice) {
			diffs = append(diffs, report.Diff{Field: "app_peer_final_view_of_device", Expected: string(exp.AppPeerFinalViewOfDevice), Actual: string(finalPayload)})
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "telemetry latest-only heartbeat supersession diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"seq 2001/2002 are the corpus's own illustrative fixture numbering; this stage's Buffer assigns its own seqs from 1 (REL-091) — the discard/never-pushed/no-loss-marker properties above are asserted structurally, not by matching the corpus's literal seq values.",
	)
}

// subjectDeviceID extracts the device_id field from a device.heartbeat (or
// box.vitals) payload — the REL-094 supersession subject key — without
// otherwise interpreting the payload (REL-095: this channel carries a
// schema's fields unmodified and does not redefine them).
func subjectDeviceID(payload json.RawMessage) string {
	var v struct {
		DeviceID string `json:"device_id"`
	}
	_ = json.Unmarshal(payload, &v)
	return v.DeviceID
}

// jsonSubsetEqual reports whether every field want names is present and
// equal in got — the app-peer's final view of a device (a subset of the
// delivered heartbeat's own fields) compared against the delivered payload.
func jsonSubsetEqual(got, want json.RawMessage) bool {
	var gt, wt any
	if err := json.Unmarshal(got, &gt); err != nil {
		return false
	}
	if err := json.Unmarshal(want, &wt); err != nil {
		return false
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

// rel090aInput / rel090aExpected decode the REL-090a case's own blocks.
type rel090aInput struct {
	BufferCapacity int `json:"buffer_capacity"`
	TelemetryPush  struct {
		Body struct {
			Entries []struct {
				Seq     int64           `json:"seq"`
				Schema  string          `json:"schema"`
				Payload json.RawMessage `json:"payload"`
				TraceID string          `json:"trace_id"`
			} `json:"entries"`
		} `json:"body"`
	} `json:"telemetry_push"`
}

type rel090aExpected struct {
	EntriesDelivered *int  `json:"entries_delivered"`
	EntriesDropped   *int  `json:"entries_dropped"`
	SilentLoss       *bool `json:"silent_loss"`
}

// driveREL090a exercises REL-090a's SECOND must, which nothing exercised before.
//
// The requirement: "An entry omitting `trace_id`, or carrying a value that is
// not a Canonical ULID, MUST still be delivered … and MUST NOT reject or drop
// the entry — a defect in correlation metadata may not become the durable-class
// loss REL-093 and REL-103 forbid."
//
// The traceability row read `covered` against REL-090's case, and that case's
// two entries BOTH carry well-formed trace ids. So a relay that refused an entry
// with a missing or malformed trace_id — discarding telemetry it is obliged to
// deliver, silently — passed the corpus. This case is the one that fails it.
//
// It asserts DELIVERY, not correlation: the trace_id an entry ends up carrying
// is REL-090a's first must and the app peer's obligation (API-063 exercises it).
// What is checked here is that none of the three entries went missing, whatever
// their metadata looked like.
func driveREL090a(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-090a")
	if !ok {
		rep.Fail("REL-090a", contract, "case not found in frozen corpus")
		return
	}
	raw, err := json.Marshal(c.Input)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal corpus input: %v", err))
		return
	}
	var in rel090aInput
	if err := json.Unmarshal(raw, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus input: %v", err))
		return
	}
	rawExp, err := json.Marshal(c.Expected)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal corpus expected: %v", err))
		return
	}
	var exp rel090aExpected
	if err := json.Unmarshal(rawExp, &exp); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus expected: %v", err))
		return
	}

	var diffs []report.Diff
	buf := telemetry.NewBuffer(in.BufferCapacity)
	// Recorded exactly as the corpus spells them, malformed values included. A
	// driver that sanitised here would be testing its own fixture rather than
	// the buffer's willingness to carry what it was given.
	for _, e := range in.TelemetryPush.Body.Entries {
		buf.RecordTraced(e.Schema, e.Payload, "", 0, e.TraceID)
	}

	pending := buf.Pending()
	up := &fixedAckUpstream{}
	if len(pending) > 0 {
		up.ack = telemetry.Ack{AckThroughSeq: pending[len(pending)-1].Seq}
	}
	ch := telemetry.NewChannel(buf, up, telemetry.ExponentialBackoff{Base: time.Millisecond, MaxAttempts: 1})
	if _, err := ch.Flush(); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("Flush: %v", err))
		return
	}
	if up.pushed == nil {
		rep.Fail(c.CaseID, contract, "no telemetry.push was delivered at all")
		return
	}

	if delivered, missing := declaredInt("entries_delivered", exp.EntriesDelivered); missing != nil {
		diffs = append(diffs, *missing)
	} else if len(up.pushed.Entries) != delivered {
		diffs = append(diffs, report.Diff{
			Field:    "telemetry.push.entries delivered (REL-090a second MUST)",
			Expected: delivered, Actual: len(up.pushed.Entries)})
	}

	// Named individually, because a count alone cannot say WHICH shape was
	// dropped — and the two shapes fail for different reasons.
	for _, want := range in.TelemetryPush.Body.Entries {
		found := false
		for _, got := range up.pushed.Entries {
			if got.Schema == want.Schema && string(got.Payload) == string(want.Payload) {
				found = true
				break
			}
		}
		if !found {
			shape := "a well-formed trace_id"
			switch {
			case want.TraceID == "":
				shape = "NO trace_id"
			case !ulid.Valid(want.TraceID):
				shape = "a malformed trace_id"
			}
			diffs = append(diffs, report.Diff{
				Field:    fmt.Sprintf("telemetry.push.entries carries the entry with %s (schema=%s)", shape, want.Schema),
				Expected: "delivered", Actual: "dropped — a metadata defect became durable-class loss (REL-093/REL-103)"})
		}
	}

	if dropped, missing := declaredInt("entries_dropped", exp.EntriesDropped); missing != nil {
		diffs = append(diffs, *missing)
	} else if got := len(in.TelemetryPush.Body.Entries) - len(up.pushed.Entries); got != dropped {
		diffs = append(diffs, report.Diff{Field: "entries dropped", Expected: dropped, Actual: got})
	}
	if silent, missing := declaredBool("silent_loss", exp.SilentLoss); missing != nil {
		diffs = append(diffs, *missing)
	} else if got := len(up.pushed.LossMarkers) > 0; got != silent {
		// `silent_loss: false` means nothing was lost, so no marker is owed. A
		// marker appearing here would mean the buffer DID drop something while
		// the entry count still reconciled — which is the shape this case exists
		// to refuse.
		diffs = append(diffs, report.Diff{Field: "loss markers accompany a lossless push", Expected: silent, Actual: got})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "an entry whose trace_id was absent or malformed was not delivered", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// declaredInt is declaredBool's sibling: an assertion whose expected value the
// corpus omitted is a check that would pass by not running.
func declaredInt(field string, v *int) (int, *report.Diff) {
	if v == nil {
		return 0, &report.Diff{
			Field:    field,
			Expected: "<declared in the corpus expected block>",
			Actual:   "absent — the assertion this gates would have been skipped silently",
		}
	}
	return *v, nil
}
