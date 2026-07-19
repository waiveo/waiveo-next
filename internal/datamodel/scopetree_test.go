package datamodel

import (
	"encoding/json"
	"os"
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
	nodes := []ScopeNode{
		{ID: "site1", Kind: "site", ParentID: ptrStr("org1"), TZ: ptrStr("America/Denver"), Lat: ptrF64(39.7392), Long: ptrF64(-104.9903)},
		{ID: "grp1", Kind: "group", ParentID: ptrStr("site1"), TZ: ptrStr("America/New_York"), Lat: ptrF64(40.7128), Long: ptrF64(-74.0060)},
		{ID: "scr1", Kind: "screen", ParentID: ptrStr("grp1")},
	}
	tree, errs := BuildScopeTree(nodes)
	if len(errs) != 0 {
		t.Fatalf("unexpected build errors: %+v", errs)
	}
	// group resolves to its own override.
	if g, e := tree.EffectiveGeo("grp1"); e != nil || g.TZ != "America/New_York" || g.Source != "own" {
		t.Errorf("group EffectiveGeo = %+v, err=%v; want own America/New_York", g, e)
	}
	// screen with no override skips the group override and lands on the site.
	g, e := tree.EffectiveGeo("scr1")
	if e != nil {
		t.Fatalf("screen EffectiveGeo error: %v", e)
	}
	if g.TZ != "America/Denver" || g.Lat != 39.7392 || g.Long != -104.9903 {
		t.Errorf("screen EffectiveGeo = %+v; want site geo America/Denver", g)
	}
	if g.Source != "ancestor-site:site1" {
		t.Errorf("screen source = %q; want ancestor-site:site1", g.Source)
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
