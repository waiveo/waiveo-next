package securitymodel1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/secretseal"
)

// The two routes internal/app/api actually mounts for the routine credential
// reset (SEC-050). They are named here so a case that fails because a route
// moved says so, rather than reporting a 404 as a contract violation.
const (
	credentialResetRoute       = "/api/v1/auth/credential-reset"
	credentialResetRedeemRoute = "/api/v1/auth/credential-reset/redeem"
)

// driveCredentialReset drives SEC-050-valid-credential-reset-grant-flow against
// the LIVE, HTTP-MOUNTED credential-reset routes — internal/app/api's own mux,
// the same handler a browser and a CLI reach — end to end: an authenticated
// admin issues, the target redeems with a value of their own over the
// unauthenticated redemption route, and the store is then interrogated for what
// changed and what did not.
//
// # What changed here, and why it matters
//
// This case used to call Store.IssueCredentialResetGrant and
// Store.RedeemCredentialResetGrant directly, because nothing else called them:
// the traceability note recorded outright that "the credential-reset flow has no
// caller ... an operator cannot invoke any of it". Two of its assertions were
// worth less than they looked as a result. The argv/log search ran over an audit
// record THIS DRIVER constructed and emitted, so it proved nothing about what
// the platform records; and `admin_response.*` described a Go struct rather than
// a response body an admin ever receives. Both now come off the shipped surface.
//
// # How each expected field is actually observed
//
//   - `grant.*` — read back off the persisted grant row (Store.Grant), never off
//     the response body. A route that returned the right shape and persisted a
//     different one fails here.
//   - `admin_response.contains_credential_value` — the ISSUING ROUTE'S OWN
//     RESPONSE BODY, as bytes, searched for the target's actual credential
//     values: the one they held before the reset and the one they set during it.
//   - `admin_response.admin_can_choose_credential_value` — TWO checks, and both
//     must say no. A structural one over the issuance surface's Go types (a
//     field a credential could travel in is a path to choose one, whether or not
//     any test sets it), and a BEHAVIORAL one on the wire: the issuing request
//     carries an unsolicited `password` member, and the target's credential must
//     be exactly what it was before afterwards. The structural check alone cannot
//     see a handler that reads an undeclared field; the behavioral check alone
//     cannot see a field nobody happened to send.
//   - `argv_capture_contains_secret` — the case's own declared argv, PLUS every
//     audit record the SHIPPED FLOW emitted (a real events sink wired into the
//     store and the authenticator, not a record this driver wrote), PLUS the
//     persisted grant row, are searched for the raw one-time code. The last is
//     the one with real teeth: it proves the code is unrecoverable from the
//     database at all, which is what makes SEC-051's "not in a journald-logged
//     line" achievable rather than merely promised.
//   - `on_redemption.*` — read out of the live store after the redemption ROUTE
//     returned: the target's TOTP credential id and sealed secret compared before
//     and after, their session list, and their API key's own credential row.
func driveCredentialReset(rep *report.Report, c corpus.Case) {
	k := newCheck(c)
	ctx := context.Background()

	adminID, okAdmin := inputString(c, "issuing_admin.principal_id")
	adminRole, okRole := inputString(c, "issuing_admin.role")
	targetID, okTarget := inputString(c, "target_user.principal_id")
	argv, okArgv := inputStrings(c, "argv_capture")
	optOutAny, okOptOut := digInput(c, "opt_out_session_revocation")
	if !okAdmin || !okRole || !okTarget || !okArgv || !okOptOut {
		k.fail(rep, "the frozen input block is missing a field this case is driven from")
		return
	}
	optOut, _ := optOutAny.(bool)

	dir, err := os.MkdirTemp("", "secmodel1-reset-")
	if err != nil {
		k.fail(rep, "scratch dir: %v", err)
		return
	}
	defer os.RemoveAll(dir)

	// The issuing admin's id the case itself names is minted for real, so the
	// grant and the audit records name the case's own subject.
	//
	// The TARGET's is not, and this is a genuine finding rather than a driver
	// shortcut: the frozen case's `target_user.principal_id`
	// ("01J8Z3K4N5P6Q7R8S9T0V1W2US") contains a `U`, which Crockford base32 — the
	// ULID alphabet DAT-005a requires and internal/shared/ulid enforces — does not
	// contain. The live store refuses to mint it. The expected block asserts
	// nothing about the target's id, so the case is driven with a store-minted
	// one and the divergence is recorded as a Note.
	h, err := newCredentialResetHarness(dir, corpusInstantMs, deterministicIDs(adminID))
	if err != nil {
		k.fail(rep, "build the credential-reset harness: %v", err)
		return
	}
	defer h.close()

	admin, err := h.authStore.CreatePrincipal(ctx, auth.KindUser, "admin")
	if err != nil {
		k.fail(rep, "create the issuing admin: %v", err)
		return
	}
	if admin.PrincipalID != adminID {
		k.fail(rep, "the fixture admin id is %q, not the case's own %q", admin.PrincipalID, adminID)
		return
	}
	if _, err := h.authStore.PutRoleBinding(ctx, admin.PrincipalID, auth.RootScopeNode, auth.Role(adminRole)); err != nil {
		k.fail(rep, "bind the admin's %s role: %v", adminRole, err)
		return
	}
	// The admin authenticates the way a real client does: a bearer credential the
	// middleware resolves, not a principal this driver asserts into a context.
	// The whole issuing route — including SEC-012's `admin` floor — therefore runs
	// exactly as it does for a signed-in operator.
	adminKey, err := h.authStore.MintAPIKey(ctx, admin.PrincipalID, "conformance")
	if err != nil {
		k.fail(rep, "mint the admin's api key: %v", err)
		return
	}

	// The target user: a password credential, an armed second factor, a live
	// browser session and a live API key — everything SEC-053 says a redemption
	// evicts, plus the one thing (SEC-052) it must leave alone.
	targetRow, err := h.authStore.CreatePrincipal(ctx, auth.KindUser, "target")
	if err != nil {
		k.fail(rep, "create the target user: %v", err)
		return
	}
	target := targetRow.PrincipalID
	if target != targetID {
		k.note("input target_user.principal_id %q is not a valid ULID (Crockford base32 excludes U; DAT-005a) and the live store refuses to mint it — driven with the store-minted id %q, which no expected field names", targetID, target)
	}

	const oldPassword = "the-password-the-target-forgot"
	const newPassword = "the-passphrase-the-target-chooses"
	// The value an ADMIN would choose if the surface let them. It is sent on the
	// issuing request as an undeclared member; nothing may act on it.
	const adminChosenPassword = "the-value-the-admin-tried-to-choose"
	if _, err := h.authStore.PutPasswordCredential(ctx, target, "target@example.invalid", oldPassword); err != nil {
		k.fail(rep, "seed the target's password credential: %v", err)
		return
	}
	// A real enrollment, begun and then armed, so the credential row under test
	// is the one the shipped path produces rather than a hand-inserted stand-in.
	totpSecret, err := h.authStore.BeginTOTPEnrollment(ctx, target, false)
	if err != nil {
		k.fail(rep, "begin the target's second-factor enrollment: %v", err)
		return
	}
	totpBefore, err := h.authStore.ArmTOTPCredential(ctx, target, totpSecret, 0)
	if err != nil {
		k.fail(rep, "arm the target's second factor: %v", err)
		return
	}
	session, err := h.authStore.MintSession(ctx, target, auth.TokenKindSession, "", auth.AALStandard, nil)
	if err != nil {
		k.fail(rep, "mint the target's session: %v", err)
		return
	}
	apiKey, err := h.authStore.MintAPIKey(ctx, target, "cli")
	if err != nil {
		k.fail(rep, "mint the target's api key: %v", err)
		return
	}

	// Everything above is fixture. The sink is emptied here so the records
	// searched below are the FLOW's, not the setup's.
	h.sink.reset()

	// --- issuance (SEC-050), over the mounted route ---------------------------
	issued := h.issue(adminKey.Token, map[string]any{
		"target_principal_id":    target,
		"keep_existing_sessions": optOut,
		// Not part of the declared request schema. A handler that honoured it
		// would give the issuing admin exactly the path SEC-050 forbids.
		"password": adminChosenPassword,
	})
	if issued.status != http.StatusCreated {
		k.fail(rep, "the issuing route refused the admin (%d %s)", issued.status, issued.raw)
		return
	}
	grantID, _ := issued.body["grant_id"].(string)
	code, _ := issued.body["code"].(string)
	if grantID == "" || code == "" {
		k.fail(rep, "the issuing route returned no grant_id/code: %s", issued.raw)
		return
	}

	// The persisted row, not the response body.
	grant, err := h.authStore.Grant(ctx, grantID)
	if err != nil {
		k.fail(rep, "read the persisted grant row: %v", err)
		return
	}
	k.stringAt("grant.purpose", grant.Purpose)
	k.stringAt("grant.resulting_principal_kind", grant.ResultingPrincipalKind)
	k.stringAt("grant.redemption_mode", grant.RedemptionMode)
	k.stringAt("grant.issued_via", grant.IssuedVia)

	url, _ := issued.body["url"].(string)
	k.boolAt("admin_response.contains_one_time_code_or_url", code != "" || url != "")
	k.boolAt("admin_response.contains_credential_value",
		bytes.Contains(issued.raw, []byte(oldPassword)) ||
			bytes.Contains(issued.raw, []byte(newPassword)) ||
			bytes.Contains(issued.raw, []byte(adminChosenPassword)))

	// The behavioral half of "no path to choose": the admin sent a value, and the
	// target's credential must be untouched by the issuance.
	afterIssue, err := h.authStore.FindPasswordCredential(ctx, "target@example.invalid")
	if err != nil {
		k.fail(rep, "read the target's credential after issuance: %v", err)
		return
	}
	adminChoiceTook := auth.VerifyPassword(afterIssue.Secret, adminChosenPassword) == nil ||
		auth.VerifyPassword(afterIssue.Secret, oldPassword) != nil
	k.boolAt("admin_response.admin_can_choose_credential_value",
		issuanceSurfaceCanCarryACredential() || adminChoiceTook)

	// --- SEC-051's argv/log/at-rest capture -----------------------------------
	haystack := append([]string(nil), argv...)
	for _, env := range h.sink.snapshot() {
		blob, err := json.Marshal(env)
		if err != nil {
			k.fail(rep, "marshal an emitted audit record: %v", err)
			return
		}
		haystack = append(haystack, string(blob))
	}
	// The persisted grant row, serialized whole: whatever a `SELECT *` of the
	// grants table could hand an operator, a log shipper, or an attacker.
	grantBlob, err := json.Marshal(grant)
	if err != nil {
		k.fail(rep, "marshal the persisted grant row: %v", err)
		return
	}
	haystack = append(haystack, string(grantBlob))
	k.boolAt("argv_capture_contains_secret", containsAny(haystack, code, oldPassword, newPassword))

	// --- redemption (SEC-052/053), over the unauthenticated route -------------
	// No credential is presented: the whole premise is a caller who cannot sign
	// in. If this route were mounted behind the auth middleware it would answer
	// 401 here and the case would fail, which is the assertion that the exemption
	// is real rather than intended.
	redeemed := h.redeem(code, newPassword)
	if redeemed.status != http.StatusNoContent {
		k.fail(rep, "the redemption route refused the target (%d %s)", redeemed.status, redeemed.raw)
		return
	}

	totpAfter, err := h.authStore.FindTOTPCredential(ctx, target)
	if err != nil {
		k.fail(rep, "read the target's second factor after redemption: %v", err)
		return
	}
	totpChanged := totpAfter.CredentialID != totpBefore.CredentialID ||
		totpAfter.Secret != totpBefore.Secret ||
		totpAfter.RevokedAt != nil

	_, sessionErr := h.authStore.LookupSession(ctx, session.Token)
	_, apiKeyErr := h.authStore.LookupSession(ctx, apiKey.Token)
	apiKeyCred, apiKeyCredErr := credentialByID(ctx, h.authStore, target, apiKey.Session.CredentialID)

	k.boolAt("on_redemption.target_totp_enrollment_changed", totpChanged)
	k.boolAt("on_redemption.target_sessions_revoked", sessionErr != nil)
	// An API key is revoked when neither its token resolves NOR its own
	// `api-key` credential row is live — SEC-020's "revocable through the same
	// mechanism" is only satisfied if BOTH halves went.
	k.boolAt("on_redemption.target_api_keys_revoked", apiKeyErr != nil && (apiKeyCredErr != nil || apiKeyCred.RevokedAt != nil))

	// The new credential really is the one the TARGET chose, and neither the old
	// one nor the one the admin tried to supply authenticates. Not in the expected
	// block; recorded as a diff if it diverges, since without it "sessions
	// revoked" could describe a flow that evicted everyone and reset nothing.
	cred, err := h.authStore.FindPasswordCredential(ctx, "target@example.invalid")
	if err != nil {
		k.fail(rep, "read the target's password credential after redemption: %v", err)
		return
	}
	if err := auth.VerifyPassword(cred.Secret, newPassword); err != nil {
		k.diff("the target's new credential authenticates", true, false)
	}
	if err := auth.VerifyPassword(cred.Secret, oldPassword); err == nil {
		k.diff("the target's old credential still authenticates", false, true)
	}
	if err := auth.VerifyPassword(cred.Secret, adminChosenPassword); err == nil {
		k.diff("the value the admin supplied became the credential", false, true)
	}
	k.finish(rep)
}

// driveGrantAuditRecord drives
// SEC-034-valid-grant-audit-carries-purpose-and-issued-via against the same
// shipped routes, over the same real events sink.
//
// SEC-034 is one sentence about the audit trail: "Every grant creation and every
// grant redemption MUST emit an audit.event carrying that grant's purpose and
// issued_via in its payload, so a recovery-purpose, console-issued redemption is
// distinguishable in the audit trail from a routine invite redemption months
// later." Nothing asserted it. The credential-reset flow emitted neither record,
// and the SEC-050 case above could not have noticed, because the only record in
// its haystack was one the driver itself constructed and emitted.
//
// So this case reads the ENVELOPES a real events sink received while the shipped
// routes ran, and asserts on the two fields the requirement names. It is driven
// on the credential-reset flow because that is the flow whose records were
// missing; the emission itself lives in MintGrant/RedeemGrant, so every other
// purpose gets it from the same statements.
func driveGrantAuditRecord(rep *report.Report, c corpus.Case) {
	k := newCheck(c)
	ctx := context.Background()

	wantPurpose, okPurpose := inputString(c, "grant.purpose")
	wantVia, okVia := inputString(c, "grant.issued_via")
	if !okPurpose || !okVia {
		k.fail(rep, "the frozen input block is missing a field this case is driven from")
		return
	}

	dir, err := os.MkdirTemp("", "secmodel1-grantaudit-")
	if err != nil {
		k.fail(rep, "scratch dir: %v", err)
		return
	}
	defer os.RemoveAll(dir)

	h, err := newCredentialResetHarness(dir, corpusInstantMs, deterministicIDs())
	if err != nil {
		k.fail(rep, "build the credential-reset harness: %v", err)
		return
	}
	defer h.close()

	admin, target, adminToken, err := h.seedAdminAndTarget(ctx, "grant-audit@example.invalid", "the-password-being-replaced")
	if err != nil {
		k.fail(rep, "seed the fixture: %v", err)
		return
	}
	_ = admin
	h.sink.reset()

	issued := h.issue(adminToken, map[string]any{"target_principal_id": target})
	if issued.status != http.StatusCreated {
		k.fail(rep, "the issuing route refused the admin (%d %s)", issued.status, issued.raw)
		return
	}
	code, _ := issued.body["code"].(string)
	const chosen = "the-passphrase-the-target-chooses"
	if red := h.redeem(code, chosen); red.status != http.StatusNoContent {
		k.fail(rep, "the redemption route refused the target (%d %s)", red.status, red.raw)
		return
	}

	created := h.sink.byAction(auth.ActionGrantCreated)
	redeemed := h.sink.byAction(auth.ActionGrantRedeemed)

	k.boolAt("grant_created.emitted", len(created) == 1)
	k.stringAt("grant_created.purpose", auditField(created, "purpose"))
	k.stringAt("grant_created.issued_via", auditField(created, "issued_via"))
	k.boolAt("grant_redeemed.emitted", len(redeemed) == 1)
	k.stringAt("grant_redeemed.purpose", auditField(redeemed, "purpose"))
	k.stringAt("grant_redeemed.issued_via", auditField(redeemed, "issued_via"))

	// The two fields must describe the grant that was actually persisted, not a
	// pair of literals a producer could hardcode. Not in the expected block —
	// recorded as a diff — because the corpus already names the values and this
	// is the check that they came from the row.
	if auditField(created, "purpose") != wantPurpose || auditField(created, "issued_via") != wantVia {
		k.diff("grant.created reflects the persisted grant", wantPurpose+"/"+wantVia,
			auditField(created, "purpose")+"/"+auditField(created, "issued_via"))
	}

	// SEC-051 rides along: neither mandatory record may carry the code.
	var records []string
	for _, env := range h.sink.snapshot() {
		blob, _ := json.Marshal(env)
		records = append(records, string(blob))
	}
	k.boolAt("records_contain_secret", containsAny(records, code, chosen))
	k.finish(rep)
}

// auditField reads one member out of exactly one matched audit envelope. Zero or
// several matches returns a value no expected string can equal, so "the record
// was not emitted" and "the record carried the wrong value" are both diffs
// rather than one silently reading as the other.
func auditField(matched []events.Envelope, field string) string {
	if len(matched) != 1 {
		return fmt.Sprintf("<%d matching records>", len(matched))
	}
	raw, err := json.Marshal(matched[0])
	if err != nil {
		return "<unmarshalable record>"
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "<undecodable record>"
	}
	// events/1 wraps the record's own members under `payload`; a producer that
	// put them at the envelope's top level instead would be a second audit
	// schema, so this looks in one place only.
	payload, _ := decoded["payload"].(map[string]any)
	v, _ := payload[field].(string)
	if v == "" {
		return "<absent>"
	}
	return v
}

// credentialResetHarness mounts the SAME http.Handler production wires (api.New) over a
// real auth store on disk, with a REAL events sink attached to the store and to
// the authenticator — so every audit record this driver reads was emitted by
// shipped code through the shipped path, not written by the driver.
type credentialResetHarness struct {
	authStore *auth.Store
	appStore  *store.Store
	handler   http.Handler
	sink      *recordingSink
	nowMs     *int64
}

func newCredentialResetHarness(dir string, startMs int64, ids func() string) (*credentialResetHarness, error) {
	now := startMs
	clock := func() int64 { return now }
	sealer, err := secretseal.New(make([]byte, secretseal.KeySize))
	if err != nil {
		return nil, fmt.Errorf("build the credential sealer: %w", err)
	}
	sink := &recordingSink{}
	auditor := auth.NewAuditor(sink, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", clock, ids, nil)
	authStore, err := auth.Open(dir+"/auth.db", clock, ids,
		auth.WithSecretSealer(sealer), auth.WithAuditor(auditor))
	if err != nil {
		return nil, err
	}
	appStore, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		_ = authStore.Close()
		return nil, err
	}
	authn := auth.NewAuthenticator(authStore, auditor, auth.NewDefaultLockout(), auth.NewRevocations())
	handler := api.New(appStore, apihttp.NewIdempotencyStore(clock, 0), clock, ids,
		origin.New(), "https://origin.example", authn, api.WithJobRunner(api.NewJobRunner()))
	return &credentialResetHarness{authStore: authStore, appStore: appStore, handler: handler, sink: sink, nowMs: &now}, nil
}

func (h *credentialResetHarness) close() {
	_ = h.authStore.Close()
	_ = h.appStore.Close()
}

// seedAdminAndTarget creates an admin holding a bearer credential and a target
// holding a password credential, returning both ids and the admin's token.
func (h *credentialResetHarness) seedAdminAndTarget(ctx context.Context, identifier, password string) (adminID, targetID, adminToken string, err error) {
	admin, err := h.authStore.CreatePrincipal(ctx, auth.KindUser, "admin")
	if err != nil {
		return "", "", "", err
	}
	if _, err := h.authStore.PutRoleBinding(ctx, admin.PrincipalID, auth.RootScopeNode, auth.RoleAdmin); err != nil {
		return "", "", "", err
	}
	key, err := h.authStore.MintAPIKey(ctx, admin.PrincipalID, "conformance")
	if err != nil {
		return "", "", "", err
	}
	target, err := h.authStore.CreatePrincipal(ctx, auth.KindUser, "target")
	if err != nil {
		return "", "", "", err
	}
	if _, err := h.authStore.PutPasswordCredential(ctx, target.PrincipalID, identifier, password); err != nil {
		return "", "", "", err
	}
	return admin.PrincipalID, target.PrincipalID, key.Token, nil
}

// issue drives one credential-reset issuance through the live mux, as the bearer
// of token.
func (h *credentialResetHarness) issue(token string, body map[string]any) httpResult {
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, credentialResetRoute, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return h.serve(req)
}

// redeem drives one redemption through the live mux, presenting NO credential.
func (h *credentialResetHarness) redeem(code, password string) httpResult {
	payload, _ := json.Marshal(map[string]string{"code": code, "password": password})
	req := httptest.NewRequest(http.MethodPost, credentialResetRedeemRoute, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	return h.serve(req)
}

func (h *credentialResetHarness) serve(req *http.Request) httpResult {
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return httpResult{status: rec.Code, body: decoded, raw: rec.Body.Bytes()}
}

// credentialSuspect matches a field name that could carry a credential value.
var credentialSuspect = regexp.MustCompile(`(?i)pass(word|phrase)|secret|credential|newpass`)

// issuanceSurfaceCanCarryACredential reports whether the credential-reset
// ISSUANCE surface has any field through which the issuing admin could supply or
// receive a credential value (SEC-050: the admin "MUST NOT be shown, and MUST
// have no path to choose, the credential value the target user eventually
// sets").
//
// This is a structural check, deliberately. The requirement is about what the
// API makes POSSIBLE, and a behavioral check can only observe what one happy
// path did — it would keep passing on the day somebody adds a `NewPassword`
// field to the options struct and no test happens to set it.
//
// `CredentialReset` is in both type names, so the name of the TYPE is not what
// is matched; only its FIELDS are.
func issuanceSurfaceCanCarryACredential() bool {
	for _, t := range []reflect.Type{
		reflect.TypeOf(auth.CredentialResetOptions{}),
		reflect.TypeOf(auth.CredentialResetHandoff{}),
	} {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if credentialSuspect.MatchString(f.Name) {
				return true
			}
		}
	}
	return false
}

// containsAny reports whether any needle appears anywhere in the haystack.
func containsAny(haystack []string, needles ...string) bool {
	for _, hay := range haystack {
		for _, needle := range needles {
			if needle != "" && strings.Contains(hay, needle) {
				return true
			}
		}
	}
	return false
}

// recordingSink captures every audit envelope the SHIPPED flow emits, so
// SEC-034's mandatory records and SEC-051's "not in a journald-logged line" can
// both be checked against what the platform actually records rather than against
// something the driver wrote.
// It is guarded because Append does NOT run on the driver's own goroutine. The
// console binding's listener accepts on its own goroutine and audits from there
// (auth.ConsoleListener.serveConn -> Auditor -> this sink), while a case reads
// the collected set from the driver's. Unguarded, that is a data race the race
// detector reports — and worse than a race in ordinary test scaffolding, because
// a case that reads this set with no happens-before edge to the writes it is
// checking may observe a partial one. Several security-model rows are `covered`
// on the strength of what these cases assert about recorded audit records; a
// verdict taken from a racy read is not reproducible-for-the-right-reason, which
// is the whole distinction the coverage bar exists to draw.
type recordingSink struct {
	mu        sync.Mutex
	envelopes []events.Envelope
}

func (s *recordingSink) Append(e events.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envelopes = append(s.envelopes, e)
}

// reset drops the fixture's own emissions so a case reads only the flow's.
func (s *recordingSink) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envelopes = nil
}

func (s *recordingSink) snapshot() []events.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.Envelope(nil), s.envelopes...)
}

// byAction returns every recorded envelope whose audit action is action.
func (s *recordingSink) byAction(action string) []events.Envelope {
	var out []events.Envelope
	for _, env := range s.snapshot() {
		raw, err := json.Marshal(env)
		if err != nil {
			continue
		}
		var decoded struct {
			Payload struct {
				Action string `json:"action"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			continue
		}
		if decoded.Payload.Action == action {
			out = append(out, env)
		}
	}
	return out
}

// credentialByID reads one of principalID's credential rows by id.
func credentialByID(ctx context.Context, st *auth.Store, principalID, credentialID string) (auth.CredentialRow, error) {
	rows, err := st.Credentials(ctx, principalID)
	if err != nil {
		return auth.CredentialRow{}, err
	}
	for _, r := range rows {
		if r.CredentialID == credentialID {
			return r, nil
		}
	}
	return auth.CredentialRow{}, errNoCredential
}

var errNoCredential = errorString("auth: no such credential row")

type errorString string

func (e errorString) Error() string { return string(e) }
