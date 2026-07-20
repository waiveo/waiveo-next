package events

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"testing"
)

// TestWebhookSignature_EVT151_ExactHexAgainstCorpus is the EVT-151 oracle:
// given the corpus's literal signed_material — <timestamp>.<raw JSON body>,
// where body is the exact compacted bytes of the fixture's "event" object —
// WebhookSignature must produce exactly the hex HMAC-SHA256 of that string
// keyed by the endpoint's own signing secret. Both the signed-material
// construction and the resulting hex are checked against an independent
// crypto/hmac+sha256 computation, so this asserts the EXACT corpus formula,
// not merely "produces some signature".
func TestWebhookSignature_EVT151_ExactHexAgainstCorpus(t *testing.T) {
	var c struct {
		Input struct {
			EndpointSigningSecret string          `json:"endpoint_signing_secret"`
			DeliveryID            string          `json:"delivery_id"`
			Timestamp             int64           `json:"timestamp"`
			Event                 json.RawMessage `json:"event"`
		} `json:"input"`
		Expected struct {
			Request struct {
				Method  string            `json:"method"`
				Headers map[string]string `json:"headers"`
			} `json:"request"`
			SignedMaterial string `json:"signed_material"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(corpusBytes(t, "EVT-151-valid-webhook-delivery-signed.json"), &c); err != nil {
		t.Fatalf("unmarshal EVT-151 corpus: %v", err)
	}

	// body is the exact raw JSON bytes signed over: json.Marshal of a
	// json.RawMessage compacts (strips insignificant whitespace) without
	// reordering keys, so this reproduces the corpus's literal body bytes.
	body, err := json.Marshal(c.Input.Event)
	if err != nil {
		t.Fatalf("marshal fixture event: %v", err)
	}
	timestamp := strconv.FormatInt(c.Input.Timestamp, 10)

	signedMaterial := timestamp + "." + string(body)
	if signedMaterial != c.Expected.SignedMaterial {
		t.Fatalf("signed_material = %q; want %q (corpus)", signedMaterial, c.Expected.SignedMaterial)
	}

	// Independent reference computation — the oracle's own formula, not a
	// call into the code under test.
	mac := hmac.New(sha256.New, []byte(c.Input.EndpointSigningSecret))
	mac.Write([]byte(signedMaterial))
	wantHex := hex.EncodeToString(mac.Sum(nil))
	if len(wantHex) != 64 {
		t.Fatalf("reference HMAC-SHA256 hex length = %d; want 64", len(wantHex))
	}

	got := WebhookSignature(c.Input.EndpointSigningSecret, timestamp, body)
	if got != wantHex {
		t.Fatalf("WebhookSignature = %q; want %q (EVT-151 exact hex HMAC-SHA256 vs corpus)", got, wantHex)
	}

	if want := c.Expected.Request.Headers[HeaderTimestamp]; want != timestamp {
		t.Fatalf("corpus X-Waiveo-Timestamp header = %q; want %q", want, timestamp)
	}
	if want := c.Expected.Request.Headers[HeaderDeliveryID]; want != c.Input.DeliveryID {
		t.Fatalf("corpus X-Waiveo-Delivery-Id header = %q; want %q", want, c.Input.DeliveryID)
	}
}

// TestBuildWebhookDelivery_Headers checks the three-header shape (EVT-151):
// X-Waiveo-Delivery-Id/X-Waiveo-Timestamp carry the caller's inputs verbatim,
// and X-Waiveo-Signature is exactly WebhookSignature over the request's own
// body — signing and header population can never disagree.
func TestBuildWebhookDelivery_Headers(t *testing.T) {
	e := Envelope{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2YB", Schema: SchemaAutomationRun}
	req, err := BuildWebhookDelivery("https://ops.example.invalid/hooks/waiveo",
		"01J8Z3K4N5P6Q7R8S9T0V1W2YF", "1752537603", "whsec_fixture_9f8e7d6c5b4a", e)
	if err != nil {
		t.Fatalf("BuildWebhookDelivery: %v", err)
	}
	if req.URL != "https://ops.example.invalid/hooks/waiveo" {
		t.Fatalf("URL = %q", req.URL)
	}
	if got := req.Headers[HeaderDeliveryID]; got != "01J8Z3K4N5P6Q7R8S9T0V1W2YF" {
		t.Fatalf("%s = %q; want the caller-supplied delivery id", HeaderDeliveryID, got)
	}
	if got := req.Headers[HeaderTimestamp]; got != "1752537603" {
		t.Fatalf("%s = %q; want the caller-supplied timestamp", HeaderTimestamp, got)
	}
	wantSig := WebhookSignature("whsec_fixture_9f8e7d6c5b4a", "1752537603", req.Body)
	if got := req.Headers[HeaderSignature]; got != wantSig {
		t.Fatalf("%s = %q; want %q (must equal WebhookSignature over the request's own body)", HeaderSignature, got, wantSig)
	}
}

// TestBuildWebhookDelivery_DeliveryIDStableAcrossRetries (EVT-156): two
// delivery attempts of the same logical delivery — the caller passing the
// SAME deliveryID both times, as its own retry loop must — produce the same
// X-Waiveo-Delivery-Id header, even though the timestamp (and therefore the
// signature) differs per attempt.
func TestBuildWebhookDelivery_DeliveryIDStableAcrossRetries(t *testing.T) {
	e := Envelope{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2YB", Schema: SchemaAutomationRun}
	const deliveryID = "01J8Z3K4N5P6Q7R8S9T0V1W2YF"
	secret := "whsec_fixture_9f8e7d6c5b4a"

	first, err := BuildWebhookDelivery("https://ops.example.invalid/hooks/waiveo", deliveryID, "1752537603", secret, e)
	if err != nil {
		t.Fatalf("BuildWebhookDelivery (attempt 1): %v", err)
	}
	retry, err := BuildWebhookDelivery("https://ops.example.invalid/hooks/waiveo", deliveryID, "1752537633", secret, e)
	if err != nil {
		t.Fatalf("BuildWebhookDelivery (attempt 2): %v", err)
	}
	if first.Headers[HeaderDeliveryID] != retry.Headers[HeaderDeliveryID] {
		t.Fatalf("delivery id changed across retries: %q vs %q (EVT-156)", first.Headers[HeaderDeliveryID], retry.Headers[HeaderDeliveryID])
	}
	if first.Headers[HeaderSignature] == retry.Headers[HeaderSignature] {
		t.Fatal("a differently-timestamped retry must not reuse the same signature")
	}
}

// TestEndpointState_DisablesAfterConsecutiveFailures (EVT-154): an endpoint
// stays active through fewer than the configured bound of consecutive
// failures, and transitions to disabled exactly upon reaching it.
func TestEndpointState_DisablesAfterConsecutiveFailures(t *testing.T) {
	st := NewEndpointState("whsec_fixture_disable_test", 3, DefaultBackoffBaseMs, DefaultBackoffCapMs, DefaultRotationOverlapMs)

	for i := 0; i < 2; i++ {
		st.RecordAttempt(false, int64(i)*1000)
		if st.Status != EndpointActive {
			t.Fatalf("status = %q after %d consecutive failures; want active (bound not yet reached)", st.Status, i+1)
		}
	}

	st.RecordAttempt(false, 3000)
	if st.Status != EndpointDisabled {
		t.Fatalf("status = %q after 3 consecutive failures; want disabled (EVT-154)", st.Status)
	}

	// A success resets the failure run but does NOT by itself reactivate a
	// disabled endpoint — only Enable does (EVT-154's explicit re-enable).
	st.RecordAttempt(true, 4000)
	if st.Status != EndpointDisabled {
		t.Fatalf("status = %q after a success while disabled; want to remain disabled until Enable (EVT-154)", st.Status)
	}
	if st.ConsecutiveFailures != 0 {
		t.Fatalf("ConsecutiveFailures = %d after a success; want reset to 0", st.ConsecutiveFailures)
	}
}

// TestEndpointState_ReEnableResumesFromLastDeliveredID (EVT-155): a disabled
// endpoint, once re-enabled, resumes from last_delivered_id under the EXACT
// same retention-window gap behavior a WS/SSE reconnect uses — this reuses
// resume.Resolve directly rather than reimplementing gap logic here.
func TestEndpointState_ReEnableResumesFromLastDeliveredID(t *testing.T) {
	var l EventLog
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y6"))
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y7"))
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y8"))

	st := NewEndpointState("whsec_fixture_resume_test", 1, DefaultBackoffBaseMs, DefaultBackoffCapMs, DefaultRotationOverlapMs)
	st.RecordDelivered("01J8Z3K4N5P6Q7R8S9T0V1W2Y6")
	st.RecordAttempt(false, 0) // one failure trips the bound of 1 -> disabled
	if st.Status != EndpointDisabled {
		t.Fatalf("status = %q; want disabled", st.Status)
	}

	st.Enable()
	if st.Status != EndpointActive {
		t.Fatalf("status = %q after Enable; want active", st.Status)
	}
	if st.LastDeliveredID != "01J8Z3K4N5P6Q7R8S9T0V1W2Y6" {
		t.Fatalf("LastDeliveredID = %q after Enable; want preserved across disable/enable (EVT-155)", st.LastDeliveredID)
	}

	// Resume via the identical mechanism a WS/SSE reconnect uses.
	out, rerr := Resolve(&l, st.LastDeliveredID)
	if rerr != nil {
		t.Fatalf("Resolve(last_delivered_id): %v", rerr)
	}
	if out.Result != ResumeResultResumed {
		t.Fatalf("resume_result = %q; want resumed", out.Result)
	}
	want := []string{"01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "01J8Z3K4N5P6Q7R8S9T0V1W2Y8"}
	if got := ids(out.Events); !stringSlicesEqual(got, want) {
		t.Fatalf("resumed backlog = %v; want %v", got, want)
	}

	// PendingAfter must agree with Resolve's own backlog.
	gotEvents, gotGap, gotErr := st.PendingAfter(&l)
	if gotErr != nil {
		t.Fatalf("PendingAfter rerr = %v; want nil (a still-retained LastDeliveredID resolves cleanly)", gotErr)
	}
	if got := ids(gotEvents); !stringSlicesEqual(got, want) {
		t.Fatalf("PendingAfter = %v; want %v (must match Resolve's backlog)", got, want)
	}
	if gotGap != nil {
		t.Fatalf("PendingAfter gap = %+v; want nil (a still-retained LastDeliveredID is a clean resume, no gap)", gotGap)
	}
}

// TestEndpointState_PendingAfter_AgedOutLastDeliveredID_Gap (EVT-143/155): a
// webhook endpoint is, from the durable-event log's perspective, just another
// subscriber — re-enabling/resuming one whose LastDeliveredID has aged past
// the log's retention window MUST get the EXACT SAME retention-window gap
// behavior (Loss markers) a WS/SSE reconnect gets from resume.Resolve, never
// a silently truncated backlog indistinguishable from an ordinary clean
// resume.
func TestEndpointState_PendingAfter_AgedOutLastDeliveredID_Gap(t *testing.T) {
	l := NewEventLog(2)
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y6")) // A
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y7")) // B
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y8")) // C (evicts A)
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y9")) // D (evicts B) -> retained [C, D]

	st := NewEndpointState("whsec_fixture_gap_test", 10, DefaultBackoffBaseMs, DefaultBackoffCapMs, DefaultRotationOverlapMs)
	// The endpoint's last successful delivery was A, before A aged out of
	// retention — e.g. the endpoint was disabled across several failures and
	// only re-enabled later (EVT-154's own scenario).
	st.RecordDelivered("01J8Z3K4N5P6Q7R8S9T0V1W2Y6")

	events, gap, gotErr := st.PendingAfter(l)
	if gotErr != nil {
		t.Fatalf("PendingAfter rerr = %v; want nil (an aged-out-but-still-known id is a gap, not an error)", gotErr)
	}

	// PendingAfter must agree EXACTLY with Resolve's own gap outcome — the
	// two mechanisms must never diverge (EVT-155).
	want, rerr := Resolve(l, st.LastDeliveredID)
	if rerr != nil {
		t.Fatalf("Resolve(last_delivered_id): %v", rerr)
	}
	if want.Result != ResumeResultGap {
		t.Fatalf("test setup invalid: Resolve.Result = %q; want gap", want.Result)
	}

	if gap == nil {
		t.Fatal("PendingAfter must return a gap frame when LastDeliveredID has aged out of retention (EVT-143/155) — an aged-out cursor must never silently resolve as a plain backlog")
	}
	if gap.Reason != ReasonRetentionExpired {
		t.Fatalf("gap reason = %q; want retention_expired", gap.Reason)
	}
	if gap.FromID == nil || *gap.FromID != "01J8Z3K4N5P6Q7R8S9T0V1W2Y6" {
		t.Fatalf("gap from_id = %v; want the endpoint's aged-out LastDeliveredID", gap.FromID)
	}
	wantOldest := "01J8Z3K4N5P6Q7R8S9T0V1W2Y8" // C, the oldest still-retained id
	if gap.ToID != wantOldest {
		t.Fatalf("gap to_id = %q; want %q (oldest retained id)", gap.ToID, wantOldest)
	}

	// No silent loss (EVT-143): delivery resumes AT the oldest retained id
	// inclusive — B is legitimately gone (aged out, gap-marked), but C and D
	// are still delivered, never dropped in addition.
	wantIDs := []string{"01J8Z3K4N5P6Q7R8S9T0V1W2Y8", "01J8Z3K4N5P6Q7R8S9T0V1W2Y9"}
	if got := ids(events); !stringSlicesEqual(got, wantIDs) {
		t.Fatalf("PendingAfter events = %v; want %v (resume AT the oldest retained id, EVT-143)", got, wantIDs)
	}
	if got, wantEv := ids(events), ids(want.Events); !stringSlicesEqual(got, wantEv) {
		t.Fatalf("PendingAfter events = %v diverge from Resolve's own gap backlog %v (EVT-155 requires identical gap behavior)", got, wantEv)
	}
}

// TestEndpointState_PendingAfter_UnresolvableLastDeliveredID_PropagatesError
// (EVT-134/143): if LastDeliveredID cannot be resolved against log at all —
// e.g. a fresh/reconstructed log (EventLog is in-memory only) that never
// recorded the envelope LastDeliveredID names, and whose lexicographic value
// is not even old enough to be a retention_expired gap — PendingAfter MUST
// propagate the RESUME_FROM_INVALID error rather than silently falling back
// to a gap-free full backlog. A gap-free full backlog is bit-for-bit
// indistinguishable from a brand-new endpoint that has never lost anything:
// exactly the "silently treated as fresh" behavior EVT-134 forbids for
// resume_from, and the silent-loss behavior EVT-143 forbids for any
// discontinuity.
func TestEndpointState_PendingAfter_UnresolvableLastDeliveredID_PropagatesError(t *testing.T) {
	l := NewEventLog(10)
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y9"))

	st := NewEndpointState("whsec_fixture_unresolvable_test", 10, DefaultBackoffBaseMs, DefaultBackoffCapMs, DefaultRotationOverlapMs)
	// Greater than the log's oldest retained id (so not a retention_expired
	// gap) but never actually recorded in l (so not a clean resume either) —
	// the one way Resolve can return RESUME_FROM_INVALID for an id that is
	// otherwise grammar/ULID valid.
	st.RecordDelivered("01J8Z3K4N5P6Q7R8S9T0V1W2ZZ")

	// Confirm the test setup actually hits Resolve's error case.
	_, rerr := Resolve(l, st.LastDeliveredID)
	if rerr == nil {
		t.Fatal("test setup invalid: Resolve(last_delivered_id) did not error")
	}

	events, gap, gotErr := st.PendingAfter(l)
	if gotErr == nil {
		t.Fatal("PendingAfter rerr = nil; want the propagated RESUME_FROM_INVALID error, not a silent fallback to a full backlog (EVT-134/143)")
	}
	if gotErr.Code != ResumeFromInvalidCode {
		t.Fatalf("PendingAfter rerr.Code = %q; want %q", gotErr.Code, ResumeFromInvalidCode)
	}
	if events != nil {
		t.Fatalf("PendingAfter events = %v; want nil on error — no fallback delivery", events)
	}
	if gap != nil {
		t.Fatalf("PendingAfter gap = %+v; want nil on error", gap)
	}
}

// TestEndpointState_PendingAfter_IDOrder (EVT-157): deliveries are owed in id
// order, and a later id is never handed out ahead of an earlier one still
// within its own retry budget — a failed attempt against the head does not
// advance the queue; only RecordDelivered does.
func TestEndpointState_PendingAfter_IDOrder(t *testing.T) {
	var l EventLog
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y6"))
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y7"))
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y8"))

	st := NewEndpointState("whsec_fixture_order_test", 10, DefaultBackoffBaseMs, DefaultBackoffCapMs, DefaultRotationOverlapMs)

	pendingEvents, pendingGap, pendingErr := st.PendingAfter(&l)
	if pendingErr != nil {
		t.Fatalf("PendingAfter rerr = %v; want nil", pendingErr)
	}
	pending := ids(pendingEvents)
	wantAll := []string{"01J8Z3K4N5P6Q7R8S9T0V1W2Y6", "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "01J8Z3K4N5P6Q7R8S9T0V1W2Y8"}
	if !stringSlicesEqual(pending, wantAll) {
		t.Fatalf("fresh endpoint's pending backlog = %v; want %v (id order)", pending, wantAll)
	}
	if pendingGap != nil {
		t.Fatalf("fresh endpoint's pending gap = %+v; want nil (nothing delivered yet, nothing to gap against)", pendingGap)
	}

	// A failed attempt against the head must not advance the queue.
	st.RecordAttempt(false, 0)
	if got, gap, err := st.PendingAfter(&l); err != nil || !stringSlicesEqual(ids(got), wantAll) || gap != nil {
		t.Fatalf("pending backlog after a failed attempt = %v (gap=%v, err=%v); want unchanged %v, no gap, no error (EVT-157)", ids(got), gap, err, wantAll)
	}

	// Only delivering the head advances the queue.
	st.RecordAttempt(true, 1000)
	st.RecordDelivered("01J8Z3K4N5P6Q7R8S9T0V1W2Y6")
	wantRemaining := []string{"01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "01J8Z3K4N5P6Q7R8S9T0V1W2Y8"}
	if got, gap, err := st.PendingAfter(&l); err != nil || !stringSlicesEqual(ids(got), wantRemaining) || gap != nil {
		t.Fatalf("pending backlog after delivering the head = %v (gap=%v, err=%v); want %v, no gap, no error", ids(got), gap, err, wantRemaining)
	}
}

// TestEndpointState_NextBackoffMs_CappedExponential (EVT-153): backoff
// doubles from the configured base on each successive attempt and never
// exceeds the configured cap.
func TestEndpointState_NextBackoffMs_CappedExponential(t *testing.T) {
	st := NewEndpointState("whsec_fixture_backoff_test", 10, 1000, 8000, DefaultRotationOverlapMs)
	cases := []struct {
		attempt int
		want    int64
	}{
		{1, 1000},
		{2, 2000},
		{3, 4000},
		{4, 8000},
		{5, 8000}, // capped
		{20, 8000},
	}
	for _, c := range cases {
		if got := st.NextBackoffMs(c.attempt); got != c.want {
			t.Fatalf("NextBackoffMs(%d) = %d; want %d (EVT-153 capped exponential)", c.attempt, got, c.want)
		}
	}
}

// TestEndpointState_AcceptSignature_RotationOverlap (EVT-158): a signature
// under the current secret always verifies; a signature under the
// immediately prior secret verifies only within the configured rotation
// overlap window after rotation, never before rotation happened or after the
// window elapses.
func TestEndpointState_AcceptSignature_RotationOverlap(t *testing.T) {
	const initialSecret = "whsec_fixture_initial_9f8e7d"
	const rotatedSecret = "whsec_fixture_rotated_1a2b3c"
	const overlapMs = int64(24 * 60 * 60 * 1000)

	st := NewEndpointState(initialSecret, 10, DefaultBackoffBaseMs, DefaultBackoffCapMs, overlapMs)
	body := []byte(`{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2YB","schema":"automation.run"}`)
	timestamp := "1752537603"

	sigInitial := WebhookSignature(initialSecret, timestamp, body)
	if !st.AcceptSignature(body, timestamp, sigInitial, 0) {
		t.Fatal("a signature under the current secret must verify before any rotation")
	}

	const rotateAt = int64(1_000_000)
	st.RotateSecret(rotatedSecret, rotateAt)

	sigRotated := WebhookSignature(rotatedSecret, timestamp, body)
	if !st.AcceptSignature(body, timestamp, sigRotated, rotateAt) {
		t.Fatal("a signature under the newly rotated secret must verify immediately")
	}
	if !st.AcceptSignature(body, timestamp, sigInitial, rotateAt+overlapMs-1) {
		t.Fatal("a signature under the prior secret must verify within the rotation overlap window (EVT-158)")
	}
	if st.AcceptSignature(body, timestamp, sigInitial, rotateAt+overlapMs+1) {
		t.Fatal("a signature under the prior secret must NOT verify once the rotation overlap window has elapsed (EVT-158)")
	}
	if !st.AcceptSignature(body, timestamp, sigRotated, rotateAt+overlapMs+1) {
		t.Fatal("the current secret must always verify, regardless of the overlap window")
	}
	if st.AcceptSignature(body, timestamp, "not-a-real-signature", rotateAt) {
		t.Fatal("an unrelated signature must never verify")
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
