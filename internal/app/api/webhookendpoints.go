package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// webhookendpoints.go is the management-plane registration surface for events/1
// outbound webhook delivery. EVT-150 defines the delivery mechanics and says
// outright that "a webhook endpoint's own registration (URL, subscribed schemas,
// scope-node selector) is a management-plane resource this contract does not
// define" — this is that resource, defined here under api/1's own conventions.
//
// # The registration itself is an ordinary resource
//
// It is a plain resourceConfig mount, so it inherits keyset pagination, the
// label selector, external_id uniqueness, Idempotency-Key on create, ETag and
// If-Match, visible-set read scoping, placement authorization on writes, and the
// audit record — none of it re-implemented here. That is the point of adding a
// family this way rather than hand-mounting one: a convention this surface
// enforces everywhere else cannot be quietly skipped by a new family.
//
// # The signing secret is not part of it
//
// The generic handlers serve a row's stored body back verbatim, so a secret in
// that body is a secret handed to every reader of the row. The signing secret is
// therefore never a member of the endpoint's representation — not on create, not
// on read, not on update — and lives sealed in a private table instead
// (internal/app/store/webhooks.go).
//
// It is OPERATOR-SUPPLIED rather than server-minted-and-returned-once. Both
// shapes are defensible and the platform already runs the other one (TOTP
// enrollment returns a freshly minted secret exactly once). This surface takes
// the operator's, for a reason specific to what a webhook secret IS: the same
// value must end up configured on a receiving server the platform does not own,
// so an operator who is going to paste it into that receiver anyway can simply
// paste it into both. That keeps the secret out of every response body on this
// surface, which in turn keeps it out of the response-replay an
// Idempotency-Key retains — a retained response is a copy of the secret with a
// lifetime nobody set out to give it.
//
// # What is NOT here
//
// Nothing in this file signs, retries, or delivers. Registration and delivery
// are separate concerns and separate packages (internal/app/webhookdeliver
// drives delivery), which is why an endpoint registered here starts receiving
// events without this file knowing that happened.

// WebhookSecretSealer seals and opens a registered endpoint's signing secret —
// satisfied by internal/app/webhookdeliver.Secrets, over the per-workspace data
// key's secret-seal sub-key (SEC-040).
//
// It is an interface here so the api layer depends on the capability rather than
// on the delivery package, which keeps the registration surface importable by a
// deployment that registers endpoints without running the delivery loop.
type WebhookSecretSealer interface {
	Seal(endpointID string, secret []byte) (string, error)
	Reseal(endpointID, sealedCurrent string) (string, error)
}

// WithWebhookSecrets wires the sealer a webhook signing secret is stored under,
// and the rotation overlap window a rotation's response publishes.
//
// It is optional, and its absence is answered truthfully rather than by the
// routes disappearing: the registration family still mounts and still works, and
// the rotate operation answers UNAVAILABLE. Storing the secret unsealed instead
// is the one thing it never degrades to.
//
// rotationOverlapMs should be the SAME figure the delivery loop is wired with —
// it is what the rotation response names as the moment the superseded secret
// stops being accepted. Zero takes the contract's own proposed window.
func WithWebhookSecrets(sealer WebhookSecretSealer, rotationOverlapMs int64) Option {
	return func(srv *server) {
		srv.webhookSecrets = sealer
		srv.webhookRotationOverlap = rotationOverlapMs
	}
}

// webhookEndpointsConfig is the registration family's resource configuration.
// Like a scheduling-core row, a webhook endpoint's OWN scope_node is its
// placement (what a selector's scope_node terms evaluate against), its
// external_id uniqueness grouping (API-101), and the node a write of it is
// authorized at (SEC-010) — all three coincide because an endpoint is placed
// directly at the node whose events it subscribes to.
func webhookEndpointsConfig() resourceConfig {
	return resourceConfig{
		kind:         store.KindWebhookEndpoint,
		path:         "webhook-endpoints",
		resourceType: "webhook-endpoints",
		displayName:  "webhook endpoint",
		createSchema: "WebhookEndpointCreate",
		updateSchema: "WebhookEndpointUpdate",
		selLabels:    func(f resourceFields) map[string]string { return f.Labels },
		placement:    func(f resourceFields) string { return f.ScopeNode },
		extScope:     func(f resourceFields) string { return f.ScopeNode },
		writeScope:   func(f resourceFields) string { return f.ScopeNode },
		validate:     validateWebhookEndpoint,
	}
}

// minSigningSecretLen is the shortest signing secret this surface accepts.
//
// EVT-151 keys an HMAC-SHA256 with it and states no minimum, so this is a
// deployment-side floor rather than a contract figure. 32 characters is chosen
// because a secret shorter than the digest it keys is the length an offline
// search reaches first, and because the value has to be typed into a receiver
// exactly once — the cost of the floor is paid at registration, not per
// delivery.
const minSigningSecretLen = 32

// validateWebhookEndpoint is the family's pre-write guard: the URL must be one
// this platform could actually POST to, and must not carry credentials.
//
// The userinfo and query-string checks are not stylistic. A URL is stored in the
// row's body, served back on every read, and named in operator-facing error
// text; anything a caller smuggles into it is disclosed everywhere the URL is.
// A secret placed in a query string is exactly the leak the house rule names, so
// a URL that carries one is refused at the door rather than being stored and
// then carefully not logged.
func validateWebhookEndpoint(_ *server, body []byte) []datamodel.Error {
	var ep struct {
		URL     string   `json:"url"`
		Schemas []string `json:"schemas"`
	}
	if err := json.Unmarshal(body, &ep); err != nil {
		return nil // a malformed body surfaces its real error on the store write.
	}

	var errs []datamodel.Error
	add := func(field, code, msg string) {
		errs = append(errs, datamodel.Error{Field: field, Code: code, Message: msg})
	}

	switch u, err := url.Parse(ep.URL); {
	case ep.URL == "" || err != nil:
		add("url", "VALUE_INVALID", "A webhook endpoint needs an absolute http or https URL to deliver to.")
	case u.Scheme != "http" && u.Scheme != "https":
		add("url", "VALUE_INVALID", "A webhook endpoint URL must use the http or https scheme; a delivery is an HTTP POST (EVT-151).")
	case u.Host == "":
		add("url", "VALUE_INVALID", "A webhook endpoint URL must name a host.")
	case u.User != nil:
		add("url", "VALUE_INVALID", "A webhook endpoint URL must not carry credentials in its userinfo component; the endpoint's signing secret is how a delivery is authenticated.")
	case u.RawQuery != "":
		// Deliberately the whole query string, not a guess at which parameter
		// looks secret-shaped: a rule that tried to recognize secrets would be
		// wrong about the first one it had not seen.
		add("url", "VALUE_INVALID", "A webhook endpoint URL must not carry a query string; a credential placed there would be recorded wherever the URL is.")
	}

	for i, s := range ep.Schemas {
		if strings.TrimSpace(s) == "" {
			add(schemaField(i), "VALUE_INVALID", "A schemas entry must be a non-empty registered or pack-namespaced schema name.")
		}
	}
	return errs
}

func schemaField(i int) string {
	return "schemas[" + strconv.Itoa(i) + "]"
}

// --- the three operations beyond CRUD --------------------------------------

// mountWebhookEndpoints registers the registration family plus the three
// operations a registered endpoint needs that plain CRUD does not express:
// installing or rotating its signing secret, re-enabling it after an auto-
// disable, and reading its delivery state.
//
// None of the three is a resourceConfig operation and none could be. A signing
// secret has no representation to GET, an endpoint's delivery state carries no
// revision to condition a write on, and re-enabling is an act rather than an
// edit — modelling any of them as a member of the endpoint's own body would put
// platform-owned execution state under If-Match and let a stale PATCH revert a
// disable the platform had just decided on.
func (srv *server) mountWebhookEndpoints(rt *router) {
	srv.mount(rt, webhookEndpointsConfig())
	base := apiPrefix + "/webhook-endpoints/{id}"
	rt.HandleFunc("POST "+base+"/signing-secret", srv.rotateWebhookSecret)
	rt.HandleFunc("POST "+base+"/enable", srv.enableWebhookEndpoint)
	rt.HandleFunc("GET "+base+"/delivery", srv.getWebhookDelivery)
}

// webhookEndpointFor resolves the endpoint named in the path and applies this
// surface's read/write posture: an endpoint outside the caller's visible set is
// 404 (byte-identical to one that does not exist, so the response is not an
// existence oracle), and one the caller may read but not write at is 403.
//
// write says which of the two checks applies. It is a parameter rather than two
// near-identical helpers so the read-then-write ordering — never disclose
// existence before authority, never refuse authority before existence — is
// written once.
func (srv *server) webhookEndpointFor(w http.ResponseWriter, r *http.Request, write bool) (store.Resource, bool) {
	id := r.PathValue("id")
	res, ok, err := srv.store.Get(r.Context(), store.KindWebhookEndpoint, id)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return store.Resource{}, false
	}
	if !ok {
		srv.webhookNotFound(w, r)
		return store.Resource{}, false
	}
	view, verr := srv.scopeView(r)
	if verr != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return store.Resource{}, false
	}
	node := parseFields(res.Body).ScopeNode
	if !view.canRead(node) {
		srv.webhookNotFound(w, r)
		return store.Resource{}, false
	}
	if write && !view.canWrite(node) {
		writeProblem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden", unauthorizedWriteDetail)
		return store.Resource{}, false
	}
	return res, true
}

func (srv *server) webhookNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No webhook endpoint exists with this identifier.")
}

// rotateWebhookSecret installs a signing secret for an endpoint (EVT-158).
//
// The first call arms the endpoint: until it lands there is no secret, and an
// endpoint with no secret is never delivered to, because an unsigned POST is not
// a delivery this contract defines. Every later call is a rotation — the secret
// being replaced stays acceptable for the overlap window, so a receiver has time
// to adopt the new one before the old stops working.
//
// The response names when the overlap ends and never echoes a secret. There is
// nothing to echo: the caller supplied the value and already has it, and a copy
// in a response body is a copy in every proxy log and every retained
// idempotency replay between here and them.
func (srv *server) rotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	srv.idempotent(w, r, raw, func(w http.ResponseWriter) {
		res, ok := srv.webhookEndpointFor(w, r, true)
		if !ok {
			return
		}
		if srv.webhookSecrets == nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
				"This deployment has no workspace secret sealer, so a webhook signing secret cannot be stored.")
			return
		}

		var body struct {
			Secret string `json:"secret"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Unprocessable Content",
				"The request body must be a JSON object carrying a `secret`.")
			return
		}
		if len(body.Secret) < minSigningSecretLen {
			// The supplied value is NOT echoed back in the refusal, however
			// short it is — a rejected secret is still a secret.
			apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
				"VALIDATION_FAILED", "Validation Failed", "One or more fields failed validation.",
				validationExtra([]datamodel.Error{{
					Field:   "secret",
					Code:    "VALUE_INVALID",
					Message: "A webhook signing secret must be at least 32 characters.",
				}}))
			return
		}

		ctx := r.Context()
		sealed, err := srv.webhookSecrets.Seal(res.ID, []byte(body.Secret))
		if err != nil {
			srv.webhookSecretError(w, r, err)
			return
		}

		// The secret being superseded is re-sealed under the prior-slot context
		// here rather than demoted by the store, which holds no key. An endpoint
		// with no state row yet is the FIRST install: there is nothing to
		// supersede, so no overlap opens.
		var sealedPrior string
		current, err := srv.store.WebhookDeliveryStateFor(ctx, res.ID)
		switch {
		case err == nil && current.SealedSecret != "":
			if sealedPrior, err = srv.webhookSecrets.Reseal(res.ID, current.SealedSecret); err != nil {
				srv.webhookSecretError(w, r, err)
				return
			}
		case err != nil && !errors.Is(err, store.ErrWebhookStateNotFound):
			writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
			return
		}

		now := srv.nowMs()
		if err := srv.store.RotateWebhookSecret(ctx, res.ID, sealed, sealedPrior, now); err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
			return
		}

		out := map[string]any{"rotated_at_ms": now, "prior_secret_expires_at_ms": nil}
		if sealedPrior != "" {
			out["prior_secret_expires_at_ms"] = now + srv.webhookRotationOverlapMs()
		}
		writeJSONValue(w, http.StatusOK, out)
	})
}

// webhookSecretError renders a sealing failure without ever echoing the value
// that failed to seal or open — it is credential material, and an error string
// is one of the places a house rule says it must never appear.
func (srv *server) webhookSecretError(w http.ResponseWriter, r *http.Request, _ error) {
	writeProblem(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
		"This endpoint's signing secret could not be sealed under the workspace's key.")
}

// webhookRotationOverlapMs is the overlap window this deployment publishes on a
// rotation. It is read from the same configuration the delivery loop is wired
// with when one was supplied, so the instant this response names is the instant
// the sender actually stops emitting the prior signature — a published window
// the sender did not honour would be worse than publishing none.
func (srv *server) webhookRotationOverlapMs() int64 {
	if srv.webhookRotationOverlap > 0 {
		return srv.webhookRotationOverlap
	}
	return events.DefaultRotationOverlapMs
}

// enableWebhookEndpoint is EVT-154's explicit operator re-enable: a disabled
// endpoint receives no further delivery attempts until this is called.
//
// It clears the failure run and leaves last_delivered_id exactly where it was,
// so a re-enabled endpoint resumes from where it stopped rather than skipping
// what it never received (EVT-155). Re-enabling an endpoint that is already
// active is not an error — it is the same end state, and a caller reconciling
// should not have to ask first.
func (srv *server) enableWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	raw, _ := readBody(w, r)
	srv.idempotent(w, r, raw, func(w http.ResponseWriter) {
		res, ok := srv.webhookEndpointFor(w, r, true)
		if !ok {
			return
		}
		st, err := srv.store.WebhookDeliveryStateFor(r.Context(), res.ID)
		if errors.Is(err, store.ErrWebhookStateNotFound) {
			// Never armed, so never disabled: report the state it is actually in
			// rather than inventing a row to satisfy the verb.
			writeJSONValue(w, http.StatusOK, webhookDeliveryBody(store.WebhookDeliveryState{
				EndpointID: res.ID, Status: events.EndpointActive,
			}))
			return
		}
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
			return
		}

		st.Status = events.EndpointActive
		st.ConsecutiveFailures = 0
		st.Attempt = 0
		st.DeliveryID = ""
		st.NextAttemptAtMs = 0
		if err := srv.store.PutWebhookDeliveryProgress(r.Context(), st); err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
			return
		}
		writeJSONValue(w, http.StatusOK, webhookDeliveryBody(st))
	})
}

// getWebhookDelivery reports an endpoint's delivery state: whether it is
// active or auto-disabled (EVT-154), how far delivery has got (EVT-155), and
// how long its current failure run is.
//
// This is the operator-facing view of the signal EVT-154 requires be raised.
// The disable itself is durable whether or not anyone is listening for the
// event, so an operator arriving after the fact can still see what happened,
// which a fire-and-forget notification alone would not give them.
func (srv *server) getWebhookDelivery(w http.ResponseWriter, r *http.Request) {
	res, ok := srv.webhookEndpointFor(w, r, false)
	if !ok {
		return
	}
	st, err := srv.store.WebhookDeliveryStateFor(r.Context(), res.ID)
	if errors.Is(err, store.ErrWebhookStateNotFound) {
		writeJSONValue(w, http.StatusOK, webhookDeliveryBody(store.WebhookDeliveryState{
			EndpointID: res.ID, Status: events.EndpointActive,
		}))
		return
	}
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	writeJSONValue(w, http.StatusOK, webhookDeliveryBody(st))
}

// webhookDeliveryBody projects the delivery state onto its api/1
// representation. The sealed secret columns are structurally absent from the
// projection — not blanked, not redacted — so there is no path by which a change
// to this struct could start serving one.
func webhookDeliveryBody(st store.WebhookDeliveryState) map[string]any {
	status := st.Status
	if status == "" {
		status = events.EndpointActive
	}
	var lastDelivered any
	if st.LastDeliveredID != "" {
		lastDelivered = st.LastDeliveredID
	}
	var secretSetAt any
	if st.SealedSecret != "" {
		secretSetAt = st.RotatedAtMs
	}
	return map[string]any{
		"endpoint_id":          st.EndpointID,
		"status":               status,
		"consecutive_failures": st.ConsecutiveFailures,
		"last_delivered_id":    lastDelivered,
		"secret_set_at_ms":     secretSetAt,
	}
}
