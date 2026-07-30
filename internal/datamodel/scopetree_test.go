package datamodel

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// ptrStr / ptrF64 build the nullable geo/parent pointers a ScopeNode carries.
func ptrStr(s string) *string   { return &s }
func ptrF64(f float64) *float64 { return &f }

// DAT-001: a complete org -> site -> group -> screen tree validates, and a device
// row + a screen resource row both placed under the GROUP node (one level above
// the tree's own screen-kind leaf) resolve to attachment kind "group" — proving
// DAT-004 attachment is independent of a node's own kind. Wired to the named case.
func TestDAT001ValidScopeNodeTree(t *testing.T) {
	b, err := os.ReadFile(corpusPath("DAT-001-valid-scope-node-tree.json"))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c struct {
		Input struct {
			ScopeNodes []ScopeNode `json:"scope_nodes"`
			Device     struct {
				ScopeNode string `json:"scope_node"`
			} `json:"device"`
			ScreenResource struct {
				ScopeNode string `json:"scope_node"`
			} `json:"screen_resource"`
		} `json:"input"`
		Expected struct {
			Valid                        bool   `json:"valid"`
			DeviceAttachmentKind         string `json:"device_attachment_kind"`
			ScreenResourceAttachmentKind string `json:"screen_resource_attachment_kind"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("unmarshal corpus: %v", err)
	}

	tree, errs := BuildScopeTree(c.Input.ScopeNodes)
	if len(errs) != 0 {
		t.Fatalf("expected valid tree, got errors: %+v", errs)
	}

	if k, ok := tree.KindOf(c.Input.Device.ScopeNode); !ok || k != c.Expected.DeviceAttachmentKind {
		t.Errorf("device attachment kind = %q,%v; want %q", k, ok, c.Expected.DeviceAttachmentKind)
	}
	if k, ok := tree.KindOf(c.Input.ScreenResource.ScopeNode); !ok || k != c.Expected.ScreenResourceAttachmentKind {
		t.Errorf("screen-resource attachment kind = %q,%v; want %q", k, ok, c.Expected.ScreenResourceAttachmentKind)
	}
}

// DAT-033: a site declares the required geo; a group beneath it with no override
// resolves to the site's own values (source ancestor-site:<siteID>); a screen
// beneath the same group with its own override resolves to its own values
// (source own). All three columns resolve together from ONE node. Wired to case.
func TestDAT033ScreenTZOverride(t *testing.T) {
	b, err := os.ReadFile(corpusPath("DAT-033-valid-screen-tz-override.json"))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c struct {
		Input struct {
			ScopeNodes []ScopeNode `json:"scope_nodes"`
		} `json:"input"`
		Expected struct {
			Valid        bool `json:"valid"`
			EffectiveGeo map[string]struct {
				TZ     string  `json:"tz"`
				Lat    float64 `json:"lat"`
				Long   float64 `json:"long"`
				Source string  `json:"source"`
			} `json:"effective_geo"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("unmarshal corpus: %v", err)
	}

	tree, errs := BuildScopeTree(c.Input.ScopeNodes)
	if len(errs) != 0 {
		t.Fatalf("expected valid tree, got errors: %+v", errs)
	}

	for nodeID, want := range c.Expected.EffectiveGeo {
		got, e := tree.EffectiveGeo(nodeID)
		if e != nil {
			t.Fatalf("EffectiveGeo(%s): unexpected error %v", nodeID, e)
		}
		if got.TZ != want.TZ || got.Lat != want.Lat || got.Long != want.Long || got.Source != want.Source {
			t.Errorf("EffectiveGeo(%s) = %+v; want tz=%s lat=%v long=%v source=%s",
				nodeID, got, want.TZ, want.Lat, want.Long, want.Source)
		}
		// The free-function form must agree on the three columns (plan signature).
		tz, lat, long, ferr := EffectiveTZ(tree, nodeID)
		if ferr != nil {
			t.Fatalf("EffectiveTZ(%s): unexpected error %v", nodeID, ferr)
		}
		if tz != want.TZ || lat != want.Lat || long != want.Long {
			t.Errorf("EffectiveTZ(%s) = %s,%v,%v; want %s,%v,%v", nodeID, tz, lat, long, want.TZ, want.Lat, want.Long)
		}
	}
}

// A group's own override MUST NOT leak to a descendant: DAT-033 resolves a
// non-declaring node to the nearest ancestor SITE-kind node, not to an
// intervening group override. site(geoA) -> group(geoB override) -> screen(none)
// therefore resolves screen to geoA / ancestor-site, never geoB.
func TestEffectiveGeoWalksToNearestSiteNotIntermediateGroup(t *testing.T) {
	const (
		site1ID = "01JS1TE0NENDVGH0AVNH9DN6R0"
		grp1ID  = "01JGRP0NENDVPG2X7R5JQC42EJ"
		scr1ID  = "01JSCR0NENDV6RHXQAQPDH4AFA"
	)
	nodes := []ScopeNode{
		{ID: site1ID, Kind: "site", ParentID: ptrStr("01J0RG0NENDVPG2X7R5JQC42EJ"), Name: "Site One", TZ: ptrStr("America/Denver"), Lat: ptrF64(39.7392), Long: ptrF64(-104.9903)},
		{ID: grp1ID, Kind: "group", ParentID: ptrStr(site1ID), Name: "Group One", TZ: ptrStr("America/New_York"), Lat: ptrF64(40.7128), Long: ptrF64(-74.0060)},
		{ID: scr1ID, Kind: "screen", ParentID: ptrStr(grp1ID), Name: "Screen One"},
	}
	tree, errs := BuildScopeTree(nodes)
	if len(errs) != 0 {
		t.Fatalf("unexpected build errors: %+v", errs)
	}
	// group resolves to its own override.
	if g, e := tree.EffectiveGeo(grp1ID); e != nil || g.TZ != "America/New_York" || g.Source != "own" {
		t.Errorf("group EffectiveGeo = %+v, err=%v; want own America/New_York", g, e)
	}
	// screen with no override skips the group override and lands on the site.
	g, e := tree.EffectiveGeo(scr1ID)
	if e != nil {
		t.Fatalf("screen EffectiveGeo error: %v", e)
	}
	if g.TZ != "America/Denver" || g.Lat != 39.7392 || g.Long != -104.9903 {
		t.Errorf("screen EffectiveGeo = %+v; want site geo America/Denver", g)
	}
	if g.Source != "ancestor-site:"+site1ID {
		t.Errorf("screen source = %q; want ancestor-site:%s", g.Source, site1ID)
	}
}

// DAT-034: an unresolvable node (no site in its ancestor chain among the provided
// nodes) MUST surface as an error — resolution NEVER substitutes box-local state.
func TestEffectiveTZUnresolvableErrors(t *testing.T) {
	// A group subtree with no site ancestor present (parent chain runs off the set).
	nodes := []ScopeNode{
		{ID: "grpA", Kind: "group", ParentID: ptrStr("grpB")},
		{ID: "grpB", Kind: "group", ParentID: ptrStr("absent-root")},
	}
	tree, _ := BuildScopeTree(nodes)
	if _, _, _, err := EffectiveTZ(tree, "grpA"); err == nil {
		t.Fatalf("expected an error resolving a node with no site ancestor; got nil (box-local fallback forbidden, DAT-034)")
	}
	if _, e := tree.EffectiveGeo("missing-node"); e == nil {
		t.Fatalf("expected an error for an unknown node id")
	}
}

// DAT-001/002/003/031/032: structural validation surfaces the taxonomy codes.
func TestScopeTreeValidationRejects(t *testing.T) {
	cases := []struct {
		name  string
		nodes []ScopeNode
		code  string
	}{
		{
			name:  "invalid kind",
			nodes: []ScopeNode{{ID: "n1", Kind: "region", ParentID: ptrStr("org1")}},
			code:  "SCOPE_NODE_KIND_INVALID",
		},
		{
			name:  "non-org null parent",
			nodes: []ScopeNode{{ID: "s1", Kind: "site", ParentID: nil, TZ: ptrStr("America/Denver"), Lat: ptrF64(1), Long: ptrF64(2)}},
			code:  "SCOPE_NODE_PARENT_INVALID",
		},
		{
			name:  "org with non-null parent",
			nodes: []ScopeNode{{ID: "o1", Kind: "org", ParentID: ptrStr("x")}},
			code:  "SCOPE_NODE_PARENT_INVALID",
		},
		{
			name: "parent-kind table violation (screen parents a group)",
			nodes: []ScopeNode{
				{ID: "scr", Kind: "screen", ParentID: ptrStr("site1")},
				{ID: "grp", Kind: "group", ParentID: ptrStr("scr")},
				{ID: "site1", Kind: "site", ParentID: ptrStr("org1"), TZ: ptrStr("America/Denver"), Lat: ptrF64(1), Long: ptrF64(2)},
			},
			code: "SCOPE_NODE_PARENT_INVALID",
		},
		{
			name:  "site missing geo",
			nodes: []ScopeNode{{ID: "s1", Kind: "site", ParentID: ptrStr("org1")}},
			code:  "SCOPE_NODE_GEO_REQUIRED",
		},
		{
			name: "multiple org nodes",
			nodes: []ScopeNode{
				{ID: "o1", Kind: "org", ParentID: nil},
				{ID: "o2", Kind: "org", ParentID: nil},
			},
			code: "SCOPE_NODE_MULTIPLE_ORG",
		},
		{
			name:  "org declares geo",
			nodes: []ScopeNode{{ID: "o1", Kind: "org", ParentID: nil, TZ: ptrStr("America/Denver"), Lat: ptrF64(1), Long: ptrF64(2)}},
			code:  "SCOPE_NODE_GEO_FORBIDDEN",
		},
		{
			name: "partial geo override on a group (columns not a unit)",
			nodes: []ScopeNode{
				{ID: "site1", Kind: "site", ParentID: ptrStr("org1"), TZ: ptrStr("America/Denver"), Lat: ptrF64(1), Long: ptrF64(2)},
				{ID: "grp", Kind: "group", ParentID: ptrStr("site1"), TZ: ptrStr("America/New_York")},
			},
			code: "SCOPE_NODE_GEO_PARTIAL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := BuildScopeTree(tc.nodes)
			if !hasErr(errs, tc.code, "") {
				t.Errorf("want error %s; got %+v", tc.code, errs)
			}
		})
	}
}

// DAT-005a: a scope node's own id MUST be a syntactically valid canonical
// ULID; a syntactically invalid one is rejected (ROW_ID_INVALID) independent
// of whether the rest of the node is otherwise valid.
func TestScopeNodeIDMustBeValidULID(t *testing.T) {
	nodes := []ScopeNode{
		{ID: "totally-not-a-ulid", Kind: "org", ParentID: nil, Name: "Bad Org"},
	}
	_, errs := BuildScopeTree(nodes)
	if !hasErr(errs, "ROW_ID_INVALID", "id") {
		t.Errorf("want ROW_ID_INVALID on field id; got %+v", errs)
	}

	// A syntactically valid ULID id is accepted.
	valid := []ScopeNode{
		{ID: "01JN10NENDVPG2X7R5JQC42EJ0", Kind: "org", ParentID: nil, Name: "Good Org"},
	}
	_, errs = BuildScopeTree(valid)
	if hasErr(errs, "ROW_ID_INVALID", "") {
		t.Errorf("a valid ULID id must not be rejected; got %+v", errs)
	}
}

// DAT-001a: a scope node's name MUST be a non-empty string of at most 200
// characters. The name check sits ABOVE the kind check in BuildScopeTree's
// per-node loop (load-bearing placement: the kind check's continue must not
// swallow a name error), so a node with both an invalid name and an invalid
// kind surfaces BOTH errors, name first.
func TestScopeNodeNameValidation(t *testing.T) {
	validSite := func(name string) ScopeNode {
		return ScopeNode{ID: "s1", Kind: "site", ParentID: ptrStr("org1"), Name: name, TZ: ptrStr("America/Denver"), Lat: ptrF64(1), Long: ptrF64(2)}
	}

	t.Run("empty name alone", func(t *testing.T) {
		_, errs := BuildScopeTree([]ScopeNode{validSite("")})
		if !hasErr(errs, "SCOPE_NODE_NAME_INVALID", "name") {
			t.Errorf("want SCOPE_NODE_NAME_INVALID on field name; got %+v", errs)
		}
	})

	t.Run("whitespace-only name", func(t *testing.T) {
		_, errs := BuildScopeTree([]ScopeNode{validSite("   ")})
		if !hasErr(errs, "SCOPE_NODE_NAME_INVALID", "name") {
			t.Errorf("want SCOPE_NODE_NAME_INVALID on field name; got %+v", errs)
		}
	})

	t.Run("name and kind both bad: exactly two errors in field order name,kind", func(t *testing.T) {
		nodes := []ScopeNode{{ID: "01JN10NENDVPG2X7R5JQC42EJ0", Kind: "building", ParentID: ptrStr("org1"), Name: ""}}
		_, errs := BuildScopeTree(nodes)
		if len(errs) != 2 {
			t.Fatalf("want exactly 2 errors, got %d: %+v", len(errs), errs)
		}
		if errs[0].Field != "name" || errs[0].Code != "SCOPE_NODE_NAME_INVALID" {
			t.Errorf("errs[0] = %+v, want field name / SCOPE_NODE_NAME_INVALID (name check must run above the kind check)", errs[0])
		}
		if errs[1].Field != "kind" || errs[1].Code != "SCOPE_NODE_KIND_INVALID" {
			t.Errorf("errs[1] = %+v, want field kind / SCOPE_NODE_KIND_INVALID", errs[1])
		}
	})

	t.Run("201-character name is rejected", func(t *testing.T) {
		name := strings.Repeat("a", 201)
		_, errs := BuildScopeTree([]ScopeNode{validSite(name)})
		if !hasErr(errs, "SCOPE_NODE_NAME_INVALID", "name") {
			t.Errorf("want SCOPE_NODE_NAME_INVALID on a 201-char name; got %+v", errs)
		}
	})

	t.Run("200-character name is valid", func(t *testing.T) {
		name := strings.Repeat("a", 200)
		_, errs := BuildScopeTree([]ScopeNode{validSite(name)})
		if hasErr(errs, "SCOPE_NODE_NAME_INVALID", "") {
			t.Errorf("a 200-char name must not be rejected; got %+v", errs)
		}
	})

	// DAT-001a's 200 limit is a character count, not a byte count: "é" is a single
	// character encoded as 2 UTF-8 bytes, so 150 of them is a 150-character,
	// 300-byte name. It MUST be accepted — len() on a Go string counts bytes, so a
	// naive len(n.Name) > 200 check wrongly rejects this as too long.
	t.Run("150-character multi-byte-rune name is valid despite exceeding 200 bytes", func(t *testing.T) {
		name := strings.Repeat("é", 150)
		if len(name) <= maxScopeNodeNameLen {
			t.Fatalf("test fixture invariant broken: name byte length = %d, want > %d", len(name), maxScopeNodeNameLen)
		}
		_, errs := BuildScopeTree([]ScopeNode{validSite(name)})
		if hasErr(errs, "SCOPE_NODE_NAME_INVALID", "") {
			t.Errorf("a 150-character (300-byte) name must not be rejected; got %+v", errs)
		}
	})

	// The 201-character boundary MUST be enforced on the character count too, even
	// when every character is multi-byte.
	t.Run("201-character multi-byte-rune name is rejected", func(t *testing.T) {
		name := strings.Repeat("é", 201)
		_, errs := BuildScopeTree([]ScopeNode{validSite(name)})
		if !hasErr(errs, "SCOPE_NODE_NAME_INVALID", "name") {
			t.Errorf("want SCOPE_NODE_NAME_INVALID on a 201-character multi-byte name; got %+v", errs)
		}
	})
}

// BuildFullScopeTree is the app store's variant: DAT-002's "parent_id MUST
// reference an existing scope node" is enforced rather than treated as a
// subtree boundary. The tolerant variant's answer on the same input is asserted
// alongside, so the strictness provably lives in the full-tree variant and not
// in a check both share — a relay snapshot must keep validating without its org.
func TestBuildFullScopeTreeResolvesEveryParent(t *testing.T) {
	org := ScopeNode{ID: "01J8Z0A0000000000000000001", Kind: "org", Name: "Org"}
	site := ScopeNode{
		ID: "01J8Z0A0000000000000000002", Kind: "site", ParentID: ptrStr(org.ID), Name: "Site",
		TZ: ptrStr("America/Chicago"), Lat: ptrF64(41.8), Long: ptrF64(-87.6),
	}
	screen := ScopeNode{ID: "01J8Z0A0000000000000000003", Kind: "screen", ParentID: ptrStr(site.ID), Name: "Screen"}

	t.Run("a complete resolving tree is valid in both variants", func(t *testing.T) {
		if _, errs := BuildFullScopeTree([]ScopeNode{org, site, screen}); len(errs) != 0 {
			t.Errorf("full: %+v", errs)
		}
		if _, errs := BuildScopeTree([]ScopeNode{org, site, screen}); len(errs) != 0 {
			t.Errorf("tolerant: %+v", errs)
		}
	})

	t.Run("an empty set is valid — the tree before its org is created", func(t *testing.T) {
		if _, errs := BuildFullScopeTree(nil); len(errs) != 0 {
			t.Errorf("full: %+v", errs)
		}
	})

	t.Run("a dangling parent is DAT-002-invalid in the full tree, a boundary in a snapshot", func(t *testing.T) {
		// site+screen without the org row: exactly what a re-kinded org (the
		// DAT-022 bypass) or a re-parent-to-garbage PATCH would leave stored.
		if _, errs := BuildFullScopeTree([]ScopeNode{site, screen}); !hasErr(errs, "SCOPE_NODE_PARENT_INVALID", "parent_id") {
			t.Errorf("full: want SCOPE_NODE_PARENT_INVALID on the dangling parent; got %+v", errs)
		}
		if _, errs := BuildScopeTree([]ScopeNode{site, screen}); len(errs) != 0 {
			t.Errorf("tolerant: a subtree must stay valid; got %+v", errs)
		}
	})
}

// TestACycleIsRefusedByTheFullTreeButNotTheSubtreeBuilder pins both halves of
// the reachability check: that it catches what no per-row rule can, and that it
// does not fire where a subtree is legitimate.
//
// A cycle passes every other check. Two groups each naming the other have a
// non-null parent_id (DAT-002), a parent that resolves, and a permitted parent
// kind — DAT-003 allows group under group. Every reference is real; the pair
// simply hangs off nothing, which is DAT-002's exactly-one-org clause read as a
// property of the tree rather than of one row.
func TestACycleIsRefusedByTheFullTreeButNotTheSubtreeBuilder(t *testing.T) {
	orgID := "01J8Z" + strings.Repeat("A", 21)
	gA := "01J8Z" + strings.Repeat("B", 21)
	gB := "01J8Z" + strings.Repeat("C", 21)

	// An org, plus two groups pointing at each other rather than at the org.
	nodes := []ScopeNode{
		{ID: orgID, Kind: "org", Name: "Root", AccountState: "active", Entitlements: json.RawMessage(`{}`)},
		{ID: gA, Kind: "group", ParentID: &gB, Name: "A"},
		{ID: gB, Kind: "group", ParentID: &gA, Name: "B"},
	}

	if _, errs := BuildFullScopeTree(nodes); len(errs) == 0 {
		t.Fatal("the full tree accepted a cycle detached from the root — every reference resolves, so no per-row rule can catch this; only a reachability walk can")
	} else {
		var sawReach bool
		for _, e := range errs {
			if e.Field == "parent_id" && strings.Contains(e.Message, "org-kind root") {
				sawReach = true
			}
		}
		if !sawReach {
			t.Fatalf("the full tree refused the cycle for some other reason: %+v", errs)
		}
	}

	// The SUBTREE builder must accept the same shape. A relay receives the scope
	// nodes its own site's schedule needs and nothing above them, so no node in a
	// snapshot reaches an org — asking would reject every legitimate snapshot.
	subtree := []ScopeNode{
		{ID: gA, Kind: "group", ParentID: &gB, Name: "A"},
		{ID: gB, Kind: "group", ParentID: &gA, Name: "B"},
	}
	if _, errs := BuildScopeTree(subtree); len(errs) != 0 {
		t.Fatalf("the subtree builder rejected a rootless set: %+v — the reachability check must not reach the relay path", errs)
	}
}

// TestAnOrglessTreeIsNotReportedByTheReachabilityWalk pins the boundary the walk
// deliberately does NOT cross.
//
// Every non-cyclic way to be detached is already an error from a per-row rule — a
// non-org with a nil parent_id, or one whose parent does not resolve — so the walk
// only needs to add cycles. Flagging an org-less tree as well fired on the
// bootstrap case (the first node of an empty store, created as a non-org) and put
// a third error about the tree's shape onto a frozen corpus case about two bad
// fields. That half is a separate change with a corpus decision in it.
func TestAnOrglessTreeIsNotReportedByTheReachabilityWalk(t *testing.T) {
	site := "01J8Z" + strings.Repeat("D", 21)
	org := "01J8Z" + strings.Repeat("E", 21)
	tz := "America/Chicago"
	lat, long := 41.8781, -87.6298

	// A site whose parent resolves to a node that is not there: reported by the
	// per-row resolution rule, NOT by the walk, and reported once.
	_, errs := BuildFullScopeTree([]ScopeNode{
		{ID: site, Kind: "site", ParentID: &org, Name: "Orphan",
			TZ: &tz, Lat: &lat, Long: &long},
	})
	for _, e := range errs {
		if strings.Contains(e.Message, "cycles without reaching") {
			t.Errorf("the walk reported a non-cyclic detached node: %+v — that is the resolution rule's job", e)
		}
	}
	if len(errs) == 0 {
		t.Fatal("a site whose parent does not resolve was accepted")
	}

	// And an empty set is not a fault: a store before its first write has no tree.
	if _, errs := BuildFullScopeTree(nil); len(errs) != 0 {
		t.Fatalf("an empty node set was refused: %+v", errs)
	}
}
