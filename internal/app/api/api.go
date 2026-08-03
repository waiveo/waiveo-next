// Package api is the app-side api/1 HTTP surface for the authoring loop: the
// management door through which scope-nodes and the scheduling-core resources
// (schedules, dayparts, playlists) are authored over HTTP, persisted by
// internal/app/store, and thereby fed into the feeder's signed desired-state.
//
// Every handler routes through the already-built, corpus-driven api/1 convention
// helpers — it never re-derives one:
//
//   - optimistic concurrency (ETag/If-Match) via apihttp.ETag / CheckIfMatch;
//   - keyset pagination (opaque cursor + {items, cursor} envelope) via
//     apihttp.ParsePageParams / DecodeCursor / Page;
//   - the label-selector grammar via apiselector.Parse / Selector.Matches;
//   - the client-assignable external_id convention via apihttp.CheckExternalIDUnique;
//   - Idempotency-Key semantics on create via apihttp.IdempotencyStore;
//   - the RFC 9457 Problem shape (every error) + Trace-Id propagation via
//     apihttp.WriteProblem / WriteProblemExt / WithTraceID.
//
// Every request is authenticated and authorized before a handler sees it
// (internal/app/auth, security-model/1): the principal an Idempotency-Key is
// scoped by (API-051) and a Job's created_by is stamped from (API-112) is the
// REAL authenticated caller, resolved from a live session or API key. There is
// no fallback identity — SEC-005 requires a route that cannot resolve an
// authorization decision to refuse rather than default-permit, so a request that
// reaches a handler always carries a principal and a handler never has to invent
// one.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/packsig"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
)

// contentTypeJSON is the media type every success response body uses; error
// bodies use apihttp.ProblemContentType instead.
const contentTypeJSON = "application/json"

// apiPrefix is the version-scoped mount point every resource path hangs off.
const apiPrefix = "/api/v1"

// server holds the dependencies every resource handler shares: the store the
// resources live in, the idempotency store guarding create replays, and the
// injected clock (epoch ms) the idempotency Begin/Complete calls are timestamped
// with — so the api layer, like the idempotency store itself, reads no wall
// clock of its own. newID is likewise injected rather than read from a
// package-level generator: the api layer mints no id from a generator of its
// own, every server-minted id (a create's server-assigned id, a synchronous
// run's run_id, a bulk-enable Job's id) comes from this single seam. content
// is the shared content-addressed origin store the upload endpoint writes
// into (and the feeder serves GET /content/<hex> from over the SAME
// instance); contentBase is the feeder's own content-origin base URL the
// upload's returned url is built from (<base>/content/<hex>, snapshot.Build's
// form).
type server struct {
	store       *store.Store
	idem        *apihttp.IdempotencyStore
	nowMs       func() int64
	newID       func() string
	content     *origin.Store
	contentBase string
	// installer runs the signature-gated, manifest-gated declarative-pack
	// install pipeline the POST /api/v1/packs handler drives
	// (internal/app/packs). It shares the same store every other resource
	// handler writes through, and verifies every artifact against packTrust.
	installer *packs.Installer
	// packTrust is the trust-anchor source pack-artifact signatures verify
	// against (marketplace/1 MKT-009b), wired by WithPackTrust. Left nil, the
	// install pipeline fails closed: every artifact refuses (unsigned as
	// PACK_UNSIGNED, signed as PACK_SIGNER_UNTRUSTED) — an unwired deployment
	// can never install a pack, which is refusal, never default-permit.
	packTrust packsig.TrustAnchors
	// market is the configured registry a pack install BY MARKETPLACE REFERENCE
	// resolves through (marketplace/1 MKT-060), wired by WithMarketplace. Left
	// nil, the artifact-upload route is unaffected and a marketplace reference
	// refuses: a deployment with no configured registry has nothing to resolve
	// against and must not invent one.
	market *packs.Market
	// devices is the device plane's read model (adopted devices and the entities
	// they expose) the devices/entities list operations serve, and the resolver a
	// command's target entity is looked up in; dispatch carries that command down
	// the owning relay's persistent connection. Both are optional (WithDevicePlane)
	// — the routes mount either way, see devices.go.
	devices  *devices.Registry
	dispatch CommandDispatcher
	// jobs executes the work an async operation accepts with 202 (jobrun.go).
	// Always non-nil: New builds and starts one when the caller wires none, so
	// an accepted Job is never a promise nothing is working on.
	jobs *JobRunner
	// auditor emits this surface's events/1 `audit.event` records (audit.go). It
	// is taken from the REQUIRED authenticator rather than wired separately, so
	// the api layer and the auth flows record through one sink (SEC-150: no
	// second audit trail) and there is no additional wiring step a deployment
	// can omit. May be nil — an Auditor is nil-safe and silent.
	auditor *auth.Auditor
	// authn is the REQUIRED authenticator itself, retained because one operation
	// needs more from it than the middleware does: the data-subject delete
	// (workspacerun.go) destroys every principal, credential and session through
	// authn.Store(), which is SEC-121's "force fresh enrollment on every
	// principal". Holding the authenticator rather than a second handle on its
	// store keeps that destruction pointed at the SAME store the middleware
	// authenticates against — two handles is how a deployment ends up erasing
	// one and authenticating from the other.
	authn *auth.Authenticator
	// workspaceArchive is where the data-subject export writes its archive/1
	// container, and the workspace signing key it signs the header with
	// (workspacerun.go). Optional: without it the export route still mounts and
	// still authorizes, and answers UNAVAILABLE.
	workspaceArchive *WorkspaceArchive
	// emergencyKit, when set, makes a successful claim issue one (ARC-110).
	emergencyKit *EmergencyKitConfig
	// webhookSecrets seals and opens a registered webhook endpoint's signing
	// secret (internal/app/webhookdeliver). Optional (WithWebhookSecrets): the
	// registration routes mount either way, and without it the rotate operation
	// answers UNAVAILABLE rather than storing a secret in the clear — the same
	// posture the auth store takes for a recoverable credential secret with no
	// sealer.
	webhookSecrets WebhookSecretSealer
	// webhookRotationOverlap is the window a rotation's superseded secret stays
	// acceptable for (EVT-158), in ms. It exists here only so a rotation's
	// response can name the instant the sender ACTUALLY stops emitting the prior
	// signature: a published window the delivery loop did not honour would be
	// worse than publishing none. Zero means the contract's proposed default.
	webhookRotationOverlap int64
	// families is the CRUD resource registry the audit middleware reads a
	// request's subject metadata out of, keyed by URL path segment. It is
	// populated by mount() itself, so a family's audit identity and its routes
	// are registered by one call and cannot drift apart.
	families map[string]resourceConfig
	// pairingRelays is the relay directory the pairing-code issuance forms
	// codes from (pairingcode.go). Optional (WithPairing): the route mounts,
	// mints and delivers the grant either way, and answers with an honest
	// code_unavailable_reason when unwired.
	pairingRelays PairingRelayDirectory
}

// authExemptPaths are the api/1 operations that declare their own
// `security: []` override in api/openapi.yaml (API-090) and so are mounted
// AHEAD of the auth middleware rather than behind it. The list is deliberately
// short, explicit, and kept here beside the mount that honours it, so "which
// routes are reachable unauthenticated" is one readable set rather than a
// property emergent from mux pattern precedence.
//
// All three entries are credential-exchange operations under API-091: a login
// mints the session a caller does not yet hold, the first-boot claim mints the
// very first principal on the box, and a credential-reset REDEMPTION is
// performed by the one person who by definition cannot present a session — the
// user whose credential is being reset (SEC-050). Requiring a pre-existing
// session for any of the three would be circular.
//
// Note which half of the reset is here and which is not: the ISSUING route
// (`POST /auth/credential-reset`) is absent from this set and is authenticated
// like any other mutating operation, because the admin issuing it is signed in.
// Only the redemption is exempt. Everything else under /api/v1 — including
// logout and the session read — is authenticated, which is what subjects logout
// to the same CSRF discipline as any other mutating browser route (SEC-024).
var authExemptPaths = map[string]bool{
	apiPrefix + "/auth/login":                   true,
	apiPrefix + "/auth/setup":                   true,
	apiPrefix + "/auth/credential-reset/redeem": true,
}

// authExempt reports whether r names one of the `security: []` operations.
func authExempt(r *http.Request) bool { return authExemptPaths[r.URL.Path] }

// router registers a handler on an http.ServeMux and, in the SAME call,
// records the method+path pattern it was registered under.
//
// It exists so the surface this package actually SERVES is enumerable from the
// very statements that serve it. api/openapi.yaml is the declared surface every
// generated client is produced from, and nothing but a reader's memory used to
// hold the two together: an operation could be declared with no handler behind
// it (a generated client method that 404s), or a handler mounted with no
// declaration (a route no client can discover and no schema constrains).
// surface_test.go closes that by diffing these recorded patterns against the
// document's operations, in both directions.
//
// Recording inside the registration call is the whole point. A hand-kept list
// of patterns beside the mounts would itself be a third thing to drift — the
// exact failure mode the check exists to remove — whereas a pattern that was
// never registered here was never served, and one that was cannot be omitted.
type router struct {
	mux      *http.ServeMux
	patterns []string
}

// newRouter builds a router over a fresh mux.
func newRouter() *router { return &router{mux: http.NewServeMux()} }

// HandleFunc registers h under pattern (Go 1.22+ "METHOD /path" form) and
// records the pattern as part of the served surface.
func (rt *router) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	rt.patterns = append(rt.patterns, pattern)
	rt.mux.HandleFunc(pattern, h)
}

// New builds the api/1 HTTP handler: a /api/v1-prefixed mux exposing the
// scope-nodes and scheduling-core (schedules/dayparts/playlists) CRUD
// operations over st, plus the content-addressed asset upload (POST
// /api/v1/content) over content, wrapped in apihttp.WithTraceID so every request
// resolves a Trace-Id once (echoed by the header and every Problem body). idem
// guards Idempotency-Key create replays; nowMs is the injected clock the
// idempotency calls are timestamped with; newID is the injected id source
// every server-minted id (a create's server-assigned id, a synchronous run's
// run_id, a bulk-enable Job's id) is drawn from — the same seam pattern as
// nowMs, so the api layer reads no wall clock AND mints no id of its own.
// content is the shared origin store the feeder also serves GET /content/<hex>
// from (one instance, so an upload is immediately servable); contentBase is
// the feeder's content-origin base URL the upload's returned url is built
// from. opts wire the optional collaborators — currently the device plane
// (WithDevicePlane), whose routes mount whether or not it is supplied.
//
// authn is REQUIRED, not an option, and that is the point: it is a positional
// parameter precisely so a caller cannot construct this handler without
// deciding who may reach it. Every route below hangs behind its middleware
// except the two credential-exchange operations in authExemptPaths, so an
// unauthenticated request to any resource family is refused before a handler
// runs (SEC-005).
func New(st *store.Store, idem *apihttp.IdempotencyStore, nowMs func() int64, newID func() string, content *origin.Store, contentBase string, authn *auth.Authenticator, opts ...Option) http.Handler {
	srv := &server{
		store: st, idem: idem, nowMs: nowMs, newID: newID, content: content, contentBase: contentBase,
		auditor:  authn.Auditor(),
		authn:    authn,
		families: map[string]resourceConfig{},
	}
	for _, opt := range opts {
		opt(srv)
	}
	// Built AFTER the options so WithPackTrust's anchors and WithMarketplace's
	// sources both reach the pipeline. A caller that wired no anchors gets a
	// fail-closed installer (see packTrust); one that wired no registry gets an
	// installer that only accepts a directly-supplied artifact.
	srv.installer = packs.NewInstaller(st, srv.packTrust, packs.WithMarketplace(srv.market))
	// A handler built without WithJobRunner still EXECUTES the work its async
	// operations accept — it just does not hand anyone the lifecycle. The
	// default is started here rather than left stopped so that forgetting the
	// option degrades to "no graceful shutdown", never to "202 Accepted and
	// nothing ever happens", which is the failure this whole path exists to
	// remove.
	if srv.jobs == nil {
		srv.jobs = NewJobRunner()
		srv.jobs.Start()
	}
	// API-116's resume, scheduled as the FIRST work this handler's runner will
	// do: every target an earlier process committed as `running` and never
	// carried to a terminal value is re-applied or reconciled (jobrun.go). It
	// takes its inventory HERE, before any request has been served, which is what
	// keeps it pointed at an earlier process's abandoned work rather than at this
	// one's live executions.
	srv.scheduleResume()
	rt, rootRT := newRouter(), newRouter()
	authHandlers := auth.NewHandlers(authn, nil, auth.RootScopeNode)
	if srv.emergencyKit != nil {
		authHandlers = authHandlers.WithEmergencyKit(srv.emergencyKit.Dir, srv.emergencyKit.WorkspaceID, srv.emergencyKit.NewID)
	}
	srv.mountAll(rt, rootRT, authHandlers)
	// The audit seam sits INSIDE the auth middleware and OUTSIDE the resource
	// mux: inside, because a record needs the resolved principal EVT-080 requires
	// (a request refused for bad credentials has no identity to attribute and is
	// the auth package's own record anyway); outside, because wrapping the mux
	// rather than each handler is what makes the next route mounted here audited
	// without anyone remembering to audit it (audit.go).
	//
	// The catch-all is registered on the root MUX directly rather than through
	// the router: it is the fall-through into the authenticated mux, not an
	// api/1 operation, and recording it would put a path no operation declares
	// into the served surface.
	rootRT.mux.Handle("/", authn.Middleware(auth.APICodes, authExempt)(srv.auditMutations(rt.mux)))

	return apihttp.WithTraceID(rootRT.mux)
}

// mountAll registers every api/1 operation this package serves. rt is the
// AUTHENTICATED mux; rootRT carries the two credential-exchange operations that
// ride ahead of the auth middleware (API-090/091).
//
// It is deliberately the ONE place the served surface is defined. New calls it
// to build the handler and surface_test.go calls it to enumerate what was
// registered, so "what this server serves" has a single answer that both the
// running process and the drift check read from the same statements. A route
// added to the running server and not to the enumeration is not possible,
// because there is only one enumeration and it IS the registration.
//
// authHandlers is used only for the method values the four auth routes are
// registered with; nothing here calls them, so the surface test may pass nil.
func (srv *server) mountAll(rt, rootRT *router, authHandlers *auth.Handlers) {
	srv.mount(rt, scopeNodesConfig())
	srv.mount(rt, schedulesConfig())
	srv.mount(rt, daypartsConfig())
	srv.mount(rt, playlistsConfig())
	srv.mount(rt, automationsConfig())
	// The two data-model/1 identity kinds (identityrows.go). They are ordinary
	// resourceConfig mounts on purpose: a screen and an adopted device are
	// authored rows with a revision to condition a write on, unlike the relay's
	// live device/entity read model below.
	srv.mount(rt, screensConfig())
	srv.mount(rt, adoptedDevicesConfig())
	// The outbound-webhook registration family plus the three operations a
	// registered endpoint needs that plain CRUD does not express — installing or
	// rotating its signing secret, re-enabling it after an auto-disable, and
	// reading its delivery state (webhookendpoints.go).
	srv.mountWebhookEndpoints(rt)
	srv.mountPacks(rt)
	// The automations family adds two operations beyond plain resource CRUD: a
	// synchronous per-automation run (openapi runAutomation) and a selector-targeted
	// fleet-mutating bulk enable/disable returning an api/1 Job (bulkEnableAutomations).
	// The literal `bulk-enable` segment is unambiguous — the generic mount registers
	// no POST on `automations/{id}` — and `{id}/run` has its own two-segment shape.
	rt.HandleFunc("POST "+apiPrefix+"/automations/{id}/run", srv.runAutomation)
	rt.HandleFunc("POST "+apiPrefix+"/automations/bulk-enable", srv.bulkEnableAutomations)
	// The screens family adds one operation beyond plain resource CRUD: minting
	// a screen-bound pairing grant and forming its human-enterable pairing
	// code (pairingcode.go). `{screen_id}/pairing-code` has the same
	// two-segment per-row action shape `{id}/run` has.
	rt.HandleFunc("POST "+apiPrefix+"/screens/{screen_id}/pairing-code", srv.issuePairingCode)
	rt.HandleFunc("POST "+apiPrefix+"/content", srv.uploadContent)
	// The read half of the async convention: a Job returned by 202 is polled
	// here until its state is terminal (API-112, openapi getJob). It is not a
	// resourceConfig mount — a Job carries no revision to condition a write on,
	// and has no write operations at all (jobs.go).
	rt.HandleFunc("GET "+apiPrefix+"/jobs/{job_id}", srv.getJob)
	// The two data-subject operations (API-120-124, openapi exportWorkspace /
	// deleteWorkspace). They are not a resourceConfig mount and never could be:
	// their subject is the workspace as a whole, which has no id in the path, no
	// revision to condition a write on, and no collection to list (workspace.go).
	rt.HandleFunc("POST "+apiPrefix+"/workspace/export", srv.exportWorkspace)
	rt.HandleFunc("POST "+apiPrefix+"/workspace/delete", srv.deleteWorkspace)
	// The device plane's two read families and its one mutating operation. They
	// are not resourceConfig mounts: a device is a read-only projection of the
	// relay's own discovery and adoption plane, with no revision to condition a
	// write on (devices.go).
	srv.mountDevicePlane(rt)

	// The session-management half of the auth family rides the AUTHENTICATED
	// mux: both operations act on the caller's own live session, so neither has
	// anything to do without one.
	rt.HandleFunc("POST "+apiPrefix+"/auth/logout", authHandlers.Logout)
	rt.HandleFunc("GET "+apiPrefix+"/auth/session", authHandlers.Session)
	// Second-factor enrollment (security-model/1 SEC-004). Both operations act on
	// the CALLER'S OWN principal and mint no session, so they belong behind the
	// middleware with the rest of the authenticated surface — a caller with no
	// session has nothing to add a second factor to. They deliberately carry no
	// secure-context precondition: SEC-004 makes `totp` the floor that stays
	// available on the self-signed fallback, unlike `passkey` (SEC-102).
	rt.HandleFunc("POST "+apiPrefix+"/auth/totp/enroll", authHandlers.EnrollTOTP)
	rt.HandleFunc("POST "+apiPrefix+"/auth/totp/confirm", srv.withDeclaredMembers("TotpConfirmRequest", authHandlers.ConfirmTOTP))
	// The ISSUING half of the routine credential reset (security-model/1
	// SEC-050). Authenticated, and behind the middleware with everything else
	// that mutates: the admin issuing a reset for somebody else is signed in as
	// themselves, so there is nothing circular about requiring a session here.
	// SEC-012's `admin` floor is enforced inside the store's issuance function.
	rt.HandleFunc("POST "+apiPrefix+"/auth/credential-reset", srv.withDeclaredMembers("CredentialResetRequest", authHandlers.IssueCredentialReset))

	// The credential-exchange half (API-090/091) mounts on the ROOT mux, ahead
	// of the middleware. Go's ServeMux prefers the more specific method+path
	// pattern over the "/" catch-all, so these reach their handlers directly
	// while every other path falls through to the authenticated mux.
	rootRT.HandleFunc("POST "+apiPrefix+"/auth/login", srv.withDeclaredMembers("LoginRequest", authHandlers.Login))
	rootRT.HandleFunc("POST "+apiPrefix+"/auth/setup", srv.withDeclaredMembers("ClaimRequest", authHandlers.Claim))
	// The REDEEMING half of the credential reset. It is the third `security: []`
	// operation, and for API-091's own reason: the caller is the person who
	// cannot log in, so requiring a session would make the operation unreachable
	// by the only party SEC-050 allows to perform it. What stands in for a
	// session is the one-time code — 256 bits, hashed at rest, single-use under
	// SEC-036's atomic consume, and rate-limited under SEC-033 before the lookup.
	rootRT.HandleFunc("POST "+apiPrefix+"/auth/credential-reset/redeem", srv.withDeclaredMembers("CredentialResetRedeemRequest", authHandlers.RedeemCredentialReset))
}

// resourceConfig parameterizes the generic resource handler for one resource
// kind: which store table it lives in, its URL path segment, the api/1
// resource-type tag its external_id uniqueness and cross-references are scoped
// by, the human noun a Problem's `detail` prose names it by (displayName), and
// the three per-kind field projections the conventions need — the selectable
// label set (labels + any reserved intrinsic), the scope-node placement a
// selector's scope_node term evaluates against, and the grouping external_id
// uniqueness is scoped by. A scope node's placement is itself and its
// external_id groups by parent; a scheduling-core row's placement and
// external_id grouping are both its scope_node (see scopenodes.go / a follow-up).
type resourceConfig struct {
	kind         store.Kind
	path         string
	resourceType string
	// displayName is the singular human noun this kind is named by in a Problem's
	// `detail` prose (a 404's "No <displayName> exists..." and an external_id
	// conflict's "...another <displayName> under this parent.") — resourceType
	// itself is a matching key (a family tag like "scope-nodes", not always a
	// grammatical noun on its own), so the two are kept separate rather than
	// reusing one for the other.
	displayName string
	selLabels   func(resourceFields) map[string]string
	placement   func(resourceFields) string
	extScope    func(resourceFields) string
	// writeScope is the scope node a WRITE of this kind is authorized at: the
	// node the row is placed UNDER, which is where SEC-010 authority over the
	// placement has to come from. For every kind whose own scope_node is its
	// placement the two coincide; for a scope node they do not — its placement
	// is ITSELF (a subtree selector must select it), but on a create that id
	// does not exist in the tree yet and on a patch it is the PARENT that
	// decides where the node hangs, so the write is authorized at parent_id.
	//
	// This is deliberately its own projection rather than a reuse of extScope,
	// which today returns the same node for all five kinds. The coincidence is
	// not accidental — API-101 groups external_id by the node a row is placed
	// under, which is the same node authority over that placement is anchored at
	// — but "which rows may collide with mine" and "may I write here at all" are
	// different questions, and a kind that answered them differently would
	// silently authorize against the wrong node if one field served both.
	writeScope func(resourceFields) string
	// createSchema/updateSchema name the `api/openapi.yaml` component schemas this
	// family's POST and PATCH bodies are DECLARED as. A named schema is enforced
	// at request time, before any store write (bodyschema.go); an empty name means
	// the family declares no request body at all in the document, which today is
	// true of the three "Shape stub — the row schema is a later minor" kinds
	// (playlists, schedules, dayparts) and of nothing else.
	//
	// These are the declared schema's OWN names rather than a per-kind rule set
	// restated here, so a family cannot be validated against a shape the document
	// does not declare — and a name the document does not define refuses the
	// write rather than skipping the check (bodyschema.go declaredSchema).
	createSchema string
	updateSchema string
	// createMembers/updateMembers name the component schemas whose declared member
	// set bounds a body, for a family that runs the MEMBER half of schema
	// enforcement without the value half. Set these instead of createSchema/
	// updateSchema when a family's per-field validation lives downstream and must
	// keep answering first (bodyschema.go undeclaredMemberRejected).
	createMembers string
	updateMembers string
	// validate, when non-nil, is a per-kind pre-write body validation run over the
	// EFFECTIVE request body — the create body, or a patch shallow-merged onto the
	// current row — BEFORE the store write; a non-empty result is rendered
	// 422 / VALIDATION_FAILED carrying the per-field errors as the api/1 `errors`
	// extension (API-013), and nothing is stored. Only the playlist kind sets it:
	// each item's asset_ref must resolve in the shared content origin (you cannot
	// schedule content that was never uploaded, DAT-041) — see scheduling.go.
	validate func(srv *server, body []byte) []datamodel.Error
	// writeGuards, when non-nil, contributes per-kind store.WriteGuards evaluated
	// over the EFFECTIVE body INSIDE the store's write transaction, beside the
	// external_id-uniqueness guard every kind already carries.
	//
	// It exists for the rules whose input can change between a pre-write check and
	// the write itself. `validate` above is the pre-write form and is the right
	// place for anything that depends only on the body; this is for anything that
	// depends on state another writer can move underneath it. Only the playlist
	// kind sets it: an asset_ref's presence in the content origin is checked here
	// as well as pre-write, because the content retention sweep can reclaim an
	// asset in between — see playlistAssetGuards (scheduling.go).
	writeGuards func(srv *server, body []byte) []store.WriteGuard
	// undeletable, when non-nil, refuses a DELETE of this row outright, from the
	// row's own fields alone — no store read, and ahead of the If-Match
	// precondition. Returning nil accepts.
	//
	// The ordering is the whole reason it is separate from deleteGuards below.
	// This hook is for a refusal no retry can ever clear, and the handler already
	// makes that argument once: an unwritable placement is refused before the
	// precondition because "inviting a viewer to retry with an ETag would be an
	// invitation to an operation they can never complete." A permanently
	// undeletable row is the same claim about the row instead of the caller. Only
	// the scope-node kind sets it — data-model/1 DAT-022's org node (scopenodes.go).
	undeletable func(resourceFields) *deleteRefused
	// deleteGuards, when non-nil, contributes per-kind store.DeleteGuards
	// evaluated INSIDE the store's delete transaction, against the store as it
	// stands under the same write lock the removal commits under.
	//
	// It is writeGuards' counterpart for the one verb with no body: a delete
	// submits nothing to validate, so a per-kind delete rule asks about the REST
	// of the store — what still points at the row about to disappear — and that is
	// state another writer can move between a pre-read and the write, which is
	// exactly why it belongs in the transaction rather than in the handler. Only
	// the scope-node kind sets it: DAT-020's child nodes and DAT-021's placed rows
	// (scopenodes.go).
	deleteGuards func(srv *server, id string) []store.DeleteGuard
}

// deleteRefused is a per-kind delete rule's typed refusal: the HTTP Status, the
// published Code the caller branches on, and the Title/Detail its Problem body
// carries. It is modelled on apihttp.ExternalIDError and used the same way — a
// guard returns it INSIDE the store transaction and writeStoreError renders it on
// the way out — so a refusal raised in the transaction and one raised in the
// handler produce the identical response through the identical path.
type deleteRefused struct {
	Status int
	Code   string
	Title  string
	Detail string
}

// Error lets a *deleteRefused satisfy the error interface; the message is the
// Problem Detail.
func (e *deleteRefused) Error() string { return e.Detail }

// resource binds a resourceConfig to the shared server so the handler methods
// can be registered as http.HandlerFuncs.
type resource struct {
	srv *server
	cfg resourceConfig
}

// mount registers the five CRUD routes for cfg under /api/v1/<path>. Go 1.22+
// method+path patterns dispatch by method, and {id} captures the resource id.
func (srv *server) mount(rt *router, cfg resourceConfig) {
	rs := &resource{srv: srv, cfg: cfg}
	// Registering the family here, in the same call that registers its routes,
	// is what keeps the audit middleware's view of this surface honest: a family
	// added later is audited under its own resource type and files its records
	// at its own placement without anyone remembering to say so twice (audit.go).
	srv.families[cfg.path] = cfg
	base := apiPrefix + "/" + cfg.path
	rt.HandleFunc("GET "+base, rs.list)
	rt.HandleFunc("POST "+base, rs.create)
	rt.HandleFunc("GET "+base+"/{id}", rs.get)
	rt.HandleFunc("PATCH "+base+"/{id}", rs.patch)
	rt.HandleFunc("DELETE "+base+"/{id}", rs.delete)
}

// resourceFields is the api/1 resource baseline the api layer reads out of a
// row's (or a request's) JSON body to drive the conventions. id/preset_id carry
// the two identity forms (a preset-batch keys on preset_id, the sole DAT-005
// exception); parent_id is a scope node's own placement parent; kind is a scope
// node's reserved intrinsic. It is a read-only projection — the store owns
// persistence and the authoritative baseline.
type resourceFields struct {
	ID         string
	PresetID   string
	ExternalID string
	ScopeNode  string
	ParentID   string
	Kind       string
	Labels     map[string]string
}

// parseFields projects a JSON body onto the resource baseline. A body that does
// not parse yields the zero fields (the store's own validation surfaces the real
// error on write).
func parseFields(body []byte) resourceFields {
	var raw struct {
		ID         string            `json:"id"`
		PresetID   string            `json:"preset_id"`
		ExternalID string            `json:"external_id"`
		ScopeNode  string            `json:"scope_node"`
		ParentID   *string           `json:"parent_id"`
		Kind       string            `json:"kind"`
		Labels     map[string]string `json:"labels"`
	}
	_ = json.Unmarshal(body, &raw)
	f := resourceFields{
		ID:         raw.ID,
		PresetID:   raw.PresetID,
		ExternalID: raw.ExternalID,
		ScopeNode:  raw.ScopeNode,
		Kind:       raw.Kind,
		Labels:     raw.Labels,
	}
	if raw.ParentID != nil {
		f.ParentID = *raw.ParentID
	}
	return f
}

// identity returns the id form a row of this kind keys on.
func (cfg resourceConfig) identity(f resourceFields) string {
	if cfg.kind == store.KindPresetBatch {
		return f.PresetID
	}
	return f.ID
}

// identityField returns the JSON key a row of this kind carries its id under.
func (cfg resourceConfig) identityField() string {
	if cfg.kind == store.KindPresetBatch {
		return "preset_id"
	}
	return "id"
}

// ---- create ---------------------------------------------------------------

func (rs *resource) create(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}

	// Idempotency-Key (optional, API-050): a keyed repeat replays or conflicts
	// before any write; an unkeyed request always executes.
	key := r.Header.Get("Idempotency-Key")
	// API-051: an Idempotency-Key is scoped to (authenticated principal, method,
	// path), so the SAME key value presented by a DIFFERENT principal is an
	// unrelated, fresh request and never a replay. The principal comes from the
	// request's resolved identity — the middleware guarantees one is present.
	scope := apihttp.IdempotencyScope{Principal: auth.PrincipalID(r), Method: r.Method, Path: r.URL.Path}
	hash := apihttp.IdempotencyBodyHash(raw)
	now := rs.srv.nowMs()
	if key != "" {
		switch out := rs.srv.idem.Begin(scope, key, hash, now); out.Kind {
		case apihttp.BeginReplay:
			replay(w, out.Response)
			return
		case apihttp.BeginConflict:
			rs.problem(w, r, http.StatusConflict, apihttp.CodeIdempotencyKeyReused, "Conflict", apihttp.IdempotencyReuseDetail(key))
			return
		case apihttp.BeginInProgress:
			rs.problem(w, r, http.StatusConflict, apihttp.CodeIdempotencyKeyInProgress, "Conflict", "A request with this Idempotency-Key is already in progress.")
			return
		}
	}

	// A fresh keyed request now holds an in-flight marker that MUST be resolved on
	// EVERY terminal path (API-052/054) — never only on success. The response is
	// composed into a capture so its exact bytes can be retained for replay, and a
	// definitive outcome (any status < 500, success OR a deterministic client
	// Problem) is Completed; a transient 5xx is Aborted so the key stays retryable.
	rc := &responseCapture{}
	rs.createExec(rc, r, raw)
	status, body, ct := rc.flush(w)

	if key != "" {
		if status < http.StatusInternalServerError {
			rs.srv.idem.Complete(scope, key, hash, apihttp.StoredResponse{
				Status:      status,
				Body:        body,
				ContentType: ct,
			}, now)
		} else {
			rs.srv.idem.Abort(scope, key, hash)
		}
	}
}

// createExec performs the create against the store and writes its outcome — a 201
// with ETag/Location, or an api/1 Problem — to w. A client-supplied id/preset_id
// is rejected upfront (rejectClientSuppliedID); external_id uniqueness
// (API-101/102) is enforced ATOMICALLY inside the store write (via a
// WriteGuard), closing the check-then-write race a pre-write snapshot in a
// separate critical section left open. w is the response capture create() owns,
// so the exact bytes are retainable for an Idempotency-Key replay.
func (rs *resource) createExec(w http.ResponseWriter, r *http.Request, raw []byte) {
	if rs.rejectClientSuppliedID(w, r, raw) {
		return
	}
	// The body the CLIENT sent, against the schema api/openapi.yaml declares for
	// this operation (bodyschema.go). It runs on raw — before ensureID injects the
	// server-minted id, which every Create schema forbids — and after
	// rejectClientSuppliedID, so a client-supplied id keeps API-105's own code
	// rather than collapsing into a generic schema violation.
	if rs.schemaRejected(w, r, rs.cfg.createSchema, raw) {
		return
	}
	if rs.undeclaredMemberRejected(w, r, rs.cfg.createMembers, raw) {
		return
	}
	// The server always mints the id (openapi: id is not part of the create
	// body; a resource's id is server-assigned, api/1 API-100) — a
	// client-supplied one was already rejected above.
	body, id := rs.ensureID(raw)
	fields := parseFields(body)

	// SEC-005 authorizes "before executing", and on a create the placement is
	// the whole of what there is to authorize: the caller named the scope node
	// this row will live under, and a row written under a node the caller has no
	// authority at is invisible to its own author the moment it lands.
	//
	// The check sits AHEAD of every stage that consults stored state — the
	// per-kind body validation, and above all the external_id uniqueness guard
	// inside the store write. That ordering is the point rather than an
	// optimization: the uniqueness guard necessarily scans rows under the target
	// node whether the caller can see them or not (API-101 scopes uniqueness by
	// placement, not by principal), so reaching it with an unauthorized
	// placement is what turns a 400 EXTERNAL_ID_CONFLICT into an existence
	// oracle for a hidden row. Refusing the PLACEMENT first means the guard is
	// never reached by a caller who was not already entitled to enumerate what
	// it scans (scopeview.go).
	view, ok := rs.requestView(w, r)
	if !ok {
		return
	}
	if !view.canWrite(rs.cfg.writeScope(fields)) {
		rs.refuseWrite(w, r)
		return
	}

	if rs.writeValidationFailed(w, r, body) {
		return
	}

	res, err := rs.srv.store.Create(r.Context(), rs.cfg.kind, body,
		rs.writeGuards(fields, "", body)...)
	if err != nil {
		rs.writeStoreError(w, r, err)
		return
	}

	w.Header().Set("ETag", apihttp.ETag(res.Revision))
	w.Header().Set("Location", apiPrefix+"/"+rs.cfg.path+"/"+id)
	writeJSON(w, http.StatusCreated, res.Body)
}

// rejectClientSuppliedID writes a 422 / ID_SERVER_ASSIGNED Problem, naming the
// kind's own identity field (id, or a preset-batch's preset_id) in `detail`,
// when raw carries a non-empty client-supplied identity, and reports whether it
// wrote a response so the caller aborts before any store write. A resource's id
// is exclusively server-assigned (API-105): api/1's Definitions name id as the
// server-minted identity, external_id (API-100–104) is the sanctioned
// client-assigned identity slot, and every Create schema api/openapi.yaml
// declares already omits id (additionalProperties: false) — this closes the gap
// where the HTTP handlers did not yet enforce that schema at runtime.
//
// ID_SERVER_ASSIGNED is the TOP-LEVEL `code`, which is what API-105 fixes for
// this rejection ("MUST be rejected with 422 Unprocessable Content / code:
// ID_SERVER_ASSIGNED"). It is deliberately not a VALIDATION_FAILED carrying
// ID_SERVER_ASSIGNED in an `errors[]` entry: API-013 reserves that shape for a
// response carrying MORE THAN ONE independent field-level failure, `code` is the
// sole machine-readable discriminant a client switches on (API-016), and
// api/openapi.yaml's own Problem schema states `errors` is "Present only when
// `code` is VALIDATION_FAILED". Nesting the code left the one value API-105
// names unreachable from where every client reads it.
//
// Applying the same check to a PATCH body (rs.patch) additionally prevents a
// client from overwriting a resource's already-assigned id after creation, which
// store.Update's merge-over-current-body would otherwise accept with no
// id-immutability check of its own.
func (rs *resource) rejectClientSuppliedID(w http.ResponseWriter, r *http.Request, raw []byte) bool {
	f := parseFields(raw)
	if rs.cfg.identity(f) == "" {
		return false
	}
	field := rs.cfg.identityField()
	apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
		"ID_SERVER_ASSIGNED", "Unprocessable Content",
		"a resource's "+field+" is assigned by the server and MUST NOT be supplied by the client; use external_id for a client-assigned identity (api/1 API-100).",
		nil)
	return true
}

// ensureID returns the create body with a freshly minted identity injected
// under the kind's identity field, plus that id. A client-supplied id is
// rejected upstream (rejectClientSuppliedID) before this ever runs, so every
// body reaching here is minted fresh from the server's injected id source
// (srv.newID — never a package-level generator).
func (rs *resource) ensureID(raw []byte) (body []byte, id string) {
	id = rs.srv.newID()
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &m)
	idJSON, _ := json.Marshal(id)
	m[rs.cfg.identityField()] = idJSON
	out, err := json.Marshal(m)
	if err != nil {
		return raw, id
	}
	return out, id
}

// ---- get ------------------------------------------------------------------

func (rs *resource) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, found, err := rs.srv.store.Get(r.Context(), rs.cfg.kind, id)
	if err != nil {
		rs.internal(w, r, err)
		return
	}
	if !found {
		rs.notFound(w, r)
		return
	}
	if visible, err := rs.readable(r, res.Body); err != nil {
		rs.internal(w, r, err)
		return
	} else if !visible {
		rs.notFound(w, r)
		return
	}
	w.Header().Set("ETag", apihttp.ETag(res.Revision))
	writeJSON(w, http.StatusOK, res.Body)
}

// readable reports whether the request's principal may read a row of this kind
// with the given stored body — i.e. whether the row's scope-node placement is
// in the caller's visible set (scopeview.go). A false answer means the caller
// MUST be answered exactly as if the row did not exist (rs.notFound), never
// with a 403 that would confirm it does.
func (rs *resource) readable(r *http.Request, body []byte) (bool, error) {
	view, err := rs.srv.scopeView(r)
	if err != nil {
		return false, err
	}
	return view.canRead(rs.cfg.placement(parseFields(body))), nil
}

// requestView builds this request's scope view ONCE, writing the 500 Problem
// itself and reporting ok=false when the tree read fails, so a caller has a
// single thing to check.
//
// Every handler needing more than one answer out of it builds it through here
// rather than calling srv.scopeView per question: a patch that tested
// visibility against one read of the tree and placement authority against a
// later one could be told the row is addressable and the destination
// authorized by two different worlds, which is precisely the disagreement
// scopeview.go exists to make impossible.
func (rs *resource) requestView(w http.ResponseWriter, r *http.Request) (scopeView, bool) {
	view, err := rs.srv.scopeView(r)
	if err != nil {
		rs.internal(w, r, err)
		return scopeView{}, false
	}
	return view, true
}

// refuseWrite writes the 403 / FORBIDDEN Problem an api/1 write draws when the
// caller holds no write authority at the scope node the write acts on
// (SEC-005: an authorization that cannot be resolved refuses, never
// default-permits). See scopeview.go for why this refusal is 403 where the read
// side's is 404, and why that distinction discloses nothing.
func (rs *resource) refuseWrite(w http.ResponseWriter, r *http.Request) {
	rs.problem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden", unauthorizedWriteDetail)
}

// unauthorizedWriteDetail is the ONE `detail` string every refusal of an
// unauthorized write carries, on every resource family and every mutating
// operation on this surface.
//
// It names no scope node, no resource kind and no id, and that is the whole of
// its design. A detail echoing the node back — or worded one way for "this node
// is not yours" and another for "this node does not exist" — would re-open in
// prose exactly the existence oracle the ordering of the check closes, and a
// Problem body is compared member for member by anyone looking for one.
const unauthorizedWriteDetail = "This principal holds no write authority at the scope node this request writes at."

// ---- list -----------------------------------------------------------------

func (rs *resource) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	cursor, limit, pperr := apihttp.ParsePageParams(q.Get("cursor"), q.Get("limit"))
	if pperr != nil {
		rs.problem(w, r, pperr.Status, pperr.Code, pperr.Title, pperr.Detail)
		return
	}

	sel, serr := apiselector.Parse(q.Get("selector"))
	if serr != nil {
		rs.problem(w, r, serr.Status, serr.Code, serr.Title, serr.Detail)
		return
	}

	// The cursor is scoped to THIS resource type (rs.cfg.resourceType), so a
	// cursor minted by another resource's list — even a byte-identical id — is
	// rejected 400 / CURSOR_INVALID here rather than silently paged from as an
	// arbitrary keyset position in the wrong collection (API-033/035).
	var afterID string
	if cursor != "" {
		lastID, cerr := apihttp.DecodeCursor(rs.cfg.resourceType, cursor)
		if cerr != nil {
			rs.problem(w, r, cerr.Status, cerr.Code, cerr.Title, cerr.Detail)
			return
		}
		afterID = lastID
	}

	rows, err := rs.srv.store.List(r.Context(), rs.cfg.kind, store.ListFilter{})
	if err != nil {
		rs.internal(w, r, err)
		return
	}

	view, err := rs.srv.scopeView(r)
	if err != nil {
		rs.internal(w, r, err)
		return
	}

	// Keyset advance past the cursor position, then the visible-set filter, then
	// the selector. ULIDs sort lexicographically in id order, so a byte
	// comparison is the keyset order.
	//
	// BOTH filters run here, BEFORE apihttp.Page cuts the page — never after.
	// That ordering is what keeps a page of `limit` rows actually `limit` rows:
	// filtering a page after the keyset window was chosen would silently return
	// fewer than asked for whenever an out-of-reach or unselected row happened
	// to fall inside it, and would leak the count of the rows removed. Filtering
	// first means the window Page cuts from already contains only rows this
	// caller may see, so a short page means the collection is genuinely
	// exhausted (API-032/034).
	//
	// The visible-set test precedes the selector, which is what makes the
	// selector a pure NARROWING of the visible set (events/1 EVT-121's rule for
	// the same grammar): the two are ANDed, and an intersection can never widen.
	// A selector naming an out-of-reach node simply matches no visible row and
	// yields an empty page rather than an error (EVT-122).
	window := make([]json.RawMessage, 0, len(rows))
	for _, res := range rows {
		if afterID != "" && res.ID <= afterID {
			continue
		}
		f := parseFields(res.Body)
		if !view.canRead(rs.cfg.placement(f)) {
			continue
		}
		if !sel.Matches(rs.cfg.selLabels(f), rs.cfg.placement(f), view.inSubtree) {
			continue
		}
		window = append(window, res.Body)
	}

	// The next cursor is bound to this resource type (rs.cfg.resourceType) so it
	// names a keyset position ONLY for this resource's list and is refused under
	// any other (API-033); the encoded token stays opaque to the client.
	page := apihttp.Page(rs.cfg.resourceType, window, limit, func(b json.RawMessage) string {
		return parseFields(b).ID
	})
	writeJSONValue(w, http.StatusOK, page)
}

// ---- patch ----------------------------------------------------------------

func (rs *resource) patch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	current, found, err := rs.srv.store.Get(r.Context(), rs.cfg.kind, id)
	if err != nil {
		rs.internal(w, r, err)
		return
	}
	if !found {
		rs.notFound(w, r)
		return
	}
	// One view answers both halves of this request's authorization, from one
	// read of the tree: whether the row is addressable at all, and whether it
	// may be written where it is and where it is going.
	view, ok := rs.requestView(w, r)
	if !ok {
		return
	}
	// A row outside the caller's visible set is unaddressable, not merely
	// unwritable: the check sits AHEAD of the If-Match precondition so an
	// out-of-reach id draws the same 404 whatever ETag was (or was not)
	// presented. A 428 IF_MATCH_REQUIRED here would confirm the row exists just
	// as surely as a 403 would (scopeview.go).
	curFields := parseFields(current.Body)
	if !view.canRead(rs.cfg.placement(curFields)) {
		rs.notFound(w, r)
		return
	}
	// Addressable, but not necessarily writable: `viewer` clears the visible-set
	// floor and nothing above it (auth.CanWrite). This refusal is 403 rather
	// than 404 because the caller can demonstrably already READ this row — it
	// discloses nothing beyond the caller's own authority — and it too precedes
	// If-Match, since inviting a viewer to retry with an ETag would be an
	// invitation to an operation they can never complete.
	if !view.canWrite(rs.cfg.writeScope(curFields)) {
		rs.refuseWrite(w, r)
		return
	}

	ifMatch, present := r.Header["If-Match"]
	outcome := apihttp.CheckIfMatch(headerValue(ifMatch), present, current.Revision)
	if !outcome.OK {
		rs.concurrencyProblem(w, r, outcome)
		return
	}

	patchBody, ok := readBody(w, r)
	if !ok {
		return
	}
	if rs.rejectClientSuppliedID(w, r, patchBody) {
		return
	}
	// The PATCH body the client sent, against the Update schema api/openapi.yaml
	// declares for this family (bodyschema.go). It runs on the patch body rather
	// than the merge: an Update schema describes what a client may SEND (every
	// member optional, at least one present), not what a complete stored row looks
	// like, so validating the merge would answer a different question.
	if rs.schemaRejected(w, r, rs.cfg.updateSchema, patchBody) {
		return
	}
	if rs.undeclaredMemberRejected(w, r, rs.cfg.updateMembers, patchBody) {
		return
	}

	merged := mergedBody(current.Body, patchBody)

	// A patch may MOVE the row. Authority over where it currently sits says
	// nothing about where it is going, so the destination is authorized on its
	// own — before the per-kind validation and before the external_id guard, for
	// the same reason the create path orders it there: the guard evaluates
	// uniqueness under the DESTINATION node (parseFields(merged) feeds
	// externalIDGuards below), so an unauthorized destination must be refused
	// before it can be used to probe one.
	//
	// A destination equal to the row's current one is not re-checked: it is not
	// a placement this request is deciding, and authority over it was just
	// established above. Everything else is checked, including the case the
	// visible-set filter cannot catch — moving a row OUT of the caller's reach,
	// where the row would vanish from its own author's view on success.
	eff := parseFields(merged)
	if dest := rs.cfg.writeScope(eff); dest != rs.cfg.writeScope(curFields) {
		if !view.canWrite(dest) {
			rs.refuseWrite(w, r)
			return
		}
	}

	// A per-kind pre-write validation (playlist asset_refs) runs over the EFFECTIVE
	// post-merge body, so a patch that introduces an un-uploaded asset_ref is
	// rejected exactly as a create would be — never stored.
	if rs.writeValidationFailed(w, r, merged) {
		return
	}

	// external_id uniqueness over the effective (post-merge) fields (API-101/102),
	// enforced atomically inside the store write by a WriteGuard — closing the
	// check-then-write race a pre-write snapshot in a separate critical section left
	// open. selfID excludes this row, so an unchanged external_id never self-collides.
	// The per-kind guards ride the same transaction, over the same effective body.
	res, err := rs.srv.store.Update(r.Context(), rs.cfg.kind, id, current.Revision, patchBody,
		rs.writeGuards(eff, id, merged)...)
	if err != nil {
		rs.writeStoreError(w, r, err)
		return
	}
	w.Header().Set("ETag", apihttp.ETag(res.Revision))
	writeJSON(w, http.StatusOK, res.Body)
}

// ---- delete ---------------------------------------------------------------

func (rs *resource) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	current, found, err := rs.srv.store.Get(r.Context(), rs.cfg.kind, id)
	if err != nil {
		rs.internal(w, r, err)
		return
	}
	if !found {
		rs.notFound(w, r)
		return
	}
	// Same ordering as patch: unaddressable before unwritable before the
	// precondition, so an out-of-reach id is a 404 regardless of the presented
	// ETag and a reader who may not mutate is refused before being invited to
	// supply one. A delete moves the row nowhere, so the row's current placement
	// is the whole of what there is to authorize.
	view, ok := rs.requestView(w, r)
	if !ok {
		return
	}
	curFields := parseFields(current.Body)
	if !view.canRead(rs.cfg.placement(curFields)) {
		rs.notFound(w, r)
		return
	}
	if !view.canWrite(rs.cfg.writeScope(curFields)) {
		rs.refuseWrite(w, r)
		return
	}

	// A per-kind refusal no retry can clear (cfg.undeletable) sits here, between
	// the authorization checks and the precondition. Behind the authorization, so
	// it can never tell a caller who could not otherwise see this row that it
	// exists; ahead of the precondition, for the reason the unwritable-placement
	// refusal above is: a 428 or a 412 asks the caller to come back with a fresh
	// revision, and there is no revision at which this delete would be allowed.
	if rs.cfg.undeletable != nil {
		if refusal := rs.cfg.undeletable(curFields); refusal != nil {
			rs.problem(w, r, refusal.Status, refusal.Code, refusal.Title, refusal.Detail)
			return
		}
	}

	ifMatch, present := r.Header["If-Match"]
	outcome := apihttp.CheckIfMatch(headerValue(ifMatch), present, current.Revision)
	if !outcome.OK {
		rs.concurrencyProblem(w, r, outcome)
		return
	}

	// The per-kind rules a caller CAN clear (cfg.deleteGuards) run last, inside
	// the store transaction — after the precondition, because clearing them is a
	// write and a caller who is going to make one has to hold a current revision
	// like any other writer.
	if err := rs.srv.store.Delete(r.Context(), rs.cfg.kind, id, current.Revision, rs.deleteGuards(id)...); err != nil {
		rs.writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteGuards is the guard set one delete carries into the store transaction:
// whatever the kind's own resourceConfig.deleteGuards contributes, and nothing
// else. Unlike writeGuards there is no shared baseline guard every kind carries —
// external_id uniqueness is a rule about a value being written, and a delete
// writes none — so a kind that names no hook carries no guard and the store's
// delete path is unchanged for it.
func (rs *resource) deleteGuards(id string) []store.DeleteGuard {
	if rs.cfg.deleteGuards == nil {
		return nil
	}
	return rs.cfg.deleteGuards(rs.srv, id)
}

// ---- shared helpers -------------------------------------------------------

// externalIDGuards returns the store WriteGuard(s) that enforce external_id
// uniqueness (API-101/102) INSIDE the write transaction — atomically with the
// write, closing the check-then-write race a pre-write snapshot leaves open. The
// guard reuses apihttp.CheckExternalIDUnique over the tx snapshot the store hands
// it (it never re-derives the rule) and returns the *apihttp.ExternalIDError
// verbatim for writeStoreError to render. selfID excuses the row being updated (so
// an unchanged external_id never self-collides); an empty externalID needs no
// guard, since it can never collide (API-100).
// writeGuards is the full guard set one write carries into the store
// transaction: the external_id-uniqueness guard every kind shares, plus whatever
// the kind's own resourceConfig.writeGuards contributes over the effective body.
//
// One assembly point, used by both create and patch, so a kind cannot end up with
// its guards enforced on one verb and not the other — which is how a rule ends up
// looking enforced while a PATCH walks straight past it.
func (rs *resource) writeGuards(fields resourceFields, selfID string, body []byte) []store.WriteGuard {
	guards := rs.externalIDGuards(fields.ExternalID, rs.cfg.extScope(fields), selfID)
	if rs.cfg.writeGuards != nil {
		guards = append(guards, rs.cfg.writeGuards(rs.srv, body)...)
	}
	return guards
}

func (rs *resource) externalIDGuards(externalID, scopeNode, selfID string) []store.WriteGuard {
	if externalID == "" {
		return nil
	}
	return []store.WriteGuard{func(existing []store.Resource) error {
		refs := rs.refsFrom(existing)
		if xerr := apihttp.CheckExternalIDUnique(refs, rs.cfg.resourceType, rs.cfg.displayName, scopeNode, externalID, selfID); xerr != nil {
			return xerr
		}
		return nil
	}}
}

// refsFrom projects the store rows a WriteGuard is handed onto the ExternalRef
// shape CheckExternalIDUnique consumes. Every row is tagged with the kind's
// resourceType (the family api/1 scopes external_id uniqueness by — API-101
// scopes by resource type together with placement, not by the row's own
// store.Kind), and placed under the kind's own grouping (a scope node by
// parent, a scheduling row by scope_node).
func (rs *resource) refsFrom(rows []store.Resource) []apihttp.ExternalRef {
	refs := make([]apihttp.ExternalRef, 0, len(rows))
	for _, res := range rows {
		f := parseFields(res.Body)
		refs = append(refs, apihttp.ExternalRef{
			ID:           res.ID,
			ExternalID:   res.ExternalID,
			ResourceType: rs.cfg.resourceType,
			ScopeNode:    rs.cfg.extScope(f),
		})
	}
	return refs
}

// mergedBody shallow-merges a patch over the current body — the EFFECTIVE
// post-patch body a per-kind validation and the external_id check both evaluate
// against. A body that cannot be re-marshaled degrades to the current body (the
// store's own validation surfaces the real error on write).
func mergedBody(current, patch []byte) []byte {
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(current, &m)
	p := map[string]json.RawMessage{}
	_ = json.Unmarshal(patch, &p)
	for k, v := range p {
		m[k] = v
	}
	merged, err := json.Marshal(m)
	if err != nil {
		return current
	}
	return merged
}

// writeValidationFailed runs the resource kind's per-kind pre-write validation (if
// any) over body and, on a non-empty result, writes the 422 / VALIDATION_FAILED
// Problem carrying the per-field errors as the api/1 `errors` extension (API-013),
// returning true so the caller aborts before any store write. It returns false
// (writing nothing) when the kind declares no validation or the body passes.
func (rs *resource) writeValidationFailed(w http.ResponseWriter, r *http.Request, body []byte) bool {
	if rs.cfg.validate == nil {
		return false
	}
	verrs := rs.cfg.validate(rs.srv, body)
	if len(verrs) == 0 {
		return false
	}
	apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
		"VALIDATION_FAILED", "Validation Failed",
		"One or more fields failed validation.", validationExtra(verrs))
	return true
}

// writeStoreError maps a store write error onto its api/1 Problem: a datamodel
// validation failure is 422 / VALIDATION_FAILED carrying the per-field errors
// (API-013); an optimistic-concurrency conflict is 412 / REVISION_CONFLICT with
// current_revision; a not-found is 404; anything else is 500 / INTERNAL.
func (rs *resource) writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	// An external_id-uniqueness rejection a WriteGuard raised inside the store write
	// (API-101/102): 400 / EXTERNAL_ID_CONFLICT, surfaced atomically with the write
	// it prevented.
	var xerr *apihttp.ExternalIDError
	if errors.As(err, &xerr) {
		rs.problem(w, r, xerr.Status, xerr.Code, xerr.Title, xerr.Detail)
		return
	}
	// A per-kind delete rule a DeleteGuard raised inside the store's delete
	// transaction (data-model/1 DAT-020/021): rendered through the same helper
	// the handler's own pre-precondition refusal uses, so the two spell one
	// refusal one way.
	var dref *deleteRefused
	if errors.As(err, &dref) {
		rs.problem(w, r, dref.Status, dref.Code, dref.Title, dref.Detail)
		return
	}
	// A rules/1 compile failure the store's compile-gate raised on an automation
	// write (compile.Compile, never re-run here): the authored rule is rejected
	// 422 / VALIDATION_FAILED carrying the compiler's message as the Problem detail
	// and its offending member as the api/1 `errors` extension (API-013) — a
	// non-compiling rule is never stored nor carried to the relay.
	var cerr *compile.CompileError
	if errors.As(err, &cerr) {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"VALIDATION_FAILED", "Validation Failed", cerr.Message, compileErrorExtra(cerr))
		return
	}
	var verr *store.ValidationError
	if errors.As(err, &verr) {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"VALIDATION_FAILED", "Validation Failed",
			"One or more fields failed validation.", validationExtra(verr.Errors))
		return
	}
	var rme *store.RevisionMismatchError
	if errors.As(err, &rme) {
		cur := rme.Current
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusPreconditionFailed,
			"REVISION_CONFLICT", "Precondition Failed",
			"The resource was modified concurrently.", map[string]any{"current_revision": cur})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		rs.notFound(w, r)
		return
	}
	rs.internal(w, r, err)
}

// validationExtra renders a datamodel error list as the api/1 `errors` extension
// member (API-013): one {field, code, message} object per failure.
func validationExtra(errs []datamodel.Error) map[string]any {
	arr := make([]map[string]string, 0, len(errs))
	for _, e := range errs {
		arr = append(arr, map[string]string{"field": e.Field, "code": e.Code, "message": e.Message})
	}
	return map[string]any{"errors": arr}
}

// concurrencyProblem writes the Problem a failed If-Match precondition yields,
// carrying current_revision for a REVISION_CONFLICT (API-023).
func (rs *resource) concurrencyProblem(w http.ResponseWriter, r *http.Request, o apihttp.ConcurrencyOutcome) {
	var extra map[string]any
	if o.CurrentRevision != nil {
		extra = map[string]any{"current_revision": *o.CurrentRevision}
	}
	apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), o.Status, o.Code, o.Title, o.Detail, extra)
}

func (rs *resource) problem(w http.ResponseWriter, r *http.Request, status int, code, title, detail string) {
	apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), status, code, title, detail, nil)
}

func (rs *resource) notFound(w http.ResponseWriter, r *http.Request) {
	rs.problem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No "+rs.cfg.displayName+" exists with this identifier.")
}

// internal is the fallback for an unexpected server-side failure (API error
// taxonomy INTERNAL); the detail is deliberately generic.
func (rs *resource) internal(w http.ResponseWriter, r *http.Request, _ error) {
	rs.problem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
}

// maxJSONBodyBytes caps a JSON request body at 1 MiB.
//
// Every handler here buffers the whole body before it decides anything — it has
// to, because an Idempotency-Key replay is keyed on those exact bytes (API-051)
// — and the read happens BEFORE the handler's own authorization check, so the
// allocation is driven by a principal whose authority for the operation has not
// been established yet. Authentication has (the mux sits behind
// auth.Middleware), so this is not an anonymous lever; it is still one
// authenticated principal deciding how much memory the process spends on a
// request it may not be allowed to make.
//
// A megabyte is far beyond any resource this surface accepts — the largest is a
// playlist with its items — and far below anything worth worrying about.
const maxJSONBodyBytes = 1 << 20

// maxContentUploadBytes caps a content-origin upload at 64 MiB.
//
// It is a separate, much larger limit because the content route carries asset
// BYTES rather than a resource description. It is not generous in the way a
// streaming ingest would be: origin.Store.Add takes a byte slice, so an upload
// is buffered in full whatever this number says, and the honest reading is that
// large-media ingest needs a streaming path rather than a bigger constant. Until
// then this bounds the buffer instead of leaving it to the client.
const maxContentUploadBytes = 64 << 20

// readBody reads the full request body, writing a 400 Problem and returning
// ok=false on a read failure — including a body that exceeds maxJSONBodyBytes.
func readBody(w http.ResponseWriter, r *http.Request) (body []byte, ok bool) {
	return readBodyLimit(w, r, maxJSONBodyBytes)
}

// readBodyLimit is readBody with an explicit ceiling, for the one route whose
// payload is not a JSON resource.
//
// http.MaxBytesReader, rather than io.LimitReader, is what makes the limit an
// error instead of a silent truncation: a truncated body would reach
// json.Unmarshal as malformed JSON and be reported as a validation failure the
// client cannot act on, while an oversize body reported as oversize tells them
// exactly what to change.
func readBodyLimit(w http.ResponseWriter, r *http.Request, max int64) (body []byte, ok bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		detail := "The request body could not be read."
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			detail = fmt.Sprintf("The request body exceeds the %d-byte limit for this operation.", max)
		}
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusBadRequest,
			"VALIDATION_FAILED", "Bad Request", detail, nil)
		return nil, false
	}
	return b, true
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeJSONValue(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// responseCapture buffers a handler's composed response (status, handler-set
// headers, body) so the exact bytes can be BOTH flushed to the real ResponseWriter
// AND retained verbatim for an Idempotency-Key replay (API-052). It captures only
// what the handler writes; the Trace-Id response header WithTraceID already set on
// the real writer is untouched and still emitted on flush.
type responseCapture struct {
	hdr    http.Header
	status int
	body   bytes.Buffer
}

func (c *responseCapture) Header() http.Header {
	if c.hdr == nil {
		c.hdr = http.Header{}
	}
	return c.hdr
}

func (c *responseCapture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *responseCapture) Write(b []byte) (int, error) { return c.body.Write(b) }

// flush copies the captured headers and body onto w, then returns the status, a
// COPY of the body bytes, and the Content-Type — the fields an Idempotency-Key
// StoredResponse retains (the copy so the retained record is immune to later buffer
// reuse). A handler that never wrote a status is treated as 200.
func (c *responseCapture) flush(w http.ResponseWriter) (status int, body []byte, contentType string) {
	for k, vs := range c.hdr {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	status = c.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	b := c.body.Bytes()
	_, _ = w.Write(b)
	return status, append([]byte(nil), b...), c.hdr.Get("Content-Type")
}

// replay writes a retained idempotent response verbatim (status + content-type +
// body). Per the StoredResponse shape, per-response headers such as ETag/Location
// are not part of the retained record and are not reproduced on replay.
func replay(w http.ResponseWriter, resp apihttp.StoredResponse) {
	ct := resp.ContentType
	if ct == "" {
		ct = contentTypeJSON
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// headerValue returns the first value of a possibly-multi-valued header slice.
func headerValue(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}
