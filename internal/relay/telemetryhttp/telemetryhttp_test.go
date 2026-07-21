package telemetryhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
)

// TestPushPostsBatchAndReturnsAck is the REL-090/092 transport oracle: the
// Upstream POSTs the telemetry.push {entries, loss_markers} body the Channel
// built to <appBaseURL>/telemetry/v1/push and returns the app peer's parsed
// telemetry.ack {ack_through_seq, loss_markers_acked}.
func TestPushPostsBatchAndReturnsAck(t *testing.T) {
	var gotPath, gotMethod, gotContentType string
	var gotBatch telemetry.PushBatch
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotContentType = r.URL.Path, r.Method, r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBatch); err != nil {
			t.Errorf("server: decode pushed batch: %v", err)
		}
		_ = json.NewEncoder(w).Encode(telemetry.Ack{
			AckThroughSeq:    2,
			LossMarkersAcked: []telemetry.SeqRange{{FromSeq: 5, ToSeq: 9}},
		})
	}))
	defer srv.Close()

	up := New(srv.URL, srv.Client())
	batch := telemetry.PushBatch{
		Entries: []telemetry.Entry{
			{Seq: 1, Schema: telemetry.SchemaAutomationRun, Payload: json.RawMessage(`{"rule_id":"01J8Z3K4N5RULEA"}`)},
			{Seq: 2, Schema: telemetry.SchemaAutomationRun, Payload: json.RawMessage(`{"rule_id":"01J8Z3K4N5RULEB"}`)},
		},
		LossMarkers: []telemetry.LossMarker{},
	}

	ack, err := up.Push(batch)
	if err != nil {
		t.Fatalf("Push: unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("request method = %q, want POST", gotMethod)
	}
	if gotPath != "/telemetry/v1/push" {
		t.Errorf("request path = %q, want /telemetry/v1/push", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("request Content-Type = %q, want application/json", gotContentType)
	}
	if len(gotBatch.Entries) != 2 || gotBatch.Entries[0].Seq != 1 || gotBatch.Entries[1].Seq != 2 {
		t.Errorf("app peer received entries = %+v, want seqs [1 2]", gotBatch.Entries)
	}
	if ack.AckThroughSeq != 2 {
		t.Errorf("ack.AckThroughSeq = %d, want 2", ack.AckThroughSeq)
	}
	if len(ack.LossMarkersAcked) != 1 || ack.LossMarkersAcked[0] != (telemetry.SeqRange{FromSeq: 5, ToSeq: 9}) {
		t.Errorf("ack.LossMarkersAcked = %+v, want [{5 9}]", ack.LossMarkersAcked)
	}
}

// TestPushNon2xxReturnsErrorNoAck proves a non-2xx response is not an ack: Push
// returns an error and a zero Ack, so the Channel leaves the batch buffered and
// retention is NOT advanced (REL-097 — an unacknowledged batch is retained).
func TestPushNon2xxReturnsErrorNoAck(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "telemetry ingest unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	up := New(srv.URL, srv.Client())
	ack, err := up.Push(telemetry.PushBatch{
		Entries:     []telemetry.Entry{{Seq: 1, Schema: telemetry.SchemaAutomationRun, Payload: json.RawMessage(`{}`)}},
		LossMarkers: []telemetry.LossMarker{},
	})
	if err == nil {
		t.Fatal("Push: want an error on a non-2xx status, got nil")
	}
	if ack.AckThroughSeq != 0 || len(ack.LossMarkersAcked) != 0 {
		t.Errorf("Push: want a zero Ack on error, got %+v", ack)
	}
}

// TestPushTransportErrorReturnsError proves a transport failure (the app peer
// unreachable) returns an error and no ack — again leaving the batch retained.
func TestPushTransportErrorReturnsError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := srv.Client()
	srv.Close() // the app peer is now unreachable

	up := New(srv.URL, client)
	if _, err := up.Push(telemetry.PushBatch{
		Entries:     []telemetry.Entry{{Seq: 1, Schema: telemetry.SchemaAutomationRun, Payload: json.RawMessage(`{}`)}},
		LossMarkers: []telemetry.LossMarker{},
	}); err == nil {
		t.Fatal("Push: want a transport error against a closed app peer, got nil")
	}
}

// TestChannelFlushOverTransportAdvancesRetentionOnAck wires a real
// telemetry.Channel over this concrete transport: a Flush pushes the buffered
// automation.run entries and, on the received ack_through_seq, advances
// retention — the acked entries are discarded (REL-092).
func TestChannelFlushOverTransportAdvancesRetentionOnAck(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch telemetry.PushBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("server: decode batch: %v", err)
		}
		_ = json.NewEncoder(w).Encode(telemetry.Ack{AckThroughSeq: highestSeq(batch)})
	}))
	defer srv.Close()

	buf := telemetry.NewBuffer(16)
	buf.Record(telemetry.SchemaAutomationRun, json.RawMessage(`{"rule_id":"01J8Z3K4N5RULEA"}`), "", 1)
	buf.Record(telemetry.SchemaAutomationRun, json.RawMessage(`{"rule_id":"01J8Z3K4N5RULEB"}`), "", 2)

	ch := telemetry.NewChannel(buf, New(srv.URL, srv.Client()), nil)
	ack, err := ch.Flush()
	if err != nil {
		t.Fatalf("Flush: unexpected error: %v", err)
	}
	if ack.AckThroughSeq != 2 {
		t.Errorf("Flush ack.AckThroughSeq = %d, want 2", ack.AckThroughSeq)
	}
	if pending := buf.Pending(); len(pending) != 0 {
		t.Errorf("after an ack through seq 2, buffer has %d pending entries, want 0 (retention advanced)", len(pending))
	}
}

// TestChannelFlushRetainsRecordsWhenPushFails proves the at-least-once spine:
// when the app peer rejects the push (non-2xx), the Flush errors and the buffer
// keeps every un-acked entry for a later retry (REL-097).
func TestChannelFlushRetainsRecordsWhenPushFails(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "app peer down", http.StatusBadGateway)
	}))
	defer srv.Close()

	buf := telemetry.NewBuffer(16)
	buf.Record(telemetry.SchemaAutomationRun, json.RawMessage(`{"rule_id":"01J8Z3K4N5RULEA"}`), "", 1)

	ch := telemetry.NewChannel(buf, New(srv.URL, srv.Client()), nil)
	if _, err := ch.Flush(); err == nil {
		t.Fatal("Flush: want an error when the app peer rejects the push, got nil")
	}
	if pending := buf.Pending(); len(pending) != 1 || pending[0].Seq != 1 {
		t.Errorf("after a failed push, buffer pending = %+v, want the un-acked entry (seq 1) retained", pending)
	}
}

func highestSeq(b telemetry.PushBatch) int64 {
	var through int64
	for _, e := range b.Entries {
		if e.Seq > through {
			through = e.Seq
		}
	}
	return through
}
