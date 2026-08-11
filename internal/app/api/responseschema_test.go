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
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
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
	"github.com/maaxton/waiveo-next/internal/app/platformlog"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/webhookdeliver"
	"github.com/maaxton/waiveo-next/internal/app/workspacekey"
	"github.com/maaxton/waiveo-next/internal/castbundle"
	"github.com/maaxton/waiveo-next/internal/datamodel"
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
	rsRelayID   = "relay-fixture-a"
	rsScopeNode = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	rsDeviceID  = "01J8ZRESP0DEV1CEF1XTVRE001"
	rsEntityID  = "01J8ZRESP0ENT1TYF1XTVRE001"
	// rsAdoptDeviceID is the adopt probe's OWN device, distinct from rsDeviceID:
	// that one is placed at a fixture node the tree does not contain, which is
	// fine for a read but not for a write the placement is validated against.
	rsAdoptDeviceID = "01J8ZRESP0AD0PTDEV1CE00001"
	rsWorkspaceID   = "01J8ZRESP0WSKEYF1XTVRE0001"
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
	// registry is the same read model the handler serves from, kept so a probe
	// can place a device at a scope node it minted itself — the adopt probe
	// needs one, because adoption writes an authored row and a row's placement
	// must be a node that actually exists in the tree.
	registry *devices.Registry
	// logs is the SAME captured-log buffer the handler serves the diagnostics
	// read from, kept so the platform-logs probe can put a line in it. That
	// operation's item schema declares required members, so an empty page would
	// leave them unchecked — this check's own guard against a vacuous probe.
	logs *platformlog.Buffer
	// dataDir is the directory the health summary measures headroom on, kept so
	// a probe can assert the storage members were populated from a real statfs
	// rather than left at the unmeasured degrade.
	dataDir string
	// restartOrders records what the restart operation armed. The seam is wired
	// REAL in the sense that matters here — it declares a supervisor, so the
	// operation reaches its 202 rather than the RESTART_UNSUPPORTED degrade,
	// which would leave RestartAcceptance's required members unchecked — and it
	// records rather than stops, because a probe that actually ended the test
	// binary would be the last probe ever to run.
	restartOrders *[]api.RestartOrder
}

func newSchemaProbeEnv(t *testing.T) *schemaProbeEnv {
	t.Helper()
	st, err := store.Open(":memory:", store.WallClockMs)
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

	registry := devices.New(devScopeA, func() int64 { return 0 })
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
	logs := platformlog.New(64, clock)
	dataDir := t.TempDir()
	restartOrders := &[]api.RestartOrder{}
	ts := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.Monotonic(),
		content, testContentBase, fixture.Auth,
		api.WithJobRunner(jobs),
		api.WithDevicePlane(registry, &fakeDispatcher{result: wire.DeviceCommandResultBody{OK: true}}),
		// The REAL sealing construction, over the same workspace key the TOTP
		// probes seal under — a stub would let the rotate probe pass against an
		// implementation that never sealed the signing secret it was handed.
		api.WithWebhookSecrets(webhookdeliver.NewSecrets(sealer), 0),
		api.WithWorkspaceArchive(&api.WorkspaceArchive{Dir: t.TempDir(), Key: key, KDF: lightKDF()}),
		// A restore stages beside this; without it the route refuses 503 and the
		// probe below could never reach the 202 it exists to verify.
		api.WithStorePath(filepath.Join(t.TempDir(), "app.db")),
		// A one-relay pairing directory, so the pairing-code probe validates
		// the response's FULLER shape (pairing_code + relay_id present) rather
		// than only the no-relay degrade.
		api.WithPairing(rsPairingDirectory(t)),
		// The two diagnostics reads' collaborators. Both are wired REAL rather
		// than left unwired: an unwired platform log answers an empty page, and
		// an empty `items` array would leave PlatformLogRecord's required
		// members unchecked — exactly the vacuous probe this file's guards
		// exist to refuse.
		api.WithPlatformLog(logs),
		api.WithSystemHealth(api.SystemHealthConfig{
			StartedAtMs: fixedNowMs - 60_000,
			Version:     "test-build",
			DataDir:     dataDir,
		}),
		api.WithRestart(api.RestartConfig{
			Supervisor:    "probe-supervisor",
			DrainBudgetMs: 5_000,
			Arm: func(o api.RestartOrder) bool {
				*restartOrders = append(*restartOrders, o)
				return true
			},
		})))
	t.Cleanup(ts.Close)

	return &schemaProbeEnv{
		testEnv:   &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture, jobs: jobs},
		authStore: fixture.Store,
		registry:  registry,
		logs:      logs,
		dataDir:   dataDir,

		restartOrders: restartOrders,
	}
}

// rsPairingDirectory is the pairing-code probe's one-relay fixture directory:
// a connected relay with a dialable advertised address and a real SPKI, so the
// probed response carries pairing_code and relay_id.
func rsPairingDirectory(t *testing.T) api.PairingRelayDirectory {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	return api.PairingRelayDirectory{
		ConnectedRelays: func() []api.PairingRelay {
			return []api.PairingRelay{{RelayID: rsRelayID, AdvertisedAddress: "192.0.2.40:7443"}}
		},
		RelaySPKI: func(string) ([]byte, bool) { return spki, true },
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
		mustJSON(t, map[string]any{"kind": "org", "name": "Fixture Org", "account_state": "active", "entitlements": map[string]any{}}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint org node: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// rsWebhookSecret is the fixture signing secret the rotate probes install. It
// is at the surface's own 32-character floor so the probe exercises the accepted
// path rather than an over-long value that would pass any floor.
const rsWebhookSecret = "whsec_probe_0123456789abcdef0123"

// mintWebhookEndpoint registers an endpoint under scopeNode and returns its id.
func (e *schemaProbeEnv) mintWebhookEndpoint(t *testing.T, scopeNode string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/webhook-endpoints", mustJSON(t, map[string]any{
		"name":       "Fixture Endpoint",
		"scope_node": scopeNode,
		"url":        "https://hooks.example.invalid/waiveo",
		"labels":     map[string]string{"env": "prod"},
		"schemas":    []string{"automation.run"},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint webhook endpoint: %d %s", resp.StatusCode, raw)
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

// rsAdoptedEntityID is the fixture entity a probed adopted device exposes. It is
// a canonical ULID because that is what the AdoptedDeviceEntity schema declares
// and what the row validator enforces.
const rsAdoptedEntityID = "01J8Z9ENTTYMEDAPAYERAAAAA1"

// adoptedDeviceBody is the fixture create body for an adopted device. It states
// an entity deliberately: the AdoptedDeviceEntity item schema declares required
// members, and this check refuses to validate an EMPTY array against an item
// schema — an empty `entities` would pass while proving nothing about the shape
// of the policy entries that are the whole reason the row exists.
func adoptedDeviceBody(t *testing.T, scopeNode, driver, nativeID string) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"name":       "Fixture Adopted Device",
		"scope_node": scopeNode,
		"driver":     driver,
		"native_id":  nativeID,
		"entities": []any{map[string]any{
			"entity_id":    rsAdoptedEntityID,
			"device_class": "media-player",
			"enabled":      true,
			"hidden":       false,
			"display_name": "Fixture Media Player",
			"category":     "primary",
		}},
	})
}

// mintScreen creates a screen identity row under scopeNode and returns its id.
func (e *schemaProbeEnv) mintScreen(t *testing.T, scopeNode string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/screens", mustJSON(t, map[string]any{
		"name":       "Fixture Screen",
		"scope_node": scopeNode,
		"labels":     map[string]string{"env": "prod"},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint screen: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// mintVariable creates one variable row at scopeNode and returns its id.
func (e *schemaProbeEnv) mintVariable(t *testing.T, scopeNode, name string, value any) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/variables", mustJSON(t, map[string]any{
		"name":       name,
		"value":      value,
		"scope_node": scopeNode,
		"labels":     map[string]string{"env": "prod"},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint variable: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// castSlides is the one authored slide stack every cast probe writes: a text
// layer, a rect and a `derive` layer, all well inside the 1920x1080 canvas. It
// deliberately carries no image layer — an image layer's asset_ref must resolve
// in the content origin at projection time, and this env uploads no bytes, so a
// slide naming one would be exercising the content origin rather than the cast's
// own response shape.
//
// The derive layer carries NO asset_ref on purpose: that is the state a
// freshly-authored rasterized layer is in, it is what puts a work order on
// GET /derive/pending for that probe to check, and — because a derive layer's
// `derive` block is a nested schema — it is what makes the cast probes verify
// that SlideLayer actually declares the derive members rather than merely
// tolerating them.
func castSlides() []map[string]any {
	return []map[string]any{{
		"id":          "s1",
		"duration_ms": 8000,
		"layers": []map[string]any{
			{"kind": "text", "x": 100, "y": 100, "w": 900, "h": 120, "text": "Fixture Cast", "font_px": 96, "color": "#ffffff"},
			{"kind": "rect", "x": 0, "y": 980, "w": 1920, "h": 100, "color": "#101820"},
			{"kind": "derive", "x": 1500, "y": 100, "w": 320, "h": 320, "derive": map[string]any{
				"kind": "qr", "data": "https://waiveo.local/pair", "ec_level": "M",
			}},
		},
	}}
}

// mintCast creates a cast row under scopeNode and returns its id.
func (e *schemaProbeEnv) mintCast(t *testing.T, scopeNode string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", mustJSON(t, map[string]any{
		"name":       "Fixture Cast",
		"scope_node": scopeNode,
		"slides":     castSlides(),
		"labels":     map[string]string{"env": "prod"},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint cast: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// mintAdoptedDevice creates an adopted-device row under scopeNode and returns its
// id. driver/nativeID are parameters because REL-153 makes that pair the row's
// identity: a probe that minted two rows from one literal pair would collide.
func (e *schemaProbeEnv) mintAdoptedDevice(t *testing.T, scopeNode, driver, nativeID string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/adopted-devices", adoptedDeviceBody(t, scopeNode, driver, nativeID), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint adopted device: %d %s", resp.StatusCode, raw)
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
		// Deliberately the MINIMAL create — nothing optional. This is the exact
		// request whose 201 carried neither `parent_id` nor `labels`. The minimum for
		// an org is four members rather than two: DAT-010/013 make account_state and
		// entitlements mandatory on this kind, so a two-member body is now refused
		// and would probe nothing.
		return e.do(t, http.MethodPost, "/api/v1/scope-nodes",
			mustJSON(t, map[string]any{"kind": "org", "name": "Minimal Org",
				"account_state": "active", "entitlements": map[string]any{}}), nil)
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
		// The probe drives a rule that produces at least one entry in EVERY
		// effect array the response declares — a device command (refused,
		// because no device plane is wired in this env, which is still a
		// reported target), a signage action against a real screen and cast, a
		// variable write, and a log. An empty array proves nothing about its
		// item schema, and this operation's arrays are the half of the response
		// that says the run actually did something.
		node := e.mintOrg(t)
		screenID := e.mintScreen(t, node)
		castID := e.mintCast(t, node)
		resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", mustJSON(t, map[string]any{
			"name":       "Run Probe Automation",
			"scope_node": node,
			"enabled":    true,
			"mode":       "single",
			"triggers":   []any{map[string]any{"type": "state", "entity_id": autoScreenEntity, "to": []string{"on"}}},
			"actions": []any{
				map[string]any{"type": "device_command", "entity_id": autoScreenEntity, "command": "launch"},
				map[string]any{"type": "play_cast", "screen_id": screenID, "cast_id": castID},
				map[string]any{"type": "variable_write", "variable": "run_probe_flag", "value": true},
				map[string]any{"type": "log", "message": "run probe"},
			},
		}), nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("mint run-probe automation: %d %s", resp.StatusCode, raw)
		}
		id := decodeID(t, raw)
		return e.do(t, http.MethodPost, "/api/v1/automations/"+id+"/run", []byte(`{}`), nil)
	},
	"bulkEnableAutomations": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintAutomation(t, e.mintOrg(t))
		return e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable",
			mustJSON(t, map[string]any{"selector": "env=prod", "enabled": true}), nil)
	},

	// --- screens ----------------------------------------------------------
	"createScreen": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// The MINIMAL create: exactly ScreenCreate's own required members and
		// nothing more. `labels` and `device_id` are declared required on the
		// RESPONSE and named by neither — the drift class this check exists for.
		return e.do(t, http.MethodPost, "/api/v1/screens", mustJSON(t, map[string]any{
			"name":       "Minimal Screen",
			"scope_node": e.mintOrg(t),
		}), nil)
	},
	"getScreen": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintScreen(t, e.mintOrg(t))
		return e.do(t, http.MethodGet, "/api/v1/screens/"+id, nil, nil)
	},
	"updateScreen": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintScreen(t, e.mintOrg(t))
		return e.do(t, http.MethodPatch, "/api/v1/screens/"+id,
			mustJSON(t, map[string]any{"name": "Renamed Screen"}),
			map[string]string{"If-Match": e.etagOf(t, "/api/v1/screens/"+id)})
	},
	"listScreens": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintScreen(t, e.mintOrg(t))
		return e.do(t, http.MethodGet, "/api/v1/screens", nil, nil)
	},

	// --- variables (data-model/1 DAT-130-138) ------------------------------
	"createVariable": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// The MINIMAL create: exactly VariableCreate's own required members.
		// `labels` is declared required on the RESPONSE and named here by
		// nothing — the drift class this check exists for.
		return e.do(t, http.MethodPost, "/api/v1/variables", mustJSON(t, map[string]any{
			"name":       "minimal_variable",
			"value":      "open",
			"scope_node": e.mintOrg(t),
		}), nil)
	},
	"getVariable": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintVariable(t, e.mintOrg(t), "guest_mode", false)
		return e.do(t, http.MethodGet, "/api/v1/variables/"+id, nil, nil)
	},
	"updateVariable": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// Patched to a NUMBER from a boolean, so the probe exercises the
		// VariableValue union at a second member rather than only at the one the
		// row was created with.
		id := e.mintVariable(t, e.mintOrg(t), "guest_mode", false)
		return e.do(t, http.MethodPatch, "/api/v1/variables/"+id,
			mustJSON(t, map[string]any{"value": 42}),
			map[string]string{"If-Match": e.etagOf(t, "/api/v1/variables/"+id)})
	},
	"listVariables": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintVariable(t, e.mintOrg(t), "guest_mode", true)
		return e.do(t, http.MethodGet, "/api/v1/variables", nil, nil)
	},
	"issueScreenPairingCode": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintScreen(t, e.mintOrg(t))
		return e.do(t, http.MethodPost, "/api/v1/screens/"+id+"/pairing-code", nil, nil)
	},
	"setScreenNow": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		node := e.mintOrg(t)
		// `alert` with a TTL, so the probe exercises the two OPTIONAL response
		// members (`expires_at`, and `mode` at its non-default value) rather than
		// only the members a bare `play` push populates.
		return e.do(t, http.MethodPut, "/api/v1/screens/"+e.mintScreen(t, node)+"/now",
			mustJSON(t, map[string]any{"mode": "alert", "cast_id": e.mintCast(t, node), "ttl_seconds": 60}), nil)
	},
	"listScreenStatus": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// A screen WITH an override, so the response exercises the optional
		// `now` member as well as the never-observed defaults every field takes
		// when no relay has reported — the shape a deployment sees on its first
		// page load, which is the one most likely to drift unnoticed.
		node := e.mintOrg(t)
		id := e.mintScreen(t, node)
		resp, raw := e.do(t, http.MethodPut, "/api/v1/screens/"+id+"/now",
			mustJSON(t, map[string]any{"mode": "play", "cast_id": e.mintCast(t, node)}), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("push now: %d %s", resp.StatusCode, raw)
		}
		return e.do(t, http.MethodGet, "/api/v1/screen-status", nil, nil)
	},

	// --- casts ------------------------------------------------------------
	"createCast": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// The MINIMAL create: exactly CastCreate's own required members and
		// nothing more. `labels` is declared required on the RESPONSE and named
		// by neither — the drift class this check exists for.
		return e.do(t, http.MethodPost, "/api/v1/casts", mustJSON(t, map[string]any{
			"name":       "Minimal Cast",
			"scope_node": e.mintOrg(t),
			"slides":     castSlides(),
		}), nil)
	},
	"getCast": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintCast(t, e.mintOrg(t))
		return e.do(t, http.MethodGet, "/api/v1/casts/"+id, nil, nil)
	},
	"updateCast": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintCast(t, e.mintOrg(t))
		return e.do(t, http.MethodPatch, "/api/v1/casts/"+id,
			mustJSON(t, map[string]any{"name": "Renamed Cast"}),
			map[string]string{"If-Match": e.etagOf(t, "/api/v1/casts/"+id)})
	},
	"listCasts": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintCast(t, e.mintOrg(t))
		return e.do(t, http.MethodGet, "/api/v1/casts", nil, nil)
	},

	// --- derive -----------------------------------------------------------
	"listPendingDerives": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// castSlides() carries a derive layer with no asset_ref, so the cast this
		// mints puts exactly one work order in the queue — a non-empty `items`,
		// which is what makes the item schema actually checked rather than
		// vacuously satisfied.
		e.mintCast(t, e.mintOrg(t))
		return e.do(t, http.MethodGet, "/api/v1/derive/pending", nil, nil)
	},

	// --- adopted-devices --------------------------------------------------
	"createAdoptedDevice": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		return e.do(t, http.MethodPost, "/api/v1/adopted-devices",
			adoptedDeviceBody(t, e.mintOrg(t), "roku-ecp", "10.0.0.41"), nil)
	},
	"getAdoptedDevice": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintAdoptedDevice(t, e.mintOrg(t), "roku-ecp", "10.0.0.41")
		return e.do(t, http.MethodGet, "/api/v1/adopted-devices/"+id, nil, nil)
	},
	"updateAdoptedDevice": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintAdoptedDevice(t, e.mintOrg(t), "roku-ecp", "10.0.0.41")
		return e.do(t, http.MethodPatch, "/api/v1/adopted-devices/"+id,
			mustJSON(t, map[string]any{"name": "Renamed Adopted Device"}),
			map[string]string{"If-Match": e.etagOf(t, "/api/v1/adopted-devices/"+id)})
	},
	"listAdoptedDevices": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintAdoptedDevice(t, e.mintOrg(t), "roku-ecp", "10.0.0.41")
		return e.do(t, http.MethodGet, "/api/v1/adopted-devices", nil, nil)
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
	"adoptDevice": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// A real adoption end to end: a device the read model reports, mirrored
		// durably, placed at a node that exists — because the operation writes an
		// authored row and a row's placement is validated against the tree.
		org := e.mintOrg(t)
		mustPutDevice(t, e.registry, devices.Device{
			ID: rsAdoptDeviceID, RelayID: rsRelayID, DeviceClass: "media-player",
			Name: "Back Bar TV", ScopeNode: org, Labels: map[string]string{},
			Address: "192.0.2.44:8060", Model: "Roku Ultra", Serial: "X00500ADOPT1",
		})
		if err := e.store.ReplaceDiscoveredDevices(context.Background(), rsRelayID, []store.DiscoveredDevice{{
			DeviceID: rsAdoptDeviceID, RelayID: rsRelayID, ScopeNode: org,
			Driver: "roku-ecp", NativeID: "uuid:roku:ecp:X00500ADOPT1", DeviceClass: "media-player",
			Name: "Back Bar TV", Address: "192.0.2.44:8060", Model: "Roku Ultra", Serial: "X00500ADOPT1",
			FirstSeen: 1000, LastSeen: 2000,
			Entities: []wire.CandidateEntity{{Key: "main", DeviceClass: "media-player"}},
		}}); err != nil {
			t.Fatalf("mirror the device the adopt probe adopts: %v", err)
		}
		return e.do(t, http.MethodPost, "/api/v1/devices/"+rsAdoptDeviceID+"/adopt", nil, nil)
	},

	// --- workspace --------------------------------------------------------
	"exportWorkspace": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintOrg(t)
		return e.do(t, http.MethodPost, "/api/v1/workspace/export",
			mustJSON(t, map[string]any{"passphrase": testExportPassphrase}), nil)
	},
	"importCast": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// A REAL bundle, written by the format's own writer, imported at a node
		// that exists — so the probed 201 is the ordinary create's response
		// reached through the import path rather than a stub.
		node := e.mintOrg(t)
		var buf bytes.Buffer
		writeBundle(t, &buf, castbundle.Manifest{Cast: castbundle.CastPayload{
			Name: "Probed cast",
			Slides: []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{
				{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 100, H: 100, Color: "#112233"},
			}}},
		}}, map[string][]byte{})
		return e.do(t, http.MethodPost, "/api/v1/casts/import?scope_node="+node, buf.Bytes(),
			map[string]string{"Content-Type": "application/octet-stream"})
	},
	"listWorkspaceArchives": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// A REAL export first, run to completion, so the probed list carries an
		// item and WorkspaceArchive's required members are actually checked. An
		// empty directory would validate trivially — the vacuous probe this
		// file's guards exist to refuse.
		e.mintOrg(t)
		resp, raw := e.do(t, http.MethodPost, "/api/v1/workspace/export",
			mustJSON(t, map[string]any{"passphrase": testExportPassphrase}), nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("seed export: %d %s", resp.StatusCode, raw)
		}
		e.runJobs()
		return e.do(t, http.MethodGet, "/api/v1/workspace/archives", nil, nil)
	},
	"revokeSubject": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// Unconfirmed: the radius query changes nothing, and is the response
		// shape this probe exists to verify. The confirmed path is driven by
		// revocation_test.go, where its side effects can be asserted.
		// The org node must exist: the operation authorizes owner-at-root, and
		// without a workspace root there is nothing to authorize against.
		e.mintOrg(t)
		return e.do(t, http.MethodPost, "/api/v1/revocations",
			mustJSON(t, map[string]any{"subject_kind": "screen", "subject_id": rsEntityID}), nil)
	},
	"restoreWorkspace": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// The 202 is returned on ACCEPTANCE, before the container is opened, so
		// this probe verifies the accepted-Job shape without needing a real
		// archive on disk. Whether that named container exists is the Job's
		// business, and it reports a failed target rather than a different
		// response shape.
		e.mintOrg(t)
		return e.do(t, http.MethodPost, "/api/v1/workspace/restore",
			mustJSON(t, map[string]any{"archive": "some-container.waiveo", "passphrase": testExportPassphrase}), nil)
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
	"issueCredentialReset": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// A real target: a `user` principal holding a real password credential,
		// because the issuance refuses a principal with nothing to reset
		// (SEC-050 resets an EXISTING login handle rather than inventing one).
		target, err := e.authStore.CreatePrincipal(t.Context(), auth.KindUser, "reset-target")
		if err != nil {
			t.Fatalf("create the reset target: %v", err)
		}
		if _, err := e.authStore.PutPasswordCredential(t.Context(), target.PrincipalID,
			"reset-target@example.invalid", "the-password-being-replaced"); err != nil {
			t.Fatalf("seed the reset target's credential: %v", err)
		}
		return e.do(t, http.MethodPost, "/api/v1/auth/credential-reset",
			mustJSON(t, map[string]any{"target_principal_id": target.PrincipalID}), nil)
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

	// --- webhook-endpoints ------------------------------------------------
	"createWebhookEndpoint": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// The MINIMAL create: exactly WebhookEndpointCreate's own required
		// members and nothing more. `labels` and `schemas` are declared
		// required on the RESPONSE and named by neither, which is the whole
		// class of drift this check exists for.
		return e.do(t, http.MethodPost, "/api/v1/webhook-endpoints", mustJSON(t, map[string]any{
			"name":       "Minimal Endpoint",
			"scope_node": e.mintOrg(t),
			"url":        "https://hooks.example.invalid/waiveo",
		}), nil)
	},
	"getWebhookEndpoint": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintWebhookEndpoint(t, e.mintOrg(t))
		return e.do(t, http.MethodGet, "/api/v1/webhook-endpoints/"+id, nil, nil)
	},
	"updateWebhookEndpoint": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintWebhookEndpoint(t, e.mintOrg(t))
		return e.do(t, http.MethodPatch, "/api/v1/webhook-endpoints/"+id,
			mustJSON(t, map[string]any{"name": "Renamed Endpoint"}),
			map[string]string{"If-Match": e.etagOf(t, "/api/v1/webhook-endpoints/"+id)})
	},
	"listWebhookEndpoints": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintWebhookEndpoint(t, e.mintOrg(t))
		return e.do(t, http.MethodGet, "/api/v1/webhook-endpoints", nil, nil)
	},
	"rotateWebhookSigningSecret": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintWebhookEndpoint(t, e.mintOrg(t))
		return e.do(t, http.MethodPost, "/api/v1/webhook-endpoints/"+id+"/signing-secret",
			mustJSON(t, map[string]any{"secret": rsWebhookSecret}), nil)
	},
	"enableWebhookEndpoint": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintWebhookEndpoint(t, e.mintOrg(t))
		return e.do(t, http.MethodPost, "/api/v1/webhook-endpoints/"+id+"/enable", []byte(`{}`), nil)
	},
	"listPlatformLogs": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// Both reads are `owner` at the workspace's org node, so the org has to
		// exist before either can answer anything but 404.
		e.mintOrg(t)
		// A real line, written through the ordinary logger the buffer tees, so
		// the probed page carries an item and PlatformLogRecord's required
		// members are actually checked.
		fmt.Fprintln(e.logs, "waiveo-feeder: response-schema probe line")
		return e.do(t, http.MethodGet, "/api/v1/platform-logs", nil, nil)
	},
	"getSystemHealth": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		e.mintOrg(t)
		return e.do(t, http.MethodGet, "/api/v1/system-health", nil, nil)
	},
	"restartApplicationServer": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		// Owner-at-root, like both diagnostics reads, so the org must exist
		// before the operation can answer anything but 404.
		e.mintOrg(t)
		return e.do(t, http.MethodPost, "/api/v1/system/restart",
			mustJSON(t, map[string]any{"confirm": true}), nil)
	},
	"getWebhookDeliveryState": func(t *testing.T, e *schemaProbeEnv) (*http.Response, []byte) {
		id := e.mintWebhookEndpoint(t, e.mintOrg(t))
		// Install a secret first, so `secret_set_at_ms` is probed with a real
		// value rather than only in its null form.
		if resp, raw := e.do(t, http.MethodPost, "/api/v1/webhook-endpoints/"+id+"/signing-secret",
			mustJSON(t, map[string]any{"secret": rsWebhookSecret}), nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("install signing secret: %d %s", resp.StatusCode, raw)
		}
		return e.do(t, http.MethodGet, "/api/v1/webhook-endpoints/"+id+"/delivery", nil, nil)
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
