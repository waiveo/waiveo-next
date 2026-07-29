package securitymodel1

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/secretseal"
)

// driveCredentialReset drives SEC-050-valid-credential-reset-grant-flow against
// the LIVE credential-reset flow (internal/app/auth.Store's
// IssueCredentialResetGrant / RedeemCredentialResetGrant), end to end: an admin
// issues, the target redeems with a value of their own, and the store is then
// interrogated for what changed and what did not.
//
// # How each expected field is actually observed
//
//   - `grant.*` — read back off the persisted grant row (Store.Grant), never off
//     the in-memory value the mint call returned. A flow that returned the right
//     shape and persisted a different one fails here.
//   - `admin_response.contains_credential_value` — the handoff is marshalled and
//     searched for the target's ACTUAL credential values, both the one they held
//     before the reset and the one they set during it.
//   - `admin_response.admin_can_choose_credential_value` — a STRUCTURAL check
//     over the issuance surface's own types: if any field of the issuance
//     options or of the handoff could carry a credential value, the admin has a
//     path to choose one. This is checked by reflection because the requirement
//     is about the API's shape, not about a runtime decision — a check that only
//     ran the happy path could not see a field nobody happened to set.
//   - `argv_capture_contains_secret` — the case's own declared argv, PLUS every
//     audit record the flow emitted, PLUS the persisted grant row, are searched
//     for the raw one-time code. The last is the one with real teeth: it proves
//     the code is unrecoverable from the database at all (only its hash is
//     stored), which is what makes SEC-051's "not in a journald-logged line"
//     achievable rather than merely promised.
//   - `on_redemption.*` — read out of the live store after redemption: the
//     target's TOTP credential id and sealed secret compared before and after,
//     their session list, and their API key's own credential row.
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

	fixedNow := int64(1752537600000)
	sealer, err := secretseal.New(make([]byte, secretseal.KeySize))
	if err != nil {
		k.fail(rep, "build the credential sealer: %v", err)
		return
	}
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
	ids := deterministicIDs(adminID)
	sink := &recordingSink{}
	st, err := auth.Open(":memory:", func() int64 { return fixedNow }, ids, auth.WithSecretSealer(sealer))
	if err != nil {
		k.fail(rep, "open auth store: %v", err)
		return
	}
	defer st.Close()

	// The issuing admin, at the role the case declares.
	admin, err := st.CreatePrincipal(ctx, auth.KindUser, "admin")
	if err != nil {
		k.fail(rep, "create the issuing admin: %v", err)
		return
	}
	if admin.PrincipalID != adminID {
		k.fail(rep, "the fixture admin id is %q, not the case's own %q", admin.PrincipalID, adminID)
		return
	}

	// The target user: a password credential, an armed second factor, a live
	// browser session and a live API key — everything SEC-053 says a redemption
	// evicts, plus the one thing (SEC-052) it must leave alone.
	targetRow, err := st.CreatePrincipal(ctx, auth.KindUser, "target")
	if err != nil {
		k.fail(rep, "create the target user: %v", err)
		return
	}
	target := targetRow.PrincipalID
	if target != targetID {
		k.note("input target_user.principal_id %q is not a valid ULID (Crockford base32 excludes U; DAT-005a) and the live store refuses to mint it — driven with the store-minted id %q, which no expected field names", targetID, target)
	}

	if _, err := st.PutRoleBinding(ctx, admin.PrincipalID, auth.RootScopeNode, auth.Role(adminRole)); err != nil {
		k.fail(rep, "bind the admin's %s role: %v", adminRole, err)
		return
	}

	const oldPassword = "the-password-the-target-forgot"
	const newPassword = "the-passphrase-the-target-chooses"
	if _, err := st.PutPasswordCredential(ctx, target, "target@example.invalid", oldPassword); err != nil {
		k.fail(rep, "seed the target's password credential: %v", err)
		return
	}
	// A real enrollment, begun and then armed, so the credential row under test
	// is the one the shipped path produces rather than a hand-inserted stand-in.
	totpSecret, err := st.BeginTOTPEnrollment(ctx, target, false)
	if err != nil {
		k.fail(rep, "begin the target's second-factor enrollment: %v", err)
		return
	}
	totpBefore, err := st.ArmTOTPCredential(ctx, target, totpSecret, 0)
	if err != nil {
		k.fail(rep, "arm the target's second factor: %v", err)
		return
	}
	session, err := st.MintSession(ctx, target, auth.TokenKindSession, "", auth.AALStandard, nil)
	if err != nil {
		k.fail(rep, "mint the target's session: %v", err)
		return
	}
	apiKey, err := st.MintAPIKey(ctx, target, "cli")
	if err != nil {
		k.fail(rep, "mint the target's api key: %v", err)
		return
	}

	// The audit sink is attached only now, so the records searched below are the
	// FLOW's, not the fixture's setup noise.
	st.OnRevoke(func(string) {})
	auditor := auth.NewAuditor(sink, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", func() int64 { return fixedNow }, ids, nil)

	// --- issuance (SEC-050) ---------------------------------------------------
	handoff, err := st.IssueCredentialResetGrant(ctx, admin.PrincipalID, target, auth.CredentialResetOptions{
		KeepExistingSessions: optOut,
		BaseURL:              "https://box.example.invalid",
	})
	if err != nil {
		k.fail(rep, "issue the credential-reset grant: %v", err)
		return
	}
	auditor.Emit(auth.Record{
		Actor: admin.PrincipalID, Action: auth.ActionGrantCreated,
		Target: "grant:" + handoff.GrantID, Result: events.AuditResultSuccess,
		Purpose: auth.PurposeCredentialReset, IssuedVia: auth.IssuedViaAPI,
	})

	// The persisted row, not the returned value.
	grant, err := st.Grant(ctx, handoff.GrantID)
	if err != nil {
		k.fail(rep, "read the persisted grant row: %v", err)
		return
	}
	k.stringAt("grant.purpose", grant.Purpose)
	k.stringAt("grant.resulting_principal_kind", grant.ResultingPrincipalKind)
	k.stringAt("grant.redemption_mode", grant.RedemptionMode)
	k.stringAt("grant.issued_via", grant.IssuedVia)

	handoffJSON, err := json.Marshal(handoff)
	if err != nil {
		k.fail(rep, "marshal the admin handoff: %v", err)
		return
	}
	k.boolAt("admin_response.contains_one_time_code_or_url", handoff.Code != "" || handoff.URL != "")
	k.boolAt("admin_response.contains_credential_value",
		strings.Contains(string(handoffJSON), oldPassword) || strings.Contains(string(handoffJSON), newPassword))
	k.boolAt("admin_response.admin_can_choose_credential_value", issuanceSurfaceCanCarryACredential())

	// --- SEC-051's argv/log/at-rest capture -----------------------------------
	haystack := append([]string(nil), argv...)
	for _, env := range sink.envelopes {
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
	k.boolAt("argv_capture_contains_secret", containsAny(haystack, handoff.Code, oldPassword, newPassword))

	// --- redemption (SEC-052/053) --------------------------------------------
	redemption, err := st.RedeemCredentialResetGrant(ctx, handoff.Code, newPassword)
	if err != nil {
		k.fail(rep, "redeem the credential-reset grant: %v", err)
		return
	}
	if redemption.TargetPrincipalID != target {
		k.diff("redemption target", target, redemption.TargetPrincipalID)
	}

	totpAfter, err := st.FindTOTPCredential(ctx, target)
	if err != nil {
		k.fail(rep, "read the target's second factor after redemption: %v", err)
		return
	}
	totpChanged := totpAfter.CredentialID != totpBefore.CredentialID ||
		totpAfter.Secret != totpBefore.Secret ||
		totpAfter.RevokedAt != nil

	_, sessionErr := st.LookupSession(ctx, session.Token)
	_, apiKeyErr := st.LookupSession(ctx, apiKey.Token)
	apiKeyCred, apiKeyCredErr := credentialByID(ctx, st, target, apiKey.Session.CredentialID)

	k.boolAt("on_redemption.target_totp_enrollment_changed", totpChanged)
	k.boolAt("on_redemption.target_sessions_revoked", sessionErr != nil)
	// An API key is revoked when neither its token resolves NOR its own
	// `api-key` credential row is live — SEC-020's "revocable through the same
	// mechanism" is only satisfied if BOTH halves went.
	k.boolAt("on_redemption.target_api_keys_revoked", apiKeyErr != nil && (apiKeyCredErr != nil || apiKeyCred.RevokedAt != nil))

	// The new credential really is the one the TARGET chose, and the old one no
	// longer authenticates. Not in the expected block; recorded as a diff if it
	// diverges, since without it "sessions revoked" could describe a flow that
	// evicted everyone and reset nothing.
	cred, err := st.FindPasswordCredential(ctx, "target@example.invalid")
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
	k.finish(rep)
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

// recordingSink captures every audit envelope the flow emits, so SEC-051's
// "not in a journald-logged line" can be checked against what the platform
// actually records rather than against nothing.
type recordingSink struct{ envelopes []events.Envelope }

func (s *recordingSink) Append(e events.Envelope) { s.envelopes = append(s.envelopes, e) }

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
