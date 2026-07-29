package store_test

import (
	"errors"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

const (
	webhookEndpointA = "01J8Z3K4N5P6Q7R8S9T0V1WEA1"
	webhookEndpointB = "01J8Z3K4N5P6Q7R8S9T0V1WEB2"
	webhookOrgNodeID = "01J8Z3K4N5P6Q7R8S9T0V1WE03"
)

func openWebhookStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestWebhookDeliveryStateAbsentUntilWritten pins that an endpoint nobody has
// given a secret or attempted a delivery for has NO state row, rather than a
// zero-valued one. A zero row would read as a configured endpoint with an empty
// secret, which is exactly the state that must never sign anything.
func TestWebhookDeliveryStateAbsentUntilWritten(t *testing.T) {
	s := openWebhookStore(t)
	_, err := s.WebhookDeliveryStateFor(t.Context(), webhookEndpointA)
	if !errors.Is(err, store.ErrWebhookStateNotFound) {
		t.Fatalf("WebhookDeliveryStateFor on an untouched endpoint = %v; want ErrWebhookStateNotFound", err)
	}
}

// TestRotateWebhookSecretKeepsExactlyOnePriorGeneration (EVT-158): the first
// install opens no overlap (there is no earlier secret a receiver could hold),
// and each later rotation demotes the CURRENT secret to prior — never two
// generations back.
func TestRotateWebhookSecretKeepsExactlyOnePriorGeneration(t *testing.T) {
	s := openWebhookStore(t)
	ctx := t.Context()

	if err := s.RotateWebhookSecret(ctx, webhookEndpointA, "sealed-one", "", 1_000); err != nil {
		t.Fatalf("first RotateWebhookSecret: %v", err)
	}
	st, err := s.WebhookDeliveryStateFor(ctx, webhookEndpointA)
	if err != nil {
		t.Fatalf("WebhookDeliveryStateFor: %v", err)
	}
	if st.SealedSecret != "sealed-one" {
		t.Fatalf("sealed secret = %q; want the freshly installed one", st.SealedSecret)
	}
	if st.SealedPriorSecret != "" {
		t.Fatalf("prior secret = %q after the FIRST install; want empty — nothing was superseded", st.SealedPriorSecret)
	}
	if st.RotatedAtMs != 1_000 {
		t.Fatalf("rotated_at_ms = %d; want 1000", st.RotatedAtMs)
	}
	if st.Status != "active" {
		t.Fatalf("status = %q on a freshly created row; want active", st.Status)
	}

	if err := s.RotateWebhookSecret(ctx, webhookEndpointA, "sealed-two", "prior-one", 2_000); err != nil {
		t.Fatalf("second RotateWebhookSecret: %v", err)
	}
	st, err = s.WebhookDeliveryStateFor(ctx, webhookEndpointA)
	if err != nil {
		t.Fatalf("WebhookDeliveryStateFor: %v", err)
	}
	if st.SealedSecret != "sealed-two" || st.SealedPriorSecret != "prior-one" {
		t.Fatalf("after one rotation: secret=%q prior=%q; want sealed-two / prior-one", st.SealedSecret, st.SealedPriorSecret)
	}

	if err := s.RotateWebhookSecret(ctx, webhookEndpointA, "sealed-three", "prior-two", 3_000); err != nil {
		t.Fatalf("third RotateWebhookSecret: %v", err)
	}
	st, err = s.WebhookDeliveryStateFor(ctx, webhookEndpointA)
	if err != nil {
		t.Fatalf("WebhookDeliveryStateFor: %v", err)
	}
	if st.SealedSecret != "sealed-three" || st.SealedPriorSecret != "prior-two" {
		t.Fatalf("after two rotations: secret=%q prior=%q; want sealed-three / prior-two — only the IMMEDIATELY prior secret is kept (EVT-158)", st.SealedSecret, st.SealedPriorSecret)
	}
	if st.SealedPriorSecret == "prior-one" {
		t.Fatal("two generations back is still accepted; EVT-158 keeps exactly one")
	}
}

// TestProgressAndSecretWritesDoNotClobberEachOther is the reason the two halves
// of the row are written by separate statements: a delivery attempt that
// started before a rotation must not restore the superseded secret when it
// writes its progress back, and a rotation must not rewind the cursor.
func TestProgressAndSecretWritesDoNotClobberEachOther(t *testing.T) {
	s := openWebhookStore(t)
	ctx := t.Context()

	if err := s.RotateWebhookSecret(ctx, webhookEndpointA, "sealed-one", "", 1_000); err != nil {
		t.Fatalf("RotateWebhookSecret: %v", err)
	}
	if err := s.PutWebhookDeliveryProgress(ctx, store.WebhookDeliveryState{
		EndpointID: webhookEndpointA, Status: "active",
		LastDeliveredID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", Attempt: 2,
		DeliveryID: "01J8Z3K4N5P6Q7R8S9T0V1W2YF", NextAttemptAtMs: 9_000,
	}); err != nil {
		t.Fatalf("PutWebhookDeliveryProgress: %v", err)
	}

	// The rotation lands while that delivery is still mid-retry.
	if err := s.RotateWebhookSecret(ctx, webhookEndpointA, "sealed-two", "prior-one", 2_000); err != nil {
		t.Fatalf("RotateWebhookSecret: %v", err)
	}
	st, err := s.WebhookDeliveryStateFor(ctx, webhookEndpointA)
	if err != nil {
		t.Fatalf("WebhookDeliveryStateFor: %v", err)
	}
	if st.LastDeliveredID != "01J8Z3K4N5P6Q7R8S9T0V1W2Y7" || st.Attempt != 2 || st.DeliveryID != "01J8Z3K4N5P6Q7R8S9T0V1W2YF" || st.NextAttemptAtMs != 9_000 {
		t.Fatalf("a rotation rewound delivery progress: %+v", st)
	}

	// The in-flight delivery then writes its own progress back.
	if err := s.PutWebhookDeliveryProgress(ctx, store.WebhookDeliveryState{
		EndpointID: webhookEndpointA, Status: "active",
		LastDeliveredID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y8", ConsecutiveFailures: 1,
	}); err != nil {
		t.Fatalf("PutWebhookDeliveryProgress: %v", err)
	}
	st, err = s.WebhookDeliveryStateFor(ctx, webhookEndpointA)
	if err != nil {
		t.Fatalf("WebhookDeliveryStateFor: %v", err)
	}
	if st.SealedSecret != "sealed-two" || st.SealedPriorSecret != "prior-one" {
		t.Fatalf("a progress write restored a superseded secret: secret=%q prior=%q", st.SealedSecret, st.SealedPriorSecret)
	}
	if st.RotatedAtMs != 2_000 {
		t.Fatalf("a progress write moved rotated_at_ms to %d; want 2000 — the overlap window is not the delivery loop's to reopen", st.RotatedAtMs)
	}
	if st.LastDeliveredID != "01J8Z3K4N5P6Q7R8S9T0V1W2Y8" || st.ConsecutiveFailures != 1 {
		t.Fatalf("progress did not land: %+v", st)
	}
}

// TestPurgeWorkspaceDestroysWebhookSigningSecrets: a data-subject purge that
// left the sealed signing secrets behind would keep credential material for
// endpoints it had just destroyed. The state table is not a resource Kind, so
// the allKinds loop does not reach it — this is what says it is reached anyway.
func TestPurgeWorkspaceDestroysWebhookSigningSecrets(t *testing.T) {
	st := openWebhookStore(t)
	ctx := t.Context()
	if _, err := st.Create(ctx, store.KindScopeNode,
		[]byte(`{"id":"`+webhookOrgNodeID+`","kind":"org","name":"Root Org"}`)); err != nil {
		t.Fatalf("create org node: %v", err)
	}

	if err := st.RotateWebhookSecret(ctx, webhookEndpointA, "sealed-one", "", 1_000); err != nil {
		t.Fatalf("RotateWebhookSecret: %v", err)
	}
	if _, err := st.WebhookDeliveryStateFor(ctx, webhookEndpointA); err != nil {
		t.Fatalf("test setup invalid — no state to purge: %v", err)
	}

	if err := st.PurgeWorkspace(ctx); err != nil {
		t.Fatalf("PurgeWorkspace: %v", err)
	}
	if _, err := st.WebhookDeliveryStateFor(ctx, webhookEndpointA); !errors.Is(err, store.ErrWebhookStateNotFound) {
		t.Fatalf("webhook delivery state survived a workspace purge (err = %v); the sealed signing secret outlived the endpoint it belonged to", err)
	}
}

// TestWebhookDeliveryStatesAreListedInEndpointIDOrder pins the inventory read
// the delivery loop walks: every endpoint, deterministically ordered, so one
// endpoint's turn never depends on storage-engine iteration order.
func TestWebhookDeliveryStatesAreListedInEndpointIDOrder(t *testing.T) {
	s := openWebhookStore(t)
	ctx := t.Context()

	if err := s.RotateWebhookSecret(ctx, webhookEndpointB, "sealed-b", "", 1_000); err != nil {
		t.Fatalf("RotateWebhookSecret(B): %v", err)
	}
	if err := s.RotateWebhookSecret(ctx, webhookEndpointA, "sealed-a", "", 1_000); err != nil {
		t.Fatalf("RotateWebhookSecret(A): %v", err)
	}

	states, err := s.WebhookDeliveryStates(ctx)
	if err != nil {
		t.Fatalf("WebhookDeliveryStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("listed %d states; want 2", len(states))
	}
	if states[0].EndpointID != webhookEndpointA || states[1].EndpointID != webhookEndpointB {
		t.Fatalf("listed order = %q, %q; want ascending endpoint id", states[0].EndpointID, states[1].EndpointID)
	}

	if err := s.DeleteWebhookDeliveryState(ctx, webhookEndpointA); err != nil {
		t.Fatalf("DeleteWebhookDeliveryState: %v", err)
	}
	if _, err := s.WebhookDeliveryStateFor(ctx, webhookEndpointA); !errors.Is(err, store.ErrWebhookStateNotFound) {
		t.Fatalf("after delete = %v; want ErrWebhookStateNotFound", err)
	}
	// Deleting what is already gone is a reconciliation, not an assertion.
	if err := s.DeleteWebhookDeliveryState(ctx, webhookEndpointA); err != nil {
		t.Fatalf("second DeleteWebhookDeliveryState: %v; want nil", err)
	}
}
