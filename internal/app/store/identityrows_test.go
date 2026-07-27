package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// identityrows_test.go covers the two identity kinds through the STORE's own
// write path: that they persist under the shared resource baseline, that a write
// to either table re-validates BOTH (the screen→device link crosses them), and
// that the declared-required members a create body omitted come back
// materialized.

const (
	idScreenRowA = "01J8Z8SCREENR0WAAAAAAAAAA1"
	idDeviceRowA = "01J8Z8DEVCEADPTEDAAAAAAAA1"
	idDeviceRowB = "01J8Z8DEVCEADPTEDBBBBBBBB2"
	idEntityRowA = "01J8Z8ENTTYMEDAPAYERAAAAA1"
)

func deviceRow(id, driver, nativeID string) datamodel.Device {
	return datamodel.Device{
		ID:        id,
		ScopeNode: screenNodeID,
		Name:      "Lobby Roku",
		Driver:    driver,
		NativeID:  nativeID,
		Entities: []datamodel.DeviceEntity{{
			EntityID:    idEntityRowA,
			DeviceClass: "media-player",
			Enabled:     true,
			DisplayName: "Lobby Roku",
			Category:    "primary",
		}},
	}
}

func screenRow(id string, deviceID *string) datamodel.Screen {
	return datamodel.Screen{ID: id, ScopeNode: screenNodeID, Name: "Lobby Screen", DeviceID: deviceID}
}

// createIdentityRows writes one device and one screen linked to it, returning
// both stored resources.
func createIdentityRows(t *testing.T, s *store.Store) (dev, scr store.Resource) {
	t.Helper()
	ctx := context.Background()
	dev, err := s.Create(ctx, store.KindAdoptedDevice, mustJSON(t, deviceRow(idDeviceRowA, "roku-ecp", "10.0.0.41")))
	if err != nil {
		t.Fatalf("create device row: %v", err)
	}
	link := idDeviceRowA
	scr, err = s.Create(ctx, store.KindScreen, mustJSON(t, screenRow(idScreenRowA, &link)))
	if err != nil {
		t.Fatalf("create screen row: %v", err)
	}
	return dev, scr
}

// TestIdentityRowsCarryTheResourceBaseline pins DAT-005b: both kinds get the same
// server-owned baseline every other kind gets — revision 1, timestamps, and the
// denormalized placement — with no per-kind special case.
func TestIdentityRowsCarryTheResourceBaseline(t *testing.T) {
	s := openMem(t)
	dev, scr := createIdentityRows(t, s)

	for _, tc := range []struct {
		kind string
		res  store.Resource
	}{{"device", dev}, {"screen", scr}} {
		if tc.res.Revision != 1 {
			t.Errorf("%s revision = %d, want 1", tc.kind, tc.res.Revision)
		}
		if tc.res.ScopeNode != screenNodeID {
			t.Errorf("%s scope_node = %q, want %q", tc.kind, tc.res.ScopeNode, screenNodeID)
		}
		if tc.res.CreatedAt == 0 || tc.res.UpdatedAt == 0 {
			t.Errorf("%s timestamps not stamped: created=%d updated=%d", tc.kind, tc.res.CreatedAt, tc.res.UpdatedAt)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(tc.res.Body, &body); err != nil {
			t.Fatalf("%s body is not an object: %v", tc.kind, err)
		}
		if _, ok := body["labels"]; !ok {
			t.Errorf("%s body omits the declared-required labels member: %s", tc.kind, tc.res.Body)
		}
	}
}

// TestIdentityRowDeclaredMembersMaterialized pins that a MINIMAL create — one
// naming none of the optional members — still comes back carrying them, so a
// typed client never meets a missing member.
func TestIdentityRowDeclaredMembersMaterialized(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	res, err := s.Create(ctx, store.KindScreen, json.RawMessage(
		`{"id":"`+idScreenRowA+`","scope_node":"`+screenNodeID+`","name":"Lobby Screen"}`))
	if err != nil {
		t.Fatalf("minimal screen create: %v", err)
	}
	if !strings.Contains(string(res.Body), `"device_id":null`) {
		t.Errorf("a screen created with no device link did not come back with device_id null: %s", res.Body)
	}

	res, err = s.Create(ctx, store.KindAdoptedDevice, json.RawMessage(
		`{"id":"`+idDeviceRowA+`","scope_node":"`+screenNodeID+`","name":"Lobby Roku","driver":"roku-ecp","native_id":"10.0.0.41"}`))
	if err != nil {
		t.Fatalf("minimal device create: %v", err)
	}
	for _, want := range []string{`"entities":[]`, `"poll_cadence_seconds":null`} {
		if !strings.Contains(string(res.Body), want) {
			t.Errorf("a minimal device create did not come back carrying %s: %s", want, res.Body)
		}
	}
}

// TestScreenLinkToAbsentDeviceRejected pins that the store refuses a screen whose
// PLY-124 link names no device row, and that the refusal leaves nothing behind.
func TestScreenLinkToAbsentDeviceRejected(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	before := gen(t, s)

	missing := idDeviceRowB
	_, err := s.Create(ctx, store.KindScreen, mustJSON(t, screenRow(idScreenRowA, &missing)))
	if err == nil {
		t.Fatal("a screen linked to a device row that does not exist was accepted")
	}
	var verr *store.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("want a *store.ValidationError, got %T: %v", err, err)
	}
	if gen(t, s) != before {
		t.Error("a rejected write advanced the store generation")
	}
	rows, err := s.List(ctx, store.KindScreen, store.ListFilter{})
	if err != nil {
		t.Fatalf("List screens: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a rejected write left %d screen row(s) behind", len(rows))
	}
}

// TestDeletingALinkedDeviceRejected is the cross-table half: a write to the
// DEVICE table re-validates the SCREEN table, so removing a device a screen still
// points at is refused — the same protection deleting a playlist a daypart points
// at already has.
func TestDeletingALinkedDeviceRejected(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	dev, _ := createIdentityRows(t, s)

	if err := s.Delete(ctx, store.KindAdoptedDevice, dev.ID, dev.Revision); err == nil {
		t.Fatal("deleting a device a screen still links to was accepted, leaving a dangling reference")
	}
	// The device must still be there: a rejected delete rolls the whole
	// transaction back.
	if _, found, err := s.Get(ctx, store.KindAdoptedDevice, dev.ID); err != nil || !found {
		t.Fatalf("the refused delete removed the row anyway (found=%v err=%v)", found, err)
	}
}

// TestDuplicateDeviceIdentityRejected pins relay/1 REL-153 through the store: two
// rows cannot claim one (driver, native_id), because re-adopting one physical
// device must resolve to one device_id.
func TestDuplicateDeviceIdentityRejected(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, store.KindAdoptedDevice, mustJSON(t, deviceRow(idDeviceRowA, "roku-ecp", "10.0.0.41"))); err != nil {
		t.Fatalf("first device create: %v", err)
	}
	if _, err := s.Create(ctx, store.KindAdoptedDevice, mustJSON(t, deviceRow(idDeviceRowB, "roku-ecp", "10.0.0.41"))); err == nil {
		t.Fatal("a second device row claiming the same (driver, native_id) was accepted")
	}
}

// TestIdentityRowUpdateBumpsRevision pins the optimistic-concurrency envelope:
// an update at the current revision succeeds and advances it, and a stale one is
// refused.
func TestIdentityRowUpdateBumpsRevision(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	_, scr := createIdentityRows(t, s)

	updated, err := s.Update(ctx, store.KindScreen, scr.ID, scr.Revision, json.RawMessage(`{"name":"Atrium Screen"}`))
	if err != nil {
		t.Fatalf("update screen at its current revision: %v", err)
	}
	if updated.Revision != scr.Revision+1 {
		t.Fatalf("revision = %d, want %d", updated.Revision, scr.Revision+1)
	}
	if !strings.Contains(string(updated.Body), "Atrium Screen") {
		t.Errorf("the update did not take: %s", updated.Body)
	}
	if _, err := s.Update(ctx, store.KindScreen, scr.ID, scr.Revision, json.RawMessage(`{"name":"Stale"}`)); err == nil {
		t.Fatal("an update at a stale revision was accepted")
	}
}
