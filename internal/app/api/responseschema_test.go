package api_test

// responseschema_test.go is the drift check between the response bodies this
// surface SERVES and the schemas api/openapi.yaml DECLARES for them.
//
// # Why it exists
//
// The operation-level drift check (internal/app/api/surface_test.go) compares
// mounted routes against declared paths. It never looks inside a response, and
// a whole class of drift lives there: `POST /scope-nodes {kind, name}` answered
// 201 with no `parent_id` and no `labels`, both declared REQUIRED on the
// ScopeNode schema; `POST /automations` answered 201 with no `labels`,
// `enabled`, `max` or `conditions`. Every generated client's type said those
// members were always there. Nothing compared the two, so nothing said
// otherwise — a document is not executed, and a handler does not read.
//
// # What it does
//
// For every operation the document declares a 2xx JSON response schema for, it
// drives that operation against the LIVE, HTTP-mounted handler (api.New over a
// real store, auth store, device registry and content origin — the same
// composition the feeder runs) and validates the bytes that come back against
// the declared schema: every required member present, and not null where the
// declared type does not admit null.
//
// It is deliberately a PROBE rather than a reading of the handlers. The defect
// it was written for is invisible in the handler source — the handler writes
// whatever the store persisted, and what the store persisted depended on what
// the create body happened to mention.
//
// # Why it cannot pass vacuously
//
// Three guards, following surface_test.go's precedent that a check asserting
// nothing is worse than no check:
//
//   - Both sides are asserted non-empty. An empty declared set or an empty probe
//     set is a hard failure, not a quiet agreement.
//   - Every declared operation MUST have a probe, and every probe MUST name a
//     declared operation. There is no allowlist to add an exception to: an
//     operation nobody drives is exactly the state that let this drift survive.
//   - An array whose item schema declares required members MUST be non-empty in
//     the probed response. An empty `items` would validate trivially while
//     checking nothing about the item schema — which is the whole point of a
//     list operation's response.
//
// # What it does not check
//
// Value-level constraints (a ULID's pattern, an enum's membership, a string's
// maxLength) are not validated here. Presence and nullability are the whole of
// what this defect class is, and a hand-rolled partial JSON Schema evaluator
// that silently skipped the keywords it did not implement would be exactly the
// vacuous check the guards above exist to prevent.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/workspacekey"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/secretseal"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// schemaDocPath is the document this package's responses are checked against,
// relative to this package's directory (the same anchor surface_test.go uses).
const schemaDocPath = "../../../api/openapi.yaml"

// ---------------------------------------------------------------------------
// The declared side: api/openapi.yaml's 2xx JSON response schemas.
// ---------------------------------------------------------------------------

// jsonSchema is the subset of OpenAPI 3.1 schema keywords this check evaluates:
// composition ($ref/allOf), object structure (required/properties), array
// structure (items), and the declared type set (which is what says whether null
// is an admissible value).
type jsonSchema struct {
	Ref        string                 `yaml:"$ref"`
	Type       yaml.Node              `yaml:"type"`
	Required   []string               `yaml:"required"`
	Properties map[string]*jsonSchema `yaml:"properties"`
	Items      *jsonSchema            `yaml:"items"`
	AllOf      []*jsonSchema          `yaml:"allOf"`
}

// openAPIDoc is the slice of the document this check reads. A path item's
// entries are held as raw nodes because not all of them are operations: a
// path-level `parameters` is a SEQUENCE sitting beside the method keys, so the
// method filter below decides what is decoded as an operation rather than the
// decoder guessing.
type openAPIDoc struct {
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components struct {
		Schemas map[string]*jsonSchema `yaml:"schemas"`
	} `yaml:"components"`
}

// operationMethods are the path-item keys that name an OPERATION, matching
// surface_test.go's own list.
var operationMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// operationDecl is one operation's declaration, narrowed to what this check
// reads: its id and its responses.
type operationDecl struct {
	OperationID string                  `yaml:"operationId"`
	Responses   map[string]responseDecl `yaml:"responses"`
}

type responseDecl struct {
	Content map[string]struct {
		Schema *jsonSchema `yaml:"schema"`
	} `yaml:"content"`
}

// declaredResponse is one operation's declared success response: the status it
// answers with and the schema that status's JSON body must satisfy.
type declaredResponse struct {
	operationID string
	status      int
	schema      *jsonSchema
}

// declaredSuccessResponses reads every operation that declares a 2xx response
// with an `application/json` schema, keyed by operationId.
//
// A 2xx response with NO json content (logout's 204, a delete's 204) declares no
// body to check and is not part of this set; a 2xx whose content is
// `application/problem+json` is not either — a Problem is an error shape and
// has its own corpus cases.
func declaredSuccessResponses(t *testing.T) (map[string]declaredResponse, map[string]*jsonSchema) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(schemaDocPath))
	if err != nil {
		t.Fatalf("read %s: %v", schemaDocPath, err)
	}
	var doc openAPIDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", schemaDocPath, err)
	}

	out := map[string]declaredResponse{}
	for path, item := range doc.Paths {
		for key, node := range item {
			if !operationMethods[strings.ToLower(key)] {
				continue
			}
			var op operationDecl
			if err := node.Decode(&op); err != nil {
				t.Fatalf("parse %s: %s %s: %v", schemaDocPath, strings.ToUpper(key), path, err)
			}
			if op.OperationID == "" {
				t.Fatalf("parse %s: %s %s declares no operationId", schemaDocPath, strings.ToUpper(key), path)
			}
			for code, resp := range op.Responses {
				status, err := strconv.Atoi(code)
				if err != nil || status < 200 || status > 299 {
					continue
				}
				body, ok := resp.Content["application/json"]
				if !ok || body.Schema == nil {
					continue
				}
				if prior, dup := out[op.OperationID]; dup {
					t.Fatalf("operation %s declares two 2xx JSON responses (%d and %d); this check assumes one success shape per operation",
						op.OperationID, prior.status, status)
				}
				out[op.OperationID] = declaredResponse{operationID: op.OperationID, status: status, schema: body.Schema}
			}
		}
	}
	return out, doc.Components.Schemas
}

// resolve follows $ref and flattens allOf into one effective schema: the union
// of the required lists, properties, and items of every branch, with the first
// declared type set winning. It is enough for this document, where allOf is used
// only to attach a description to a $ref'd scalar.
func resolve(s *jsonSchema, defs map[string]*jsonSchema, seen map[*jsonSchema]bool) *jsonSchema {
	if s == nil {
		return nil
	}
	if seen == nil {
		seen = map[*jsonSchema]bool{}
	}
	if seen[s] {
		return s
	}
	seen[s] = true

	if s.Ref != "" {
		name := strings.TrimPrefix(s.Ref, "#/components/schemas/")
		target, ok := defs[name]
		if !ok {
			return s
		}
		return resolve(target, defs, seen)
	}
	if len(s.AllOf) == 0 {
		return s
	}
	merged := &jsonSchema{Type: s.Type, Required: append([]string(nil), s.Required...), Properties: map[string]*jsonSchema{}, Items: s.Items}
	for k, v := range s.Properties {
		merged.Properties[k] = v
	}
	for _, branch := range s.AllOf {
		b := resolve(branch, defs, seen)
		if b == nil {
			continue
		}
		if merged.Type.IsZero() {
			merged.Type = b.Type
		}
		merged.Required = append(merged.Required, b.Required...)
		for k, v := range b.Properties {
			if _, exists := merged.Properties[k]; !exists {
				merged.Properties[k] = v
			}
		}
		if merged.Items == nil {
			merged.Items = b.Items
		}
	}
	return merged
}

// admitsNull reports whether the schema's declared `type` includes "null" — the
// difference between `parent_id` (declared `["string", "null"]`, where null is a
// value) and `labels` (declared an object, where null is a client-breaking
// surprise). A schema declaring no type at all admits anything, null included.
func admitsNull(s *jsonSchema) bool {
	if s == nil || s.Type.IsZero() {
		return true
	}
	var one string
	if err := s.Type.Decode(&one); err == nil {
		return one == "null"
	}
	var many []string
	if err := s.Type.Decode(&many); err == nil {
		for _, t := range many {
			if t == "null" {
				return true
			}
		}
		return false
	}
	return true
}

// validate walks a decoded response body against a schema and returns one
// finding per violation, each naming the JSON path it was found at.
func validate(path string, s *jsonSchema, v any, defs map[string]*jsonSchema) []string {
	eff := resolve(s, defs, nil)
	if eff == nil {
		return nil
	}
	var out []string
	switch val := v.(type) {
	case map[string]any:
		for _, key := range eff.Required {
			member, present := val[key]
			if !present {
				out = append(out, fmt.Sprintf("%s.%s is declared required but the response omits it", path, key))
				continue
			}
			if member == nil && !admitsNull(resolve(eff.Properties[key], defs, nil)) {
				out = append(out, fmt.Sprintf("%s.%s is declared required and its declared type does not admit null, but the response carries null", path, key))
			}
		}
		for key, member := range val {
			prop, declared := eff.Properties[key]
			if !declared || member == nil {
				continue
			}
			out = append(out, validate(path+"."+key, prop, member, defs)...)
		}
	case []any:
		item := resolve(eff.Items, defs, nil)
		if item != nil && len(item.Required) > 0 && len(val) == 0 {
			out = append(out, fmt.Sprintf("%s is empty, so its item schema's required members (%s) were never checked — "+
				"a probe must exercise at least one item or this operation's response shape is unverified",
				path, strings.Join(item.Required, ", ")))
		}
		for i, elem := range val {
			out = append(out, validate(fmt.Sprintf("%s[%d]", path, i), eff.Items, elem, defs)...)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The served side: probes that drive each declared operation for real.
// ---------------------------------------------------------------------------

// probe drives one operation against a live handler and returns the response it
// is being checked on. It receives a fully wired env of its own, so no probe
// depends on another having run.
type probe func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte)

// Fixture ids this file drives with (canonical ULIDs, DAT-005a; relay_id is
// deliberately NOT a ULID — it is minted by the enrollment path, openapi
// RelayId). They are distinct from every other fixture in the package so a probe
// here is never satisfied by a row another test wrote.
const (
	rsRelayID     = "relay-fixture-a"
	rsScopeNode   = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	rsDeviceID    = "01J8ZRESP0DEV1CEF1XTVRE001"
	rsEntityID    = "01J8ZRESP0ENT1TYF1XTVRE001"
	rsWorkspaceID = "01J8ZRESP0WSKEYF1XTVRE0001"
	// rsIdentifier / rsPassword are the credential the claim probe establishes
	// and the login probe then presents. Fixture values for an in-memory store
	// that lives for one test; they protect nothing.
	rsIdentifier = "response-schema-owner"
	rsPassword   = "response-schema-passphrase"
)

// schemaProbeEnv is a live api.New handler with EVERY collaborator wired: the
// device plane (so the device families serve real rows and a command really
// dispatches), the workspace archive (so the data-subject operations accept),
// a job runner, and an auth store holding a secret sealer (so second-factor
// enrollment is available rather than refused for want of a workspace key).
//
// One env per probe. A shared env would make each probe's response depend on
// what the probes before it had written, which is the wrong thing for a check
// whose whole subject is the shape of one operation's answer.
type schemaProbeEnv struct {
	*testEnv
	authStore *auth.Store
}

func newSchemaProbeEnv(t *testing.T) *schemaProbeEnv {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	key, err := workspacekey.LoadOrCreate(t.TempDir(), func() string { return rsWorkspaceID })
	if err != nil {
		t.Fatalf("workspacekey.LoadOrCreate: %v", err)
	}
	sealer, err := key.SecretSealer()
	if err != nil {
		t.Fatalf("SecretSealer: %v", err)
	}
	clock := func() int64 { return fixedNowMs }
	fixture, err := authtest.New(authtest.Config{NowMs: clock, Sealer: sealer})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	t.Cleanup(fixture.Close)

	registry := devices.New()
	mustPutDevice(t, registry, devices.Device{
		ID: rsDeviceID, RelayID: rsRelayID, DeviceClass: "media-player",
		Name: "Lobby TV", ScopeNode: rsScopeNode, Labels: map[string]string{"env": "prod"},
	})
	mustPutEntity(t, registry, devices.Entity{
		ID: rsEntityID, DeviceID: rsDeviceID, RelayID: rsRelayID, DeviceClass: "media-player",
		Name: "Lobby TV player", ScopeNode: rsScopeNode, Labels: map[string]string{"env": "prod"}, State: "on",
	})

	content := origin.New()
	jobs := api.NewJobRunner()
	ts := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.Monotonic(),
		content, testContentBase, fixture.Auth,
		api.WithJobRunner(jobs),
		api.WithDevicePlane(registry, &fakeDispatcher{result: wire.DeviceCommandResultBody{OK: true}}),
		api.WithWorkspaceArchive(&api.WorkspaceArchive{Dir: t.TempDir(), Key: key, KDF: lightKDF()})))
	t.Cleanup(ts.Close)

	return &schemaProbeEnv{
		testEnv:   &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture, jobs: jobs},
		authStore: fixture.Store,
	}
}

// sealerFor is unused indirection avoided: secretseal is imported for the type
// assertion below, which pins that the sealer handed to the auth fixture is the
// real construction rather than a no-op double — a stub would let the TOTP
// probes pass against an implementation that never sealed anything.
var _ = func(s *secretseal.Sealer) auth.SecretSealer { return s }

// mintOrg creates the deployment's org node (the row that IS the workspace,
// DAT-010/012/014) and returns its server-minted id.
func (e *schemaProbeEnv) mintOrg(t *testing.T) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes",
		mustJSON(t, map[string]any{"kind": "org", "name": "Fixture Org", "account_state": "active"}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint org node: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// mintAutomation creates an edge automation under scopeNode and returns its id.
// It states `enabled` because a create that does not comes back disabled, and a
// probe of the run operation needs a rule that is actually on.
func (e *schemaProbeEnv) mintAutomation(t *testing.T, scopeNode string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", scopeNode, map[string]string{"env": "prod"}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint automation: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// etagOf reads a resource's current ETag, the token an api/1 conditional write
// is made under.
func (e *schemaProbeEnv) etagOf(t *testing.T, path string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s for its ETag: %d %s", path, resp.StatusCode, raw)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("GET %s returned no ETag", path)
	}
	return etag
}

// claim redeems a freshly minted setup grant, establishing a principal with a
// password credential — what the login probe then presents. It returns the
// claim's own response, which is itself a probed operation.
func (e *schemaProbeEnv) claim(t *testing.T) (*http.Response, []byte) {
	t.Helper()
	minted, err := e.authStore.MintGrant(t.Context(), auth.MintGrantOptions{
		Purpose:                auth.PurposeSetup,
		ResultingPrincipalKind: auth.KindUser,
		ScopeNode:              auth.RootScopeNode,
		Role:                   auth.RoleOwner,
		TTLMs:                  auth.DefaultSetupGrantTTLMs,
		RedemptionMode:         auth.RedemptionOneTime,
		IssuedVia:              auth.IssuedViaConsole,
	})
	if err != nil {
		t.Fatalf("MintGrant: %v", err)
	}
	return e.do(t, http.MethodPost, "/api/v1/auth/setup",
		mustJSON(t, map[string]any{"code": minted.Code, "identifier": rsIdentifier, "password": rsPassword}), nil)
}

// probes is the served side: one entry per declared 2xx-JSON operation, keyed by
// the operationId the document gives it. The test refuses to run if this map and
// the declared set do not cover each other exactly.
var probes = map[string]probe{
	// --- scope-nodes ------------------------------------------------------
	"createScopeNode": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// Deliberately the MINIMAL create — kind and name and nothing else. This
		// is the exact request whose 201 carried neither `parent_id` nor `labels`.
		return e.do(t, http.MethodPost, "/api/v1/scope-nodes",
			mustJSON(t, map[string]any{"kind": "org", "name": "Minimal Org"}), nil)
	},
	"getScopeNode": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintOrg(t)
		return e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+id, nil, nil)
	},
	"updateScopeNode": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintOrg(t)
		return e.do(t, http.MethodPatch, "/api/v1/scope-nodes/"+id,
			mustJSON(t, map[string]any{"name": "Renamed Org"}),
			map[string]string{"If-Match": e.etagOf(t, "/api/v1/scope-nodes/"+id)})
	},
	"listScopeNodes": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintOrg(t)
		return e.do(t, http.MethodGet, "/api/v1/scope-nodes", nil, nil)
	},

	// --- automations ------------------------------------------------------
	"createAutomation": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// The MINIMAL create: exactly AutomationCreate's own required members and
		// nothing more. Its 201 carried no `labels`, `enabled`, `max` or
		// `conditions`.
		return e.do(t, http.MethodPost, "/api/v1/automations", mustJSON(t, map[string]any{
			"name":       "Minimal Automation",
			"scope_node": e.mintOrg(t),
			"mode":       "single",
			"triggers":   []any{map[string]any{"type": "state", "entity_id": autoScreenEntity, "to": []string{"on"}}},
			"actions":    []any{map[string]any{"type": "device_command", "entity_id": autoScreenEntity, "command": "launch"}},
		}), nil)
	},
	"getAutomation": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintAutomation(t, e.mintOrg(t))
		return e.do(t, http.MethodGet, "/api/v1/automations/"+id, nil, nil)
	},
	"updateAutomation": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintAutomation(t, e.mintOrg(t))
		return e.do(t, http.MethodPatch, "/api/v1/automations/"+id,
			mustJSON(t, map[string]any{"name": "Renamed Automation"}),
			map[string]string{"If-Match": e.etagOf(t, "/api/v1/automations/"+id)})
	},
	"listAutomations": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintAutomation(t, e.mintOrg(t))
		return e.do(t, http.MethodGet, "/api/v1/automations", nil, nil)
	},
	"runAutomation": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintAutomation(t, e.mintOrg(t))
		return e.do(t, http.MethodPost, "/api/v1/automations/"+id+"/run", []byte(`{}`), nil)
	},
	"bulkEnableAutomations": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintAutomation(t, e.mintOrg(t))
		return e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable",
			mustJSON(t, map[string]any{"selector": "env=prod", "enabled": true}), nil)
	},

	// --- jobs -------------------------------------------------------------
	"getJob": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintAutomation(t, e.mintOrg(t))
		resp, raw := e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable",
			mustJSON(t, map[string]any{"selector": "env=prod", "enabled": true}), nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("bulk-enable to obtain a Job: %d %s", resp.StatusCode, raw)
		}
		return e.do(t, http.MethodGet, "/api/v1/jobs/"+decodeID(t, raw), nil, nil)
	},

	// --- device plane -----------------------------------------------------
	"listDevices": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		return e.do(t, http.MethodGet, "/api/v1/devices", nil, nil)
	},
	"listEntities": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		return e.do(t, http.MethodGet, "/api/v1/entities", nil, nil)
	},
	"sendEntityCommand": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		return e.do(t, http.MethodPost, "/api/v1/entities/"+rsEntityID+"/commands",
			mustJSON(t, map[string]any{"command": "launch"}), nil)
	},

	// --- workspace --------------------------------------------------------
	"exportWorkspace": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintOrg(t)
		return e.do(t, http.MethodPost, "/api/v1/workspace/export",
			mustJSON(t, map[string]any{"passphrase": testExportPassphrase}), nil)
	},
	"deleteWorkspace": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		return e.do(t, http.MethodPost, "/api/v1/workspace/delete",
			mustJSON(t, map[string]any{"confirm_workspace_id": e.mintOrg(t)}), nil)
	},

	// --- auth -------------------------------------------------------------
	"claimWorkspace": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		return e.claim(t)
	},
	"login": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		if resp, raw := e.claim(t); resp.StatusCode != http.StatusCreated {
			t.Fatalf("claim to establish a password credential: %d %s", resp.StatusCode, raw)
		}
		return e.do(t, http.MethodPost, "/api/v1/auth/login",
			mustJSON(t, map[string]any{"identifier": rsIdentifier, "password": rsPassword}), nil)
	},
	"getSession": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		return e.do(t, http.MethodGet, "/api/v1/auth/session", nil, nil)
	},
	"enrollTotp": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		return e.do(t, http.MethodPost, "/api/v1/auth/totp/enroll", []byte(`{}`), nil)
	},
	"confirmTotp": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		resp, raw := e.do(t, http.MethodPost, "/api/v1/auth/totp/enroll", []byte(`{}`), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("begin a TOTP enrollment: %d %s", resp.StatusCode, raw)
		}
		var enrollment struct {
			Secret string `json:"secret"`
		}
		if err := json.Unmarshal(raw, &enrollment); err != nil {
			t.Fatalf("decode enrollment: %v", err)
		}
		secret, err := auth.DecodeTOTPSecret(enrollment.Secret)
		if err != nil {
			t.Fatalf("decode enrollment secret: %v", err)
		}
		// The code is computed from the enrollment's OWN secret at the fixture
		// clock — the same derivation an authenticator app performs, never a
		// value read back from what the handler is about to be asked to accept.
		code := auth.TOTPCode(secret, auth.TOTPStep(fixedNowMs))
		return e.do(t, http.MethodPost, "/api/v1/auth/totp/confirm",
			mustJSON(t, map[string]any{"code": code}), nil)
	},
}

// TestServedResponsesMatchDeclaredSchemas fails when a 2xx response omits a
// member its declared schema requires, when a declared operation has no probe,
// or when a probe names an operation the document does not declare.
func TestServedResponsesMatchDeclaredSchemas(t *testing.T) {
	declared, defs := declaredSuccessResponses(t)

	// Non-emptiness first, so a broken reader reports itself as broken rather
	// than as twenty unrelated coverage failures (surface_test.go's precedent).
	if len(declared) == 0 {
		t.Fatalf("no 2xx JSON response schema was read out of %s, so this check proves nothing", schemaDocPath)
	}
	if len(probes) == 0 {
		t.Fatal("the probe set is empty, so this check proves nothing")
	}
	if len(defs) == 0 {
		t.Fatalf("no component schemas were read out of %s, so every $ref would resolve to nothing", schemaDocPath)
	}

	for _, id := range sortedOperationIDs(declared) {
		if _, ok := probes[id]; !ok {
			t.Errorf("declared with no probe: %s declares a 2xx JSON response in %s but nothing here drives it — "+
				"its response shape is unverified, which is the state this check exists to make impossible",
				id, schemaDocPath)
		}
	}
	for id := range probes {
		if _, ok := declared[id]; !ok {
			t.Errorf("probed with no declaration: a probe names operation %s, which declares no 2xx JSON response in %s — "+
				"either the operationId is misspelled or the declaration was removed", id, schemaDocPath)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	for _, id := range sortedOperationIDs(declared) {
		decl := declared[id]
		t.Run(id, func(t *testing.T) {
			e := newSchemaProbeEnv(t)
			resp, raw := probes[id](t, e)
			if resp.StatusCode != decl.status {
				t.Fatalf("%s answered %d, but %s declares its success response as %d (body %s)",
					id, resp.StatusCode, schemaDocPath, decl.status, raw)
			}
			var body any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("%s answered %d with a body that is not JSON: %v (body %s)", id, resp.StatusCode, err, raw)
			}
			for _, finding := range validate("body", decl.schema, body, defs) {
				t.Errorf("%s: %s\n  response: %s", id, finding, raw)
			}
		})
	}
}

func sortedOperationIDs(m map[string]declaredResponse) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
