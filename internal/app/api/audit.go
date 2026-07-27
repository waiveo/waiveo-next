package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// audit.go emits this surface's audit trail: exactly one events/1 `audit.event`
// (EVT-080) per mutating api/1 request, from ONE middleware seam rather than a
// call planted inside each handler.
//
// # Why a middleware, and not the store's post-commit hook
//
// The store's post-commit hook (store.OnCommit) is the obvious candidate — it
// sits downstream of every write and no handler can route around it. It is
// nonetheless the wrong seam for an audit record, for four reasons, each one
// independently disqualifying:
//
//   - It carries no actor. The hook's type is `func()`: no arguments, no
//     context, so nothing about the authenticated principal can reach it.
//     `actor_principal` is EVT-080's first required field, and identity lives
//     ABOVE the store by design (the store has never known who is calling it).
//   - It never fires for a REFUSED mutation. A write turned away at
//     authorization (403), body validation (422), a failed precondition (412) or
//     an unresolvable id (404) opens no transaction at all. EVT-083 is explicit
//     that "a failed action is exactly as auditable as a successful one, never
//     elided for having failed" — auditing from the commit path would record
//     only the mutations that SUCCEEDED, which is precisely the half of the
//     trail an operator investigating a probe or an intrusion needs least.
//   - It never fires for a mutation that is not a store write. Dispatching a
//     device command (POST /entities/{id}/commands) changes the state of a
//     physical screen and an upload adds bytes to the content origin; neither
//     opens a store transaction. Job writes bypass the hook deliberately too.
//   - It carries no subject. The hook does not report which kind, id or scope
//     node committed, so EVT-012's placement could only be recovered by
//     re-reading the store on every commit and diffing it.
//
// The middleware has all four: the principal (the auth middleware it sits
// inside has already resolved one), the outcome (the response status), the
// subject (the route), and that subject's placement (read from the row itself).
//
// And it is unforgettable in the dimension that actually matters here. It wraps
// the MUX, not a handler, so a route registered tomorrow is audited by
// construction — there is no per-handler call for the next handler to omit,
// which is the failure this seam exists to prevent. The CRUD families are
// picked up from the very `mount` call that registers their routes (api.go), so
// even the subject metadata cannot drift out of step with the routing table.
// A mutating route this file has never heard of still records: auditRouteOf
// falls through to a generic family/verb reading of the path rather than
// returning "not audited".
//
// The store's hook is left exactly as it is, still nudging relays on a
// generation advance. Nothing here needs to chain onto it.
//
// # Scope: which mutations are audited
//
// EVT-081's mandatory list is a FLOOR ("at least each of the following"), and
// two of its members are served by this mux — pack install/remove, and the
// privileged operations reachable over an administrative binding. Auditing
// every mutating path rather than only the enumerated ones is deliberately
// wider than the floor: an audit trail that covers "the operations someone
// remembered to enumerate" is one enumeration mistake away from a gap, and
// EVT-081 permits the superset outright.
//
// Failures are audited alongside successes, per EVT-083 above. A refused
// mutation is frequently the more interesting record of the two.
//
// # Which requests are NOT audited here
//
// Two exclusions, both narrow:
//
//   - The auth family (/api/v1/auth/...). Those handlers emit their OWN
//     SEC-150 records, with the flow-specific fields this middleware cannot see
//     (a credential id, a grant's purpose and issued_via, the clock assessment
//     SEC-091 attaches to a login failure). Auditing them here as well would
//     double-record every logout under a vaguer action name.
//   - Requests that never resolve a principal. This middleware sits INSIDE
//     auth.Middleware, so a request refused for bad or absent credentials never
//     reaches it. That placement is not merely convenient: EVT-080 types
//     `actor_principal` as a required ULID, so a request with no authenticated
//     identity cannot form a valid audit.event at all. Authentication failures
//     are the auth package's own records (login.failure/lockout, SEC-150), which
//     is where the identity that failed is actually known.
const (
	// auditVerbCreate/Update/Delete name the three CRUD acts.
	auditVerbCreate = "create"
	auditVerbUpdate = "update"
	auditVerbDelete = "delete"
)

// auditRoute is the static, method-and-path-derived description of one mutating
// request: what act it performs, and on which resource.
//
// action and resourceType are both built from api/1's OWN resource-type tag
// (resourceConfig.resourceType — "schedules", "scope-nodes", "packrow"), never
// from a second vocabulary invented for the audit trail. That tag is already
// what external_id uniqueness and pagination cursors are scoped by, so an audit
// record names a resource family by the same string the rest of the surface
// does, and a reader correlating a record back to an operation does not have to
// learn a translation table.
type auditRoute struct {
	// action is EVT-080's `action`: a short, stable `<resource-type>.<verb>`
	// identifier, in the dotted style the auth package's own actions use.
	action string
	// resourceType is the `<resource-type>` half of EVT-080's
	// `<resource-type>:<id>` target form.
	resourceType string
	// id is the `<id>` half, when the path names one. Empty for a create (the
	// server has not minted it yet) and for a collection-wide operation.
	id string
	// kind is the store table this subject's placement is read from, when the
	// subject is a generic CRUD resource. The zero value means "not a store
	// row" — an entity, a pack, an upload.
	kind store.Kind
	// placement projects a stored row's body onto its scope-node placement. It
	// is the kind's OWN projection, taken from the resourceConfig, so the audit
	// record files a row at the same node the read filter and the write
	// authorization already use for it (a scope node is placed at itself, a
	// scheduling row at its scope_node).
	placement func(resourceFields) string
	// created marks an operation whose subject does not exist until the handler
	// has run: its id and placement come from the RESPONSE, not the path.
	created bool
	// entity marks POST /entities/{id}/commands, whose subject's placement lives
	// in the device registry rather than the store.
	entity bool
	// packID and collection identify a pack-collection row's subject, which is
	// keyed by a (pack, collection, entity_id) triple rather than a store Kind.
	packID     string
	collection string
	// packRow marks that triple as present.
	packRow bool
}

// auditMutations wraps next so every mutating request through it records one
// `audit.event`. Non-mutating requests, and the excluded families documented at
// the top of this file, pass through untouched.
//
// A nil auditor (a deployment or a unit test constructed without an event sink)
// degrades to a plain pass-through rather than to a panic: the Auditor type is
// nil-safe and silent by the same design, and an audit sink is a deployment
// collaborator, not a precondition for serving a request.
func (srv *server) auditMutations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if srv.auditor == nil || !auditedMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		rt, ok := srv.auditRouteOf(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		// The subject's placement is read BEFORE the handler runs. That ordering
		// is required, not incidental: a DELETE's subject is gone by the time the
		// handler returns, and a PATCH may have MOVED its subject to a different
		// node — and the node a mutation should be filed at is the one it acted
		// on, which is the one it had on the way in.
		node := srv.auditSubjectNode(r, rt, rt.id)

		rec := &auditRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		// A create's subject did not exist on the way in. Its server-minted id
		// comes from the response the handler just wrote, and its placement from
		// the row that now exists.
		id := rt.id
		if rt.created {
			if newID := rec.createdID(); newID != "" {
				id = newID
				node = srv.auditSubjectNode(r, rt, id)
			}
		}

		srv.auditor.Emit(auth.Record{
			TraceID: apihttp.TraceID(r),
			Actor:   auth.PrincipalID(r),
			Action:  rt.action,
			Target:  auditTarget(rt.resourceType, id),
			Result:  auditResult(rec.status),
			// An empty node defers to the Auditor's own deployment node, which is
			// the correct filing for a subject that has no placement of its own —
			// an installed pack, a content-addressed upload, a fleet-wide
			// operation — and for a subject that could not be resolved at all (a
			// 404's unknown id, a refused create that never landed). An
			// unresolvable subject is exactly the case where no narrower placement
			// is KNOWN, and inventing one would file the record somewhere it does
			// not belong.
			ScopeNode: node,
		})
	})
}

// auditedMethod reports whether a request method mutates. PUT is included even
// though this surface currently exposes none: the cost of listing it is nothing,
// and the cost of omitting it is an unaudited route the day one is added.
func auditedMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// auditResult maps a response status onto EVT-080's closed result enum. Any 2xx
// is a success; everything else — a refusal, a validation rejection, a failed
// precondition, an internal error — is a failure that EVT-083 requires to be
// recorded in full rather than dropped.
func auditResult(status int) string {
	if status >= 200 && status < 300 {
		return events.AuditResultSuccess
	}
	return events.AuditResultFailure
}

// auditTarget renders EVT-080's `<resource-type>:<id>` target form. An operation
// naming no single id — a rejected create that never minted one, a fleet-wide
// bulk operation — still yields a NON-EMPTY target, because EVT-083 requires a
// failure to carry every field a success does and validateAuditEvent enforces
// it. The bare resource type is the honest reading in that case: the act was
// against the collection, and no narrower subject exists to name.
func auditTarget(resourceType, id string) string {
	if id == "" {
		return resourceType
	}
	return resourceType + ":" + id
}

// auditRouteOf derives the audit route from a request's method and path, and
// reports whether the request is audited here at all.
//
// The default arm is the important one: a path naming no family this file knows
// still produces a record, from the family segment and the method's verb. A
// future route mounted without a thought for auditing therefore degrades to a
// COARSER record, never to a missing one.
func (srv *server) auditRouteOf(r *http.Request) (auditRoute, bool) {
	rest := strings.TrimPrefix(r.URL.Path, apiPrefix+"/")
	if rest == r.URL.Path || rest == "" {
		return auditRoute{}, false
	}
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		return auditRoute{}, false
	}
	family := segs[0]

	switch {
	// The auth family records itself (SEC-150) — see this file's header.
	case family == "auth":
		return auditRoute{}, false

	// POST /content — a content-addressed upload. Its "id" is the server-computed
	// asset_ref in the response, and it carries no scope-node placement of its
	// own (content is addressed by its own hash, not placed in the tree).
	case family == "content" && len(segs) == 1:
		return auditRoute{action: "content.upload", resourceType: "content", created: true}, true

	// POST /entities/{id}/commands — a command dispatched at a physical device.
	case family == "entities" && len(segs) == 3 && segs[2] == "commands":
		return auditRoute{
			action: "entities.command", resourceType: "entities", id: segs[1], entity: true,
		}, true

	case family == "packs":
		return packAuditRoute(r, segs)
	}

	// The generic CRUD families, taken from the same registry `mount` populates.
	if cfg, ok := srv.families[family]; ok {
		return crudAuditRoute(r, cfg, segs)
	}

	return auditRoute{
		action:       family + "." + auditVerbOf(r.Method),
		resourceType: family,
		id:           pathID(segs),
	}, true
}

// crudAuditRoute derives the route for one of the families mounted by
// srv.mount: the five CRUD operations plus any action-style POST hanging off
// the same base path.
func crudAuditRoute(r *http.Request, cfg resourceConfig, segs []string) (auditRoute, bool) {
	rt := auditRoute{
		resourceType: cfg.resourceType,
		kind:         cfg.kind,
		placement:    cfg.placement,
	}
	switch {
	// POST /<family> — a create. The subject's id is minted by the server.
	case len(segs) == 1 && r.Method == http.MethodPost:
		rt.action = cfg.resourceType + "." + auditVerbCreate
		rt.created = true

	// POST /<family>/<verb> — a collection-wide action (automations/bulk-enable).
	// The literal segment is the act's own name, and no single row is its
	// subject, so it keeps the bare resource type as its target.
	case len(segs) == 2 && r.Method == http.MethodPost:
		rt.action = cfg.resourceType + "." + segs[1]

	// PATCH|DELETE /<family>/{id} — a mutation of one existing row.
	case len(segs) == 2:
		rt.action = cfg.resourceType + "." + auditVerbOf(r.Method)
		rt.id = segs[1]

	// POST /<family>/{id}/<verb> — a per-row action (automations/{id}/run).
	case len(segs) == 3 && r.Method == http.MethodPost:
		rt.action = cfg.resourceType + "." + segs[2]
		rt.id = segs[1]

	default:
		rt.action = cfg.resourceType + "." + auditVerbOf(r.Method)
		rt.id = pathID(segs)
	}
	return rt, true
}

// packAuditRoute derives the route for the declarative-packs surface. A pack id
// is two path segments (<publisher>/<name>, MAN-001), so the offsets differ from
// every other family and are spelled out here rather than folded into the
// generic reading.
func packAuditRoute(r *http.Request, segs []string) (auditRoute, bool) {
	switch {
	// POST /packs — a pack install. EVT-081 names this one explicitly.
	case len(segs) == 1 && r.Method == http.MethodPost:
		return auditRoute{action: "packs.install", resourceType: "packs", created: true}, true

	// DELETE /packs/{publisher}/{name} — a pack removal, likewise named by
	// EVT-081. A pack is a workspace-level artifact with no scope-node placement
	// of its own; the record files at the deployment's node (auditNodeOf).
	case len(segs) == 3 && r.Method == http.MethodDelete:
		return auditRoute{action: "packs.remove", resourceType: "packs", id: segs[1] + "/" + segs[2]}, true

	// POST /packs/{publisher}/{name}/data/{collection} — a collection row create.
	case len(segs) == 5 && segs[3] == "data" && r.Method == http.MethodPost:
		return auditRoute{
			action: packRowResourceType + "." + auditVerbCreate, resourceType: packRowResourceType,
			packRow: true, packID: segs[1] + "/" + segs[2], collection: segs[4], created: true,
		}, true

	// PATCH|DELETE /packs/{publisher}/{name}/data/{collection}/{entity_id}.
	case len(segs) == 6 && segs[3] == "data":
		return auditRoute{
			action: packRowResourceType + "." + auditVerbOf(r.Method), resourceType: packRowResourceType,
			id:      segs[5],
			packRow: true, packID: segs[1] + "/" + segs[2], collection: segs[4],
		}, true
	}
	return auditRoute{
		action: "packs." + auditVerbOf(r.Method), resourceType: "packs", id: pathID(segs),
	}, true
}

// auditVerbOf maps a mutating method onto its audit verb.
func auditVerbOf(method string) string {
	switch method {
	case http.MethodPost:
		return auditVerbCreate
	case http.MethodPatch, http.MethodPut:
		return auditVerbUpdate
	case http.MethodDelete:
		return auditVerbDelete
	default:
		return strings.ToLower(method)
	}
}

// pathID returns the last path segment as a best-effort subject id, for the
// fallback arms that have no structural reading of the route.
func pathID(segs []string) string {
	if len(segs) < 2 {
		return ""
	}
	return segs[len(segs)-1]
}

// auditSubjectNode resolves the EVT-012 scope-node placement an audit record is
// filed at: "the scope node the event's SUBJECT resource — the entity, rule,
// screen, device, or principal the event is about — is itself placed under". It
// returns "" when the subject has no placement to read, leaving the fallback to
// the Auditor (see the call site).
//
// This is deliberately NOT the deployment's own node for every record. Scope
// filtering on the event stream is applied per event against the subscriber's
// visible set (EVT-120/123), so the placement decides who can see the record: a
// mutation of a row under Site A filed at the workspace node would be invisible
// to the Site A operator whose own screens it changed, and visible to a Site B
// operator with no authority over it. Reading the placement off the subject
// itself is what makes the audit trail visible to exactly the principals who can
// see the thing it is about.
//
// Every lookup here is a READ of state the request is already touching; an error
// is answered "" (fall back), never surfaced — an audit record must not be able
// to fail the request it observes.
func (srv *server) auditSubjectNode(r *http.Request, rt auditRoute, id string) string {
	if id == "" {
		return ""
	}
	switch {
	case rt.entity:
		if srv.devices == nil {
			return ""
		}
		entity, found := srv.devices.Entity(id)
		if !found {
			return ""
		}
		return entity.ScopeNode

	case rt.packRow:
		row, found, err := srv.store.GetPackRow(r.Context(), rt.packID, rt.collection, id)
		if err != nil || !found {
			return ""
		}
		return row.ScopeNode

	case rt.kind != "" && rt.placement != nil:
		res, found, err := srv.store.Get(r.Context(), rt.kind, id)
		if err != nil || !found {
			return ""
		}
		return rt.placement(parseFields(res.Body))
	}
	return ""
}

// auditRecorder wraps the ResponseWriter to observe what the handler answered:
// the status (which becomes EVT-080's result) and, for a create, enough of the
// response to recover the server-minted id.
//
// Headers pass THROUGH to the real writer rather than being buffered, so a
// handler's ETag and Location reach the client unchanged and are readable here
// afterwards. Only a create's body is retained, and only up to a small cap —
// the ids this needs live at the front of a small JSON object, and an audit
// record must not turn a large response into a large allocation.
type auditRecorder struct {
	http.ResponseWriter
	status int
	body   []byte
}

// auditBodyCap bounds the response bytes retained for id recovery. A created
// resource's JSON identity fields sit well inside this; anything past it is
// irrelevant to the record.
const auditBodyCap = 4096

func (rec *auditRecorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *auditRecorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if room := auditBodyCap - len(rec.body); room > 0 {
		if len(b) < room {
			room = len(b)
		}
		rec.body = append(rec.body, b[:room]...)
	}
	return rec.ResponseWriter.Write(b)
}

// createdID recovers the server-minted id of a resource the handler just
// created, from the response it wrote.
//
// The Location header is consulted first because every create on this surface
// sets it (api.go, packs_data.go) and its last segment IS the new id — no body
// parsing, no per-family knowledge. The body is the fallback for the creates
// that answer with an identity but no Location: a content upload's asset_ref, a
// bulk operation's Job id.
func (rec *auditRecorder) createdID() string {
	if loc := rec.Header().Get("Location"); loc != "" {
		if i := strings.LastIndex(loc, "/"); i >= 0 && i+1 < len(loc) {
			return loc[i+1:]
		}
	}
	var m struct {
		ID       string `json:"id"`
		EntityID string `json:"entity_id"`
		PresetID string `json:"preset_id"`
		AssetRef string `json:"asset_ref"`
	}
	if err := json.Unmarshal(rec.body, &m); err != nil {
		return ""
	}
	for _, v := range []string{m.ID, m.EntityID, m.PresetID, m.AssetRef} {
		if v != "" {
			return v
		}
	}
	return ""
}
