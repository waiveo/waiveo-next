package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	_ "modernc.org/sqlite"
)

// preRuleSeedIDs is the demo seed's identifiers as they were BEFORE DAT-005a was
// enforced, keyed by the identifier the seed carries today. They are the literals
// from the commit that added the enforcement, which regenerated them in the same
// change — so this table is the historical record of what a box seeded by an
// older build actually holds on disk, not something derived from the migration
// under test.
//
// The three demo identifiers that were already canonical (the site scope node,
// the rule entity, and the demo automation) are deliberately absent: they did not
// change, and a migration that touched them would be a bug this file is here to
// catch.
var preRuleSeedIDs = map[string]string{
	"01J8Z0DEM00RGANCEST0RB0VND": "01J8Z0DEMOORGANCESTORBOUND", // org ancestor (a dangling parent_id)
	"01J8Z4DEM0SCREENF1RSTPH0TN": "01J8Z4DEMOSCREENFIRSTPHOTN", // screen scope node
	"01J8Z5DEM0SCHEDV1ETW0DAYPT": "01J8Z5DEMOSCHEDULETWODAYPT", // schedule
	"01J8Z6DEM0P1AY11STC0NTENT1": "01J8Z6DEMOPLAYLISTCONTENT1", // playlist
	"01J8Z7DEM0DAYPARTC0NTENT01": "01J8Z7DEMODAYPARTCONTENT01", // content daypart
	"01J8Z7DEM0DAYPART0VERN1GHT": "01J8Z7DEMODAYPARTOVERNIGHT", // overnight daypart
	"01J8Z8DEM0PRESETBATCHF1RE1": "01J8Z8DEMOPRESETBATCHFIRE1", // preset batch
}

// preRuleUnchangedSeedIDs are the demo identifiers that were already canonical
// before the rule landed. The site node matters most: the feeder binds a relay to
// that exact scope node over the hello handshake, and the binding lives outside
// this store — rewriting it would strand an adopted relay.
var preRuleUnchangedSeedIDs = []string{
	"01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", // site scope node
	"01J8Z3K4N5P6Q7R8S9T0V1SCRN", // rule entity
	"01J8Z3K4N5P6Q7R8S9T0V1AT01", // demo automation
}

// idMigrationTables is every table the canonicalization reads or writes, with the
// columns this file compares. It is spelled out here rather than discovered from
// the store so a table added later without a decision about its identifiers shows
// up as a compile-clean but failing test rather than as silence.
var idMigrationTables = []struct {
	name    string
	columns []string
	order   string
}{
	{"scope_nodes", []string{"id", "revision", "external_id", "labels", "scope_node", "created_at", "updated_at", "body"}, "id"},
	{"playlists", []string{"id", "revision", "external_id", "labels", "scope_node", "created_at", "updated_at", "body"}, "id"},
	{"schedules", []string{"id", "revision", "external_id", "labels", "scope_node", "created_at", "updated_at", "body"}, "id"},
	{"dayparts", []string{"id", "revision", "external_id", "labels", "scope_node", "created_at", "updated_at", "body"}, "id"},
	{"validity_windows", []string{"id", "revision", "external_id", "labels", "scope_node", "created_at", "updated_at", "body"}, "id"},
	{"fallbacks", []string{"id", "revision", "external_id", "labels", "scope_node", "created_at", "updated_at", "body"}, "id"},
	{"preset_batches", []string{"id", "revision", "external_id", "labels", "scope_node", "created_at", "updated_at", "body"}, "id"},
	{"automations", []string{"id", "revision", "external_id", "labels", "scope_node", "created_at", "updated_at", "body", "execution_class"}, "id"},
	{"jobs", []string{"id", "created_by", "created_at", "updated_at"}, "id"},
	{"job_targets", []string{"job_id", "seq", "target_id", "state", "scope_node"}, "job_id, seq"},
	{"pack_rows", []string{"pack_id", "collection", "entity_id", "scope_node", "body"}, "pack_id, collection, entity_id"},
	{"meta", []string{"id", "generation"}, "id"},
}

// newPlaylistBody is a create the store must accept once — and only once — the
// stored identifiers conform. Its own id is canonical, so the only thing that can
// reject it is DAT-005a's judgement of the resulting FULL row set.
const newPlaylistID = "01JAAAAAAAAAAAAAAAAAAAAAAA"

func newPlaylistBody(scopeNode string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"id":%q,"scope_node":%q,"name":"Authored After The Rule","items":[{"source":"asset","asset_ref":%q}]}`,
		newPlaylistID, scopeNode, seedAssetRef))
}

// openStoreAt opens a file-backed store and closes it when the test ends.
func openStoreAt(t *testing.T, dsn string) *store.Store {
	t.Helper()
	s, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", dsn, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// withRawDB runs fn against the store file directly, bypassing every validator
// the store package interposes. It is how this file forges the states a store
// package that enforces DAT-005a can no longer produce: a store as an older build
// left it, and the malformed identifiers the migration must refuse.
func withRawDB(t *testing.T, dsn string, fn func(db *sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("raw open %s: %v", dsn, err)
	}
	defer db.Close()
	fn(db)
}

// regressToPreRuleIDs rewrites a freshly seeded store back to the identifiers the
// pre-DAT-005a build wrote, producing a genuine write-dead store on disk.
//
// The substitution here is a plain unanchored replacement over the whole of each
// text column — deliberately a different (and blunter) mechanism than the
// migration's quoted-token replacement, so the fixture is not the code under test
// run backwards.
func regressToPreRuleIDs(t *testing.T, dsn string) {
	t.Helper()
	withRawDB(t, dsn, func(db *sql.DB) {
		for _, tbl := range idMigrationTables {
			if tbl.name == "meta" || tbl.name == "jobs" {
				continue
			}
			text := textColumns(tbl.columns)
			if len(text) == 0 {
				continue
			}
			keys := primaryKeyColumns(tbl.name)
			sel := strings.Join(append(append([]string{}, keys...), text...), ", ")
			rows, err := db.Query(`SELECT ` + sel + ` FROM ` + tbl.name)
			if err != nil {
				t.Fatalf("regress: select %s: %v", tbl.name, err)
			}
			type update struct{ key, vals []any }
			var updates []update
			for rows.Next() {
				cells := make([]any, len(keys)+len(text))
				ptrs := make([]any, len(cells))
				for i := range cells {
					ptrs[i] = &cells[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					rows.Close()
					t.Fatalf("regress: scan %s: %v", tbl.name, err)
				}
				vals := make([]any, len(text))
				for i, c := range cells[len(keys):] {
					s, _ := c.(string)
					for cur, old := range preRuleSeedIDs {
						s = strings.ReplaceAll(s, cur, old)
					}
					vals[i] = s
				}
				updates = append(updates, update{key: cells[:len(keys)], vals: vals})
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatalf("regress: read %s: %v", tbl.name, err)
			}
			rows.Close()

			sets := make([]string, len(text))
			for i, c := range text {
				sets[i] = c + " = ?"
			}
			where := make([]string, len(keys))
			for i, k := range keys {
				where[i] = k + " = ?"
			}
			stmt := `UPDATE ` + tbl.name + ` SET ` + strings.Join(sets, ", ") + ` WHERE ` + strings.Join(where, " AND ")
			for _, u := range updates {
				if _, err := db.Exec(stmt, append(append([]any{}, u.vals...), u.key...)...); err != nil {
					t.Fatalf("regress: update %s: %v", tbl.name, err)
				}
			}
		}
	})
}

// textColumns drops the columns that never hold an identifier, so the fixture
// rewriter does not try to string-substitute an integer.
func textColumns(cols []string) []string {
	skip := map[string]bool{"revision": true, "created_at": true, "updated_at": true, "seq": true, "generation": true}
	var out []string
	for _, c := range cols {
		if !skip[c] {
			out = append(out, c)
		}
	}
	return out
}

func primaryKeyColumns(table string) []string {
	switch table {
	case "job_targets":
		return []string{"job_id", "seq"}
	case "pack_rows":
		return []string{"pack_id", "collection", "entity_id"}
	default:
		return []string{"rowid"}
	}
}

// dumpStore reads every row of every table into comparable text — the row-level
// identity this file compares a store against itself with.
func dumpStore(t *testing.T, dsn string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	withRawDB(t, dsn, func(db *sql.DB) {
		for _, tbl := range idMigrationTables {
			rows, err := db.Query(`SELECT ` + strings.Join(tbl.columns, ", ") + ` FROM ` + tbl.name + ` ORDER BY ` + tbl.order)
			if err != nil {
				t.Fatalf("dump: select %s: %v", tbl.name, err)
			}
			var got []string
			for rows.Next() {
				cells := make([]any, len(tbl.columns))
				ptrs := make([]any, len(cells))
				for i := range cells {
					ptrs[i] = &cells[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					rows.Close()
					t.Fatalf("dump: scan %s: %v", tbl.name, err)
				}
				parts := make([]string, len(cells))
				for i, c := range cells {
					parts[i] = fmt.Sprintf("%s=%v", tbl.columns[i], c)
				}
				got = append(got, strings.Join(parts, "|"))
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatalf("dump: read %s: %v", tbl.name, err)
			}
			rows.Close()
			out[tbl.name] = got
		}
	})
	return out
}

// storedIDs is every identifier the eight resource tables hold, sorted.
func storedIDs(t *testing.T, dsn string) []string {
	t.Helper()
	var out []string
	withRawDB(t, dsn, func(db *sql.DB) {
		for _, tbl := range idMigrationTables {
			if tbl.name == "meta" || tbl.name == "jobs" || tbl.name == "job_targets" || tbl.name == "pack_rows" {
				continue
			}
			rows, err := db.Query(`SELECT id FROM ` + tbl.name)
			if err != nil {
				t.Fatalf("ids: select %s: %v", tbl.name, err)
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					t.Fatalf("ids: scan %s: %v", tbl.name, err)
				}
				out = append(out, id)
			}
			rows.Close()
		}
	})
	sort.Strings(out)
	return out
}

// seededStore returns the path of a freshly seeded, canonical store, closed so a
// caller may reopen or rewrite it.
func seededStore(t *testing.T) string {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")
	s, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := s.SeedDemo(context.Background(), seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dsn
}

// TestMigrateRowIDsUnsticksAPreRuleStore is the end-to-end proof. It builds a
// genuine pre-DAT-005a store, shows that a write against it fails BEFORE the
// migration and succeeds after, and shows that the identifiers it lands on are
// exactly the ones a store seeded by the current build carries — so a migrated
// box and a fresh box are the same store, not two dialects.
func TestMigrateRowIDsUnsticksAPreRuleStore(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)

	// The oracle: what a store seeded by the CURRENT build holds. Taken from the
	// production seed path before the fixture is regressed, never from the
	// migration.
	wantIDs := storedIDs(t, dsn)
	wantGeneration := int64(8)

	regressToPreRuleIDs(t, dsn)

	// The fixture really is the older build's store: every identifier that
	// changed is back to its pre-rule spelling, and the ones that never changed
	// are untouched.
	legacy := strings.Join(storedIDs(t, dsn), " ")
	for current, old := range preRuleSeedIDs {
		if current == "01J8Z0DEM00RGANCEST0RB0VND" {
			continue // a dangling parent_id, never a row of its own
		}
		if !strings.Contains(legacy, old) {
			t.Fatalf("fixture is not a pre-rule store: expected legacy id %q among %v", old, legacy)
		}
		if strings.Contains(legacy, current) {
			t.Fatalf("fixture is not a pre-rule store: post-rule id %q still present", current)
		}
	}
	for _, id := range preRuleUnchangedSeedIDs[:1] {
		if !strings.Contains(legacy, id) {
			t.Fatalf("fixture lost the already-canonical site node %q", id)
		}
	}

	// A Job and a pack row that reference the legacy identifiers, as a box that
	// had been used would hold. Both live outside the resource tables and outside
	// every datamodel validator, so nothing but the migration itself can keep
	// them pointing at real rows.
	withRawDB(t, dsn, func(db *sql.DB) {
		if _, err := db.Exec(
			`INSERT INTO jobs (id, created_by, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			"01J9AAAAAAAAAAAAAAAAAAAAAA", "01J9BBBBBBBBBBBBBBBBBBBBBB", 1, 1); err != nil {
			t.Fatalf("insert job: %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO job_targets (job_id, seq, target_id, state, scope_node) VALUES (?, ?, ?, ?, ?)`,
			"01J9AAAAAAAAAAAAAAAAAAAAAA", 0, preRuleSeedIDs["01J8Z6DEM0P1AY11STC0NTENT1"], "succeeded",
			preRuleSeedIDs["01J8Z4DEM0SCREENF1RSTPH0TN"]); err != nil {
			t.Fatalf("insert job target: %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO pack_rows (pack_id, collection, entity_id, revision, lifecycle_state, scope_node, created_at, updated_at, body)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"acme/demo", "widgets", "01J9CCCCCCCCCCCCCCCCCCCCCC", 1, "active",
			preRuleSeedIDs["01J8Z4DEM0SCREENF1RSTPH0TN"], 1, 1, `{}`); err != nil {
			t.Fatalf("insert pack row: %v", err)
		}
		// A durable audit record filed at the legacy screen node. Its scope_node
		// is the ONLY input to a subscriber's visible set (events/1 EVT-120), so
		// one left pointing at a node the migration renamed would still be stored
		// and would match nobody — a permanent audit record (SEC-150) no operator
		// could ever read again.
		if _, err := db.Exec(
			`INSERT INTO events (id, schema, ts, scope_node, trace_id, cost_class, retention_class, origin, origin_principal, payload)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"01J9DDDDDDDDDDDDDDDDDDDDDD", "audit.event", 1, preRuleSeedIDs["01J8Z4DEM0SCREENF1RSTPH0TN"],
			"01J9EEEEEEEEEEEEEEEEEEEEEE", "audit", "audit-long", "internal", "", `{}`); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	})

	s := openStoreAt(t, dsn)

	// BEFORE: the store is write-dead. The playlist being created is itself
	// impeccable; it is rejected because the rows already present are not.
	_, err := s.Create(ctx, store.KindPlaylist, newPlaylistBody(preRuleSeedIDs["01J8Z4DEM0SCREENF1RSTPH0TN"]))
	var ve *store.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Create against a pre-rule store = %v, want a *store.ValidationError", err)
	}
	var sawRowIDInvalid bool
	for _, e := range ve.Errors {
		if e.Code == "ROW_ID_INVALID" {
			sawRowIDInvalid = true
		}
	}
	if !sawRowIDInvalid {
		t.Fatalf("Create against a pre-rule store failed with %+v, want at least one ROW_ID_INVALID", ve.Errors)
	}
	if g := gen(t, s); g != wantGeneration {
		t.Fatalf("a rejected write changed the generation: %d, want %d", g, wantGeneration)
	}

	// THE MIGRATION.
	m, err := s.MigrateRowIDs(ctx)
	if err != nil {
		t.Fatalf("MigrateRowIDs: %v", err)
	}
	if len(m.Rewrites) != len(preRuleSeedIDs) {
		t.Fatalf("MigrateRowIDs rewrote %d id(s) (%+v), want %d", len(m.Rewrites), m.Rewrites, len(preRuleSeedIDs))
	}
	for _, rw := range m.Rewrites {
		want, ok := preRuleSeedIDs[rw.To]
		if !ok {
			t.Errorf("rewrote %q -> %q, which is not a known pre-rule identifier", rw.From, rw.To)
			continue
		}
		if rw.From != want {
			t.Errorf("rewrote %q -> %q, want the source to be %q", rw.From, rw.To, want)
		}
	}

	// AFTER: the same write now succeeds.
	res, err := s.Create(ctx, store.KindPlaylist, newPlaylistBody("01J8Z4DEM0SCREENF1RSTPH0TN"))
	if err != nil {
		t.Fatalf("Create after MigrateRowIDs: %v", err)
	}
	if res.ID != newPlaylistID {
		t.Fatalf("created playlist id = %q, want %q", res.ID, newPlaylistID)
	}

	// The migrated store carries exactly the identifiers a fresh one does.
	gotIDs := storedIDs(t, dsn)
	var migrated []string
	for _, id := range gotIDs {
		if id != newPlaylistID {
			migrated = append(migrated, id)
		}
	}
	if strings.Join(migrated, " ") != strings.Join(wantIDs, " ") {
		t.Fatalf("migrated ids = %v, want the freshly-seeded ids %v", migrated, wantIDs)
	}
	for _, id := range migrated {
		if !ulid.Valid(id) {
			t.Errorf("migrated id %q is not a canonical ULID", id)
		}
	}

	// The site node — the scope node an adopted relay is bound to from outside
	// this store — was already canonical and must not have moved.
	if !strings.Contains(strings.Join(migrated, " "), preRuleUnchangedSeedIDs[0]) {
		t.Fatalf("the site scope node %q is gone after migration; a bound relay would be stranded", preRuleUnchangedSeedIDs[0])
	}

	// Referential integrity, checked field by field rather than only through the
	// validators, so a reference that silently resolved to the wrong row would
	// still be caught.
	nodes, rows, _, generation, err := s.DesiredStateRows(ctx)
	if err != nil {
		t.Fatalf("DesiredStateRows: %v", err)
	}
	if _, errs := datamodel.BuildScopeTree(nodes); len(errs) != 0 {
		t.Fatalf("migrated scope tree failed validation: %+v", errs)
	}
	rowSet, errs := datamodel.ValidateRows(rows)
	if len(errs) != 0 {
		t.Fatalf("migrated scheduling rows failed validation: %+v", errs)
	}
	assertReferencesResolve(t, nodes, rowSet)

	// One generation bump for the migration, one for the create that followed.
	if generation != wantGeneration+2 {
		t.Fatalf("generation after migration + one create = %d, want %d", generation, wantGeneration+2)
	}

	// The Job target, the pack row and the durable event followed their rows.
	withRawDB(t, dsn, func(db *sql.DB) {
		var targetID, targetScope, packScope string
		if err := db.QueryRow(`SELECT target_id, scope_node FROM job_targets`).Scan(&targetID, &targetScope); err != nil {
			t.Fatalf("read job target: %v", err)
		}
		if targetID != "01J8Z6DEM0P1AY11STC0NTENT1" {
			t.Errorf("job target_id = %q, want the migrated playlist id", targetID)
		}
		if targetScope != "01J8Z4DEM0SCREENF1RSTPH0TN" {
			t.Errorf("job target scope_node = %q, want the migrated screen node id", targetScope)
		}
		if err := db.QueryRow(`SELECT scope_node FROM pack_rows`).Scan(&packScope); err != nil {
			t.Fatalf("read pack row: %v", err)
		}
		if packScope != "01J8Z4DEM0SCREENF1RSTPH0TN" {
			t.Errorf("pack row scope_node = %q, want the migrated screen node id", packScope)
		}
		var eventID, eventScope, eventPayload string
		if err := db.QueryRow(`SELECT id, scope_node, payload FROM events`).Scan(&eventID, &eventScope, &eventPayload); err != nil {
			t.Fatalf("read event: %v", err)
		}
		if eventID != "01J9DDDDDDDDDDDDDDDDDDDDDD" {
			t.Errorf("the audit record's own id must not be rewritten; got %q", eventID)
		}
		if eventScope != "01J8Z4DEM0SCREENF1RSTPH0TN" {
			t.Errorf("audit record scope_node = %q, want the migrated screen node id — otherwise it matches no visible set (EVT-120)", eventScope)
		}
		if eventPayload != `{}` {
			t.Errorf("the migration repairs identifiers, it does not edit a recorded fact; payload = %q", eventPayload)
		}
	})
}

// assertReferencesResolve walks every parent/child, schedule, playlist, preset and
// fallback reference and confirms it names a row that is actually present. It
// exists beside the datamodel validators rather than instead of them: those check
// the scheduling-core references, but nothing checks that a row's scope_node names
// a real node, and a migration that renamed a node while leaving its rows behind
// would otherwise pass.
func assertReferencesResolve(t *testing.T, nodes []datamodel.ScopeNode, rs datamodel.RowSet) {
	t.Helper()

	nodeIDs := map[string]bool{}
	for _, n := range nodes {
		nodeIDs[n.ID] = true
	}
	// A parent that is absent is a subtree boundary, not a dangling reference —
	// but a parent that is present must be the node the child was under.
	for _, n := range nodes {
		if n.ParentID != nil && !ulid.Valid(*n.ParentID) {
			t.Errorf("scope node %q has a non-canonical parent_id %q", n.ID, *n.ParentID)
		}
	}

	scheduleIDs := map[string]bool{}
	for _, s := range rs.Schedules {
		scheduleIDs[s.ID] = true
	}
	playlistIDs := map[string]bool{}
	for _, p := range rs.Playlists {
		playlistIDs[p.ID] = true
	}
	presetIDs := map[string]bool{}
	for _, p := range rs.PresetBatches {
		presetIDs[p.PresetID] = true
	}
	fallbackIDs := map[string]bool{}
	for _, f := range rs.Fallbacks {
		fallbackIDs[f.ID] = true
	}

	need := func(what, from, ref string, in map[string]bool) {
		t.Helper()
		if ref == "" {
			return
		}
		if !in[ref] {
			t.Errorf("%s %q references %s %q, which no longer resolves", what, from, what, ref)
		}
	}
	for _, p := range rs.Playlists {
		need("playlist scope_node", p.ID, p.ScopeNode, nodeIDs)
	}
	for _, s := range rs.Schedules {
		need("schedule scope_node", s.ID, s.ScopeNode, nodeIDs)
		need("schedule fallback_id", s.ID, s.FallbackID, fallbackIDs)
	}
	for _, d := range rs.Dayparts {
		need("daypart scope_node", d.ID, d.ScopeNode, nodeIDs)
		need("daypart schedule_id", d.ID, d.ScheduleID, scheduleIDs)
		need("daypart playlist_id", d.ID, d.PlaylistID, playlistIDs)
		need("daypart preset_batch_id", d.ID, d.PresetBatchID, presetIDs)
	}
	for _, v := range rs.ValidityWindows {
		need("validity window scope_node", v.ID, v.ScopeNode, nodeIDs)
		need("validity window schedule_id", v.ID, v.ScheduleID, scheduleIDs)
	}
	for _, f := range rs.Fallbacks {
		need("fallback scope_node", f.ID, f.ScopeNode, nodeIDs)
		need("fallback playlist_id", f.ID, f.PlaylistID, playlistIDs)
	}
	for _, p := range rs.PresetBatches {
		need("preset batch scope_node", p.PresetID, p.ScopeNode, nodeIDs)
	}
}

// TestMigrateRowIDsRewritesTheAutomationBodyIdentity pins the one kind whose
// identifier lives in two places at once: the automations table's id column and
// the `id` key inside the rule body the compiler reads. A migration that moved
// only the column would leave a rule whose body names a rule that no longer
// exists.
func TestMigrateRowIDsRewritesTheAutomationBodyIdentity(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)

	const legacyRuleID = "01J8Z3K4N5P6Q7R8S9TOVIAT02"
	const wantRuleID = "01J8Z3K4N5P6Q7R8S9T0V1AT02"

	// A second automation, written the way a pre-rule build could: straight into
	// the table, past the store's validators.
	withRawDB(t, dsn, func(db *sql.DB) {
		body := fmt.Sprintf(
			`{"id":%q,"mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1SCRN","to":["on"]}],`+
				`"conditions":[],"actions":[{"type":"preset_batch","preset_id":%q}],"revision":1,"created_at":1,"updated_at":1}`,
			legacyRuleID, "01J8Z8DEMOPRESETBATCHFIRE1")
		if _, err := db.Exec(
			`INSERT INTO automations (id, revision, external_id, labels, scope_node, created_at, updated_at, body, execution_class)
			 VALUES (?, ?, '', '{}', ?, 1, 1, ?, 'app')`,
			legacyRuleID, 1, "01J8Z4DEMOSCREENFIRSTPHOTN", body); err != nil {
			t.Fatalf("insert legacy automation: %v", err)
		}
	})
	regressToPreRuleIDs(t, dsn)

	s := openStoreAt(t, dsn)
	if _, err := s.MigrateRowIDs(ctx); err != nil {
		t.Fatalf("MigrateRowIDs: %v", err)
	}

	withRawDB(t, dsn, func(db *sql.DB) {
		var id, scopeNode, body string
		if err := db.QueryRow(`SELECT id, scope_node, body FROM automations WHERE id = ?`, wantRuleID).
			Scan(&id, &scopeNode, &body); err != nil {
			t.Fatalf("read migrated automation %q: %v", wantRuleID, err)
		}
		var decoded struct {
			ID      string `json:"id"`
			Actions []struct {
				PresetID string `json:"preset_id"`
			} `json:"actions"`
		}
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("migrated automation body is not JSON: %v", err)
		}
		if decoded.ID != wantRuleID {
			t.Errorf("automation body id = %q, want %q", decoded.ID, wantRuleID)
		}
		if scopeNode != "01J8Z4DEM0SCREENF1RSTPH0TN" {
			t.Errorf("automation scope_node = %q, want the migrated screen node id", scopeNode)
		}
		// A rule action reaching into the scheduling core by preset_id is a
		// reference no scheduling validator inspects; the value-matching rewrite
		// is what keeps it pointing at the preset batch.
		if len(decoded.Actions) != 1 || decoded.Actions[0].PresetID != "01J8Z8DEM0PRESETBATCHF1RE1" {
			t.Errorf("automation action preset_id = %+v, want the migrated preset batch id", decoded.Actions)
		}
	})
}

// TestMigrateRowIDsIsIdempotent runs the migration twice over the same store: the
// second run must find nothing to do and change nothing.
func TestMigrateRowIDsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)
	regressToPreRuleIDs(t, dsn)

	s := openStoreAt(t, dsn)
	first, err := s.MigrateRowIDs(ctx)
	if err != nil {
		t.Fatalf("first MigrateRowIDs: %v", err)
	}
	if len(first.Rewrites) == 0 {
		t.Fatal("first MigrateRowIDs rewrote nothing; the fixture is not a pre-rule store")
	}
	after := dumpStore(t, dsn)

	second, err := s.MigrateRowIDs(ctx)
	if err != nil {
		t.Fatalf("second MigrateRowIDs: %v", err)
	}
	if len(second.Rewrites) != 0 {
		t.Fatalf("second MigrateRowIDs rewrote %+v, want nothing", second.Rewrites)
	}
	assertSameStore(t, after, dumpStore(t, dsn))
}

// TestMigrateRowIDsLeavesAConformingStoreUntouched is the safety claim that lets
// this run unattended at every boot: a store that already conforms is not written
// to at all — not a row, not the generation, not even a commit hook, which would
// otherwise nudge every connected relay on every restart for no reason.
func TestMigrateRowIDsLeavesAConformingStoreUntouched(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)
	before := dumpStore(t, dsn)

	s := openStoreAt(t, dsn)
	var hookFired int
	s.OnCommit(func() { hookFired++ })

	m, err := s.MigrateRowIDs(ctx)
	if err != nil {
		t.Fatalf("MigrateRowIDs: %v", err)
	}
	if len(m.Rewrites) != 0 {
		t.Fatalf("MigrateRowIDs rewrote %+v on a conforming store, want nothing", m.Rewrites)
	}
	if hookFired != 0 {
		t.Fatalf("the commit hook fired %d time(s) on a conforming store, want 0", hookFired)
	}
	assertSameStore(t, before, dumpStore(t, dsn))

	// And the read-only report agrees with the migration that ran.
	plan, err := s.PlanRowIDMigration(ctx)
	if err != nil {
		t.Fatalf("PlanRowIDMigration: %v", err)
	}
	if len(plan.Rewrites) != 0 {
		t.Fatalf("PlanRowIDMigration on a conforming store = %+v, want nothing", plan.Rewrites)
	}
}

// TestMigrateRowIDsRefusesAnIdentifierItCannotFold: an identifier that is not a
// near-miss ULID cannot have come from the seed or from any api/1 write, so the
// migration stops rather than inventing one — and stopping leaves the store byte
// for byte as it was, including the rewrites it had already computed for the
// rows around it.
func TestMigrateRowIDsRefusesAnIdentifierItCannotFold(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)
	regressToPreRuleIDs(t, dsn)
	withRawDB(t, dsn, func(db *sql.DB) {
		if _, err := db.Exec(`UPDATE playlists SET id = ?, body = replace(body, ?, ?) WHERE id = ?`,
			"demo-playlist", `"01J8Z6DEMOPLAYLISTCONTENT1"`, `"demo-playlist"`, "01J8Z6DEMOPLAYLISTCONTENT1"); err != nil {
			t.Fatalf("plant unfoldable id: %v", err)
		}
	})
	before := dumpStore(t, dsn)

	s := openStoreAt(t, dsn)
	_, err := s.MigrateRowIDs(ctx)
	var blocked *store.IDMigrationBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("MigrateRowIDs = %v, want an *store.IDMigrationBlockedError", err)
	}
	var named bool
	for _, b := range blocked.Blocked {
		if b.ID == "demo-playlist" && b.Kind == store.KindPlaylist {
			named = true
		}
	}
	if !named {
		t.Errorf("blocked report %+v does not name the offending playlist id", blocked.Blocked)
	}
	assertSameStore(t, before, dumpStore(t, dsn))
}

// TestMigrateRowIDsRefusesAFoldThatCollides: two identifiers that differ only in
// characters the fold erases would land on one row. Merging them would destroy
// one of the two, so the migration refuses instead.
func TestMigrateRowIDsRefusesAFoldThatCollides(t *testing.T) {
	ctx := context.Background()
	dsn := seededStore(t)
	withRawDB(t, dsn, func(db *sql.DB) {
		// Two playlists whose ids differ only by I-versus-L and O-versus-0 —
		// both fold onto the identifier the seeded playlist already holds.
		for _, id := range []string{"01J8Z6DEMOPLAYLISTCONTENT1", "01J8Z6DEMOPLAYIISTCONTENT1"} {
			if _, err := db.Exec(
				`INSERT INTO playlists (id, revision, external_id, labels, scope_node, created_at, updated_at, body)
				 VALUES (?, 1, '', '{}', ?, 1, 1, ?)`,
				id, "01J8Z4DEM0SCREENF1RSTPH0TN",
				fmt.Sprintf(`{"id":%q,"scope_node":"01J8Z4DEM0SCREENF1RSTPH0TN","name":"Collides","items":[],"revision":1,"created_at":1,"updated_at":1}`, id),
			); err != nil {
				t.Fatalf("plant colliding playlist %q: %v", id, err)
			}
		}
	})
	before := dumpStore(t, dsn)

	s := openStoreAt(t, dsn)
	_, err := s.MigrateRowIDs(ctx)
	var blocked *store.IDMigrationBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("MigrateRowIDs = %v, want an *store.IDMigrationBlockedError", err)
	}
	if len(blocked.Blocked) == 0 {
		t.Fatal("blocked report is empty")
	}
	for _, b := range blocked.Blocked {
		if !strings.Contains(b.Reason, "already holds") {
			t.Errorf("blocked %+v, want a collision reason", b)
		}
	}
	assertSameStore(t, before, dumpStore(t, dsn))
}

func assertSameStore(t *testing.T, want, got map[string][]string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("table count changed: %d -> %d", len(want), len(got))
	}
	for table, wantRows := range want {
		gotRows, ok := got[table]
		if !ok {
			t.Errorf("table %s disappeared", table)
			continue
		}
		if len(wantRows) != len(gotRows) {
			t.Errorf("%s: row count %d -> %d", table, len(wantRows), len(gotRows))
			continue
		}
		for i := range wantRows {
			if wantRows[i] != gotRows[i] {
				t.Errorf("%s row %d changed:\n  before %s\n  after  %s", table, i, wantRows[i], gotRows[i])
			}
		}
	}
}
