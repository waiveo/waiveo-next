package store_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// Guards at this package's exported boundary, from the sweep recorded in issue
// #179. Every one below was re-run against `go test ./...` and survives there,
// not merely against this package's own suite — the distinction that made the
// first version of that issue overstate its evidence.
//
// What they have in common is that the caller is trusted code inside this repo,
// so none of them is reachable from a request today. That is an argument for
// testing them, not against it: a boundary check earns its place by holding for
// the caller who has not been written yet, and the way it stops holding is
// silently, because nothing in the suite ever passed it a bad value.

// TestAKindOutsideTheClosedSetNeverReachesTheSQL is the most consequential of
// them, and its own comment says why: this guard "is what makes the `... `+table`
// interpolation in the CRUD SQL safe: only a known-good constant table name ever
// reaches it".
//
// A Kind is a string type, and every Kind in production is a package-level
// constant — no code anywhere converts a request value into one, so the guard
// cannot fire today. But it is the only thing between a Kind and a table name
// spliced directly into SQL, and "no caller does that yet" is a property of the
// current callers rather than of the type.
//
// So this drives it through every CRUD entry point that takes a Kind, using
// values a string-typed parameter permits: an unknown name, a SQL fragment, and
// the empty string.
func TestAKindOutsideTheClosedSetNeverReachesTheSQL(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	for _, kind := range []store.Kind{
		"",
		"not_a_table",
		"scope_nodes; DROP TABLE scope_nodes",
		"scope_nodes--",
		"sqlite_master",
	} {
		t.Run(string(kind), func(t *testing.T) {
			if _, err := s.Create(ctx, kind, json.RawMessage(`{"id":"01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"}`)); err == nil {
				t.Errorf("Create accepted kind %q — this check is the only thing standing between a Kind and a "+
					"table name interpolated into the CRUD SQL", kind)
			}
			if _, _, err := s.Get(ctx, kind, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"); err == nil {
				t.Errorf("Get accepted kind %q", kind)
			}
			if _, err := s.List(ctx, kind, store.ListFilter{}); err == nil {
				t.Errorf("List accepted kind %q", kind)
			}
			if _, err := s.Update(ctx, kind, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", 1, json.RawMessage(`{}`)); err == nil {
				t.Errorf("Update accepted kind %q", kind)
			}
			if err := s.Delete(ctx, kind, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", 1); err == nil {
				t.Errorf("Delete accepted kind %q", kind)
			}
		})
	}

	// The control: a real kind still reaches its table through every one of those
	// entry points. Without it, a store that refused every kind would satisfy
	// each assertion above while serving nothing.
	if _, err := s.List(ctx, store.KindScopeNode, store.ListFilter{}); err != nil {
		t.Fatalf("List refused a known kind: %v", err)
	}
}

// TestARowWithNoIdentityFieldIsRefused pins the PROPERTY and deliberately names
// no layer, because the guard the sweep flagged turns out not to be the only
// thing holding it.
//
// Create reads the id out of the caller's body and makes it the row's primary
// key, and an empty one would store a row under the empty string. There is an
// explicit check for that — and disabling it leaves the property intact, because
// the row then fails datamodel validation instead.
//
// I am not asserting that check's own message, and the reason is worth stating.
// Its wording is "row is missing its identity field"; the validator's is
// "validation failed (3 error(s))", which enumerates every field actually wrong
// rather than the first one noticed. Pinning the guard's message would entrench
// the LESS informative of the two answers as the required one. So what is
// asserted is that no row without an identity is created, whichever layer says
// so — and the guard stays as the earlier, cheaper refusal.
func TestARowWithNoIdentityFieldIsRefused(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	for _, body := range []string{
		`{}`,
		`{"id":""}`,
		`{"name":"a scope node with no id"}`,
	} {
		if _, err := s.Create(ctx, store.KindScopeNode, json.RawMessage(body)); err == nil {
			t.Errorf("Create accepted a row with no identity field: %s", body)
		}
	}
}

// TestSnapshotIntoRefusesAnEmptyDestination asserts the MESSAGE, because an
// empty path fails either way and the two failures are not equivalent.
//
// The destination is interpolated into `VACUUM INTO` as a SQL string literal —
// the method's own comment says so. With the check, an empty path is refused
// before anything runs, as "empty destination path". Without it the call
// proceeds, attempts the vacuum, and surfaces as
// "snapshot into : secretfile: tighten : chmod : no such file or directory" —
// a chmod failure on a path the operator never gave, reported from two layers
// below the mistake, after the statement has already been attempted.
//
// A presence assertion cannot tell those apart, which is why this one does not
// use it.
func TestSnapshotIntoRefusesAnEmptyDestination(t *testing.T) {
	s := openMem(t)
	err := s.SnapshotInto(context.Background(), "")
	if err == nil {
		t.Fatal("SnapshotInto accepted an empty destination — that value is interpolated into VACUUM INTO")
	}
	if !strings.Contains(err.Error(), "empty destination path") {
		t.Errorf("an empty destination failed as %q — it must be refused BEFORE the vacuum is attempted and named "+
			"as the empty path it is, not surfaced as a chmod failure on a path the caller never gave", err)
	}

	// The control: a real destination still snapshots.
	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := s.SnapshotInto(context.Background(), dst); err != nil {
		t.Fatalf("SnapshotInto refused a real destination: %v", err)
	}
}

// TestCreateJobRefusesAJobWithNoCreator is API-112.
//
// A job is a durable record of something a principal asked for, and created_by
// is the only field that says who. Stored empty, the job is not merely
// unattributed — it is indistinguishable from one whose creator was recorded as
// nothing, which is what an audit trail cannot afford to be ambiguous about.
//
// The nil-job case beside it is the cruder half of the same boundary: without
// it, the very next line dereferences the pointer.
func TestCreateJobRefusesAJobWithNoCreator(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.CreateJob(ctx, nil, fixtureScopes(), fixtureOp()); err == nil {
		t.Error("CreateJob accepted a nil job — the next line reads its id")
	}

	noCreator := newFixtureJob()
	noCreator.CreatedBy = ""
	if err := s.CreateJob(ctx, noCreator, fixtureScopes(), fixtureOp()); err == nil {
		t.Error("CreateJob accepted a job with no created_by — that field is the only record of which principal " +
			"asked for the work (API-112), and an empty one cannot be told from a creator recorded as nothing")
	}

	// The control: an attributed job is still accepted, and reads back with its
	// creator intact.
	if err := s.CreateJob(ctx, newFixtureJob(), fixtureScopes(), fixtureOp()); err != nil {
		t.Fatalf("CreateJob refused a well-formed job: %v", err)
	}
}

// TestAdvanceJobRefusesANilTransition.
//
// AdvanceJob's whole contract is that the caller's function drives the state
// machine under the write lock. A nil one is not a no-op: without the check it
// is called, and the transaction that was going to commit the result panics
// midway through the write path.
func TestAdvanceJobRefusesANilTransition(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.CreateJob(ctx, newFixtureJob(), fixtureScopes(), fixtureOp()); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := s.AdvanceJob(ctx, jobID, nil); err == nil {
		t.Error("AdvanceJob accepted a nil transition function — it is invoked inside the write transaction")
	}
}

// TestRotateWebhookSecretRefusesAnEmptySecret is the sharpest of the webhook
// guards, because the value it refuses is one every consumer reads as "this
// endpoint has no secret".
//
// webhookdeliver's Open says so in terms: an empty current secret "means the
// endpoint has none yet — not an error, just an endpoint nothing can be signed
// for". And the api layer reports secret_set_at only when the sealed secret is
// non-empty.
//
// So without this check a rotation does not install a bad secret — it SILENTLY
// UNSETS signing. The upsert overwrites the real sealed blob with "", deliveries
// from that endpoint go out unsigned, and the console shows no secret at all for
// an endpoint an operator has just rotated. Every layer behaves correctly on the
// value it is given; the only place the impossible value can be refused is here.
func TestRotateWebhookSecretRefusesAnEmptySecret(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	const endpoint = "01J8ZWEBH00KENDP01NTAAAAA1"

	if err := s.RotateWebhookSecret(ctx, endpoint, "sealed-current-blob", "", 1752537000000); err != nil {
		t.Fatalf("RotateWebhookSecret: %v", err)
	}

	if err := s.RotateWebhookSecret(ctx, endpoint, "", "sealed-current-blob", 1752537600000); err == nil {
		t.Error("RotateWebhookSecret accepted an empty secret — every consumer reads that as 'this endpoint has " +
			"no secret', so the rotation would silently unset signing and deliveries would go out unsigned")
	}

	// The secret that was there is still there: the refusal wrote nothing.
	st, err := s.WebhookDeliveryStateFor(ctx, endpoint)
	if err != nil {
		t.Fatalf("WebhookDeliveryStateFor: %v", err)
	}
	if st.SealedSecret != "sealed-current-blob" {
		t.Errorf("sealed secret is %q after a refused rotation, want the one installed before it", st.SealedSecret)
	}

	// The control: a real rotation still replaces it.
	if err := s.RotateWebhookSecret(ctx, endpoint, "sealed-next-blob", "sealed-current-blob", 1752538000000); err != nil {
		t.Fatalf("a real rotation was refused: %v", err)
	}
}

// TestWebhookWritesRefuseAnEmptyEndpointID covers both writers.
//
// endpoint_id is the primary key of the row each one upserts, so an empty one
// does not fail — it creates a single shared row under "" that every
// unidentified caller then overwrites. The delivery loop's progress and an
// operator's rotation would land on the same row.
func TestWebhookWritesRefuseAnEmptyEndpointID(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.RotateWebhookSecret(ctx, "", "sealed-blob", "", 1752537000000); err == nil {
		t.Error("RotateWebhookSecret accepted an empty endpoint id")
	}
	if err := s.PutWebhookDeliveryProgress(ctx, store.WebhookDeliveryState{}); err == nil {
		t.Error("PutWebhookDeliveryProgress accepted a state with no endpoint id — that is the row's primary key, " +
			"so every unidentified writer would share one row")
	}

	// The control: both writers work with a real endpoint id.
	const endpoint = "01J8ZWEBH00KENDP01NTBBBBB2"
	if err := s.RotateWebhookSecret(ctx, endpoint, "sealed-blob", "", 1752537000000); err != nil {
		t.Fatalf("RotateWebhookSecret refused a real endpoint: %v", err)
	}
	if err := s.PutWebhookDeliveryProgress(ctx, store.WebhookDeliveryState{EndpointID: endpoint, Status: "active"}); err != nil {
		t.Fatalf("PutWebhookDeliveryProgress refused a real endpoint: %v", err)
	}
}
