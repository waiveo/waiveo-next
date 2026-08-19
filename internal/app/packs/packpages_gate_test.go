package packs

// THE SHIPPED-PAGE MANIFEST GATE — the Go half of the authoring gate #205 asked
// for.
//
// web/src/renderer/pack-pages.gate.test.ts proves every shipped page document is
// valid ui-schema/1 by driving the renderer's OWN validatePage. That ends the
// class of defect #205 reported: a page the RENDERER refuses.
//
// It cannot end the neighbouring class, because the remaining authorities live
// HERE, in Go, and a TypeScript test cannot reach them. A page can be flawless
// ui-schema/1 and still be refused — at install, at save, or at the moment an
// operator presses a button — because it disagrees with the MANIFEST shipped
// beside it. Three seams, three different refusals, none of them visible to a
// validator that reads the page document alone:
//
//	1. MAN-064  a settings-form `source` naming no singleton collection
//	            → INSTALL refuses the artifact: 422 SETTINGS_SOURCE_NOT_SINGLETON
//	              (collectBundleFiles → checkSettingsSource). The page never
//	              reaches the box at all.
//	2. MAN-051  a `bind` naming a field the collection does not declare
//	            → the page PAINTS; the operator fills it in; Save answers
//	              422 UNKNOWN_FIELD (ValidateRowWrite). Nothing persists.
//	3. MAN-100  a `call-action` naming an action the manifest does not declare
//	            → the page PAINTS; the button answers 404 "This pack declares no
//	              action of that name." (api.declaredAction).
//
// 2 and 3 are the worse pair. The console shows a live, correct-looking page and
// the ONLY detector is an operator pressing the control — which is precisely
// #205's shape (a human at the last step), reproduced at the other host-side
// validators of the same file. A one-character typo in any of the four discovery
// policy controls lands there.
//
// AUTHORITY BY CONSTRUCTION, NOT BY RESEMBLANCE — the same discipline the web
// gate states, for the same reason: a second validator that diverges from the
// host's is worse than no gate. This file is an IN-PACKAGE test (`package packs`,
// not `packs_test`) for exactly one reason — it calls `checkSettingsSource`
// directly, the very function collectBundleFiles calls at install, so MAN-064
// cannot drift between this gate and the installer. The field check drives the
// exported `ValidateRowWrite`, the same function the pack-data write path calls,
// and reads only its UNKNOWN_FIELD verdict. The action check is a name lookup
// over the same `manifest.Action` slice `api.declaredAction` reads. Nothing here
// restates a rule, and nothing here invents one: where the host has no check —
// a list-detail `detail.source` naming an undeclared collection, which no host
// component refuses — this gate is deliberately silent rather than the first
// component in the system to have an opinion.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/manifest"
)

// packIDPattern mirrors PACK_ID in the web gate — the shared discriminator that
// tells a pack manifest from every other manifest.json in the tree.
var packIDPattern = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9-]+$`)

// gatePack is one discovered pack: where it lives, what it declares, and the
// page documents it ships.
type gatePack struct {
	dir   string // repo-relative
	id    string
	man   manifest.PackManifest
	pages []gatePage
}

type gatePage struct {
	file     string // repo-relative path of the page document
	path     string // the manifest's ui.pages[].path
	pageType string
	doc      []byte
}

// gateRepoRoot walks up from the package directory to the repo root, anchored on
// repo-level facts (go.mod + contracts/) rather than on any one pack — so moving
// or deleting a pack cannot silently repoint the root. Mirrors repoRoot() in the
// web gate.
func gateRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("pack-manifest-gate: getwd: %v", err)
	}
	for {
		_, goMod := os.Stat(filepath.Join(dir, "go.mod"))
		_, contracts := os.Stat(filepath.Join(dir, "contracts"))
		if goMod == nil && contracts == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("pack-manifest-gate: cannot locate the repo root (go.mod + contracts/)")
		}
		dir = parent
	}
}

// gateTrackedFiles lists tracked files. SCOPE IS DERIVED, NEVER LISTED — a
// hard-coded file list is the bug this gate exists to prevent, so a pack added
// tomorrow in a directory nobody has created yet is swept automatically.
// tracked-only excludes build output and .claude/worktrees with no maintained
// exclusion list. Same source, and therefore the same scope, as the web gate.
func gateTrackedFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("pack-manifest-gate: git ls-files: %v", err)
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

// discoverGatePacks finds every pack in the repo the same way the web gate does:
// a directory with a manifest.json declaring a publisher/name `id` and a
// `compat` object. That discriminator rejects conformance/driven-manifest.json
// (a contract-driver index, not a pack) without naming it.
func discoverGatePacks(t *testing.T) (string, []gatePack) {
	t.Helper()
	root := gateRepoRoot(t)
	var packs []gatePack
	for _, file := range gateTrackedFiles(t, root) {
		if filepath.Base(file) != "manifest.json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			continue
		}
		var probe struct {
			ID     string          `json:"id"`
			Compat json.RawMessage `json:"compat"`
		}
		if json.Unmarshal(raw, &probe) != nil {
			continue
		}
		// The same discriminator the web gate uses, so the two gates provably
		// sweep the SAME set: a publisher/name id and a `compat` object. It
		// rejects conformance/driven-manifest.json (a contract-driver index, not
		// a pack) without naming it.
		if !packIDPattern.MatchString(probe.ID) || len(probe.Compat) == 0 {
			continue
		}
		var m manifest.PackManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("pack-manifest-gate: %s declares id %q but does not decode as a manifest: %v", file, probe.ID, err)
		}
		dir := filepath.Dir(file)
		p := gatePack{dir: dir, id: probe.ID, man: m}
		for _, page := range m.UI.Pages {
			rel := filepath.Join(dir, filepath.FromSlash(pageDocPath(page.Path)))
			doc, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				// UIS-001 — the web gate owns this assertion too; failing here as
				// well keeps this gate from silently skipping a page.
				t.Errorf("pack-manifest-gate: %s declares page %q but %s is unreadable: %v", probe.ID, page.Path, rel, err)
				continue
			}
			p.pages = append(p.pages, gatePage{
				file:     filepath.ToSlash(rel),
				path:     page.Path,
				pageType: page.PageType,
				doc:      doc,
			})
		}
		packs = append(packs, p)
	}
	sort.Slice(packs, func(i, j int) bool { return packs[i].dir < packs[j].dir })
	return root, packs
}

// TestShippedPagesDiscovered — a gate that silently matches nothing is not a
// gate. If discovery ever breaks (a moved directory, a renamed manifest, a git
// invocation that returns nothing) this fails loudly rather than reporting a
// green zero.
func TestShippedPagesDiscovered(t *testing.T) {
	_, packs := discoverGatePacks(t)
	if len(packs) == 0 {
		t.Fatal("pack-manifest-gate: discovered no packs at all — the sweep is broken, not the repo clean")
	}
	pages := 0
	for _, p := range packs {
		pages += len(p.pages)
	}
	if pages == 0 {
		t.Fatal("pack-manifest-gate: discovered no page documents at all — the sweep is broken, not the repo clean")
	}
	t.Logf("pack-manifest-gate: %d packs, %d shipped page documents", len(packs), pages)
}

// TestShippedSettingsFormsBindADeclaredSingleton drives the INSTALLER'S OWN
// MAN-064 check over every settings-form page in the repo.
//
// Until this existed nothing installed the first-party artifacts: only the
// example packs reached the installer (internal/app/api/packs_example_e2e_test.go),
// and extensions/discovery_test.go asserted merely that ui/settings.json EXISTS.
// So a discovery settings page whose `source` named no singleton collection
// passed `npm run check`, passed `go test ./...`, and was refused with 422
// SETTINGS_SOURCE_NOT_SINGLETON the moment the pack was built, signed and
// POSTed — green CI, and the owner's only UI surface never reaching the box.
func TestShippedSettingsFormsBindADeclaredSingleton(t *testing.T) {
	_, packs := discoverGatePacks(t)
	checked := 0
	for _, p := range packs {
		for _, page := range p.pages {
			if page.pageType != settingsFormPageType {
				continue
			}
			checked++
			// THE authority: the same function collectBundleFiles calls at
			// install (packs.go), not a resemblance of it.
			if err := checkSettingsSource(&p.man, page.doc, page.path); err != nil {
				t.Errorf("pack-manifest-gate: %s (%s): %v\n"+
					"    INSTALL REFUSES THIS PACK — the page never reaches the box.", page.file, p.id, err)
			}
		}
	}
	if checked == 0 {
		t.Fatal("pack-manifest-gate: no settings-form page was checked — the sweep is broken")
	}
}

// TestShippedPagesNameDeclaredFieldsAndActions closes the two seams that let a
// page RENDER and then refuse the operator's actual work.
func TestShippedPagesNameDeclaredFieldsAndActions(t *testing.T) {
	_, packs := discoverGatePacks(t)
	fields, actions := 0, 0

	for _, p := range packs {
		declaredActions := map[string]bool{}
		for _, a := range p.man.Actions {
			declaredActions[a.Name] = true
		}
		collections := map[string]manifest.Collection{}
		for _, c := range p.man.DataModel.Collections {
			collections[c.Name] = c
		}

		for _, page := range p.pages {
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(page.doc, &doc); err != nil {
				t.Errorf("pack-manifest-gate: %s is not a JSON object: %v", page.file, err)
				continue
			}

			// ── MAN-100: every call-action names a declared action ──────────
			// The same set lookup api.declaredAction performs, over the same
			// manifest.Action slice. A miss is 404 at the button.
			found := collectCallActions(page.doc)
			// An INDEPENDENT oracle for the structured walk above: every
			// `call-action` in the document is a literal `"call-action"` in its
			// bytes, and this walk is exhaustive (whole document, no scope
			// filtering), so the two counts must agree exactly. Without this a
			// walk that silently stopped matching — a renamed JSON tag, a
			// restructured ActionRef — would pass every page by finding nothing
			// to check, which is the "green because it looked at nothing" failure
			// this gate exists to prevent. Content-derived, so it stays correct
			// as pages are added or removed.
			if raw := strings.Count(string(page.doc), `"call-action"`); raw != len(found) {
				t.Errorf("pack-manifest-gate: %s: the ActionRef walk found %d call-action(s) but the document contains %d — the walk has stopped matching and this gate is checking nothing",
					page.file, len(found), raw)
			}
			for _, ref := range found {
				actions++
				if !declaredActions[ref] {
					t.Errorf("pack-manifest-gate: %s (%s): call-action names %q, which this manifest declares no action for (MAN-100).\n"+
						"    The page RENDERS and the button answers 404 \"This pack declares no action of that name.\"\n"+
						"    declared: %v", page.file, p.id, ref, sortedKeys(declaredActions))
				}
			}

			// ── MAN-051: every record-scoped bind names a declared field ────
			coll, name, ok := pageRecordCollection(doc, page.pageType, collections)
			if !ok {
				// No resolvable collection. For a settings-form that is MAN-064's
				// verdict, reported by the test above; for a list-detail no host
				// component refuses it, so this gate says nothing rather than
				// becoming the first one with an opinion.
				continue
			}
			for _, bind := range recordBinds(doc, page.pageType) {
				root, isField := bindRootField(bind)
				if !isField {
					continue // a reserved root ($ui/$context/$root/$params/item) is not a record field
				}
				fields++
				// THE authority: the exported ValidateRowWrite, the same function
				// the pack-data write path calls. A JSON null isolates the
				// declared-ness question from the type question — data.go reports
				// UNKNOWN_FIELD before it ever type-checks a value — so only the
				// UNKNOWN_FIELD verdict is read here.
				if !fieldIsDeclared(coll, root) {
					t.Errorf("pack-manifest-gate: %s (%s): bind %q roots at %q, which collection %q does not declare (MAN-051).\n"+
						"    The page RENDERS, the operator fills it in, and Save answers 422 UNKNOWN_FIELD — nothing persists.\n"+
						"    declared: %v", page.file, p.id, bind, root, name, fieldNames(coll))
				}
			}

			// A `create` newAction seeds a draft from itemDefault and `submit`
			// persists it down the same write path, so an undeclared key there is
			// the identical 422 with an extra click in front of it.
			for _, seed := range createSeedFields(doc, collections) {
				fields++
				if !fieldIsDeclared(seed.coll, seed.field) {
					t.Errorf("pack-manifest-gate: %s (%s): newAction itemDefault seeds %q, which collection %q does not declare (MAN-051).\n"+
						"    The draft opens and Save answers 422 UNKNOWN_FIELD.\n"+
						"    declared: %v", page.file, p.id, seed.field, seed.collName, fieldNames(seed.coll))
				}
			}
		}
	}

	// The bind walk gets a floor rather than an exact oracle: it deliberately
	// filters by Scope (a `repeat`'s itemTemplate is excluded, see recordBinds),
	// so the raw count of `"bind"` in the bytes is legitimately higher and only a
	// total zero is diagnostic. Every settings-form in the repo binds its
	// collection's fields; sweeping none means the walk died, not that the pages
	// stopped editing anything.
	if fields == 0 {
		t.Fatal("pack-manifest-gate: swept 0 record binds across every shipped page — the walk has stopped matching, not the repo gone clean")
	}
	t.Logf("pack-manifest-gate: checked %d record binds and %d call-actions", fields, actions)
}

// fieldIsDeclared asks the REAL pack-data write gate whether a collection
// declares a field, by offering it that one field as a null-valued write.
// ValidateRowWrite reports UNKNOWN_FIELD before it type-checks anything and
// treats a JSON null as clearing the field, so the answer isolates exactly the
// declared-ness question and nothing else. Envelope and host-owned keys
// (scope_node, entity_id, …) are skipped by that same function, so a bind naming
// one is correctly not an undeclared field here either.
func fieldIsDeclared(c manifest.Collection, field string) bool {
	body, err := json.Marshal(map[string]any{field: nil})
	if err != nil {
		return false
	}
	_, errs := ValidateRowWrite(c, body)
	for _, e := range errs {
		if e.Field == field && e.Code == "UNKNOWN_FIELD" {
			return false
		}
	}
	return true
}

func fieldNames(c manifest.Collection) []string {
	out := make([]string, 0, len(c.Fields))
	for _, f := range c.Fields {
		out = append(out, f.Name)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pageRecordCollection resolves the collection whose record a page's own
// record-scoped binds address: a settings-form's `source` (UIS-030), or a
// list-detail's `detail.source` (UIS-020). A root Binding may carry a predicate
// or REST-ish segments ("menu_items[entity_id=$ui.selected]"), so the collection
// is the leading name of the first "/"-part.
func pageRecordCollection(doc map[string]json.RawMessage, pageType string, collections map[string]manifest.Collection) (manifest.Collection, string, bool) {
	var rawSource json.RawMessage
	switch pageType {
	case settingsFormPageType:
		rawSource = doc["source"]
	case "list-detail":
		var detail map[string]json.RawMessage
		if json.Unmarshal(doc["detail"], &detail) != nil {
			return manifest.Collection{}, "", false
		}
		rawSource = detail["source"]
	default:
		return manifest.Collection{}, "", false
	}
	var s string
	if json.Unmarshal(rawSource, &s) != nil || s == "" {
		return manifest.Collection{}, "", false // a LiveBinding/paginated source, or absent — ui-schema/1 owns the shape
	}
	name := s
	if i := strings.IndexAny(name, "/"); i >= 0 {
		name = name[:i]
	}
	if i := strings.IndexByte(name, '['); i >= 0 {
		name = name[:i]
	}
	c, ok := collections[name]
	return c, name, ok
}

// bindRootField reduces a Binding to the record field it roots at. A reserved
// root ($root/$ui/$context/$params/item, UIS-100/102) addresses something other
// than the record and is not a field.
func bindRootField(bind string) (string, bool) {
	root := bind
	if i := strings.IndexAny(root, ".["); i >= 0 {
		root = root[:i]
	}
	if root == "" || strings.HasPrefix(root, "$") || root == "item" {
		return "", false
	}
	return root, true
}

// recordBinds collects the binds whose Scope is unambiguously the page's own
// record: a settings-form's section fields (UIS-030) and a list-detail's
// detail.root subtree (UIS-020).
//
// It deliberately does NOT descend into a `repeat`'s `itemTemplate`, where Scope
// narrows to an array element (UIS-107) and a bind names an element member
// rather than a collection field — checking those against the collection would
// be a false positive. A `repeat`'s OWN bind names the array and IS collected.
// Page-level `fragments` are skipped for the same reason: a fragment's Scope is
// wherever it is referenced from, which is not statically one record here.
func recordBinds(doc map[string]json.RawMessage, pageType string) []string {
	var out []string
	switch pageType {
	case settingsFormPageType:
		var sections []struct {
			Fields []json.RawMessage `json:"fields"`
		}
		if json.Unmarshal(doc["sections"], &sections) != nil {
			return nil
		}
		for _, s := range sections {
			for _, f := range s.Fields {
				collectBinds(f, &out)
			}
		}
	case "list-detail":
		var detail map[string]json.RawMessage
		if json.Unmarshal(doc["detail"], &detail) != nil {
			return nil
		}
		collectBinds(detail["root"], &out)
	}
	return out
}

// collectBinds walks one widget subtree in a single Scope, collecting `bind`
// strings. It descends `children` and a `switch`'s `cases[].render` (same
// Scope), and stops at `props.itemTemplate` (narrowed Scope, see recordBinds).
func collectBinds(raw json.RawMessage, out *[]string) {
	if len(raw) == 0 {
		return
	}
	var node struct {
		Bind     json.RawMessage   `json:"bind"`
		Children []json.RawMessage `json:"children"`
		Props    struct {
			Cases []struct {
				Render json.RawMessage `json:"render"`
			} `json:"cases"`
		} `json:"props"`
	}
	if json.Unmarshal(raw, &node) != nil {
		return
	}
	var bind string
	if json.Unmarshal(node.Bind, &bind) == nil && bind != "" {
		*out = append(*out, bind)
	}
	for _, c := range node.Children {
		collectBinds(c, out)
	}
	for _, c := range node.Props.Cases {
		collectBinds(c.Render, out)
	}
}

type createSeed struct {
	coll     manifest.Collection
	collName string
	field    string
}

// createSeedFields reads a list-detail `newAction`'s create seed (UIS-164): the
// itemDefault keys are written verbatim down the create path, so each must be a
// field the target collection declares.
func createSeedFields(doc map[string]json.RawMessage, collections map[string]manifest.Collection) []createSeed {
	var action struct {
		Verb        string                     `json:"verb"`
		Target      string                     `json:"target"`
		ItemDefault map[string]json.RawMessage `json:"itemDefault"`
	}
	if len(doc["newAction"]) == 0 || json.Unmarshal(doc["newAction"], &action) != nil {
		return nil
	}
	if action.Verb != "create" || action.Target == "" {
		return nil
	}
	name := action.Target
	if i := strings.IndexAny(name, "/["); i >= 0 {
		name = name[:i]
	}
	c, ok := collections[name]
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(action.ItemDefault))
	for k := range action.ItemDefault {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]createSeed, 0, len(keys))
	for _, k := range keys {
		out = append(out, createSeed{coll: c, collName: name, field: k})
	}
	return out
}

// collectCallActions collects every `call-action` ActionRef's `action` name
// anywhere in a page document — a generic walk, so it finds them wherever they
// sit (a button's on.press, a table's rowPress, a wizard's onFinish) without
// encoding where those places are.
func collectCallActions(raw json.RawMessage) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if t["verb"] == "call-action" {
				if name, ok := t["action"].(string); ok {
					out = append(out, name)
				}
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	walk(doc)
	return out
}
