package wire

import "encoding/json"

// StateSnapshotBody is the relay/1 `state.snapshot` message body
// (relay/1 REL-051): `{generation, hash, signature, sections}`.
// `signed_with_key` is diagnostic-only (REL-075) and omitted when unset.
type StateSnapshotBody struct {
	Generation    int64    `json:"generation"`
	Hash          string   `json:"hash"`
	Signature     string   `json:"signature"`
	SignedWithKey string   `json:"signed_with_key,omitempty"`
	Sections      Sections `json:"sections"`
}

// Sections is the relay/1 `state.snapshot` `sections` object (REL-060):
// exactly these 7 keys, all present in every snapshot (an empty array or
// an explicit empty placeholder where a site has nothing to populate a
// section with yet — never an omitted key).
type Sections struct {
	ScreenPrograms     []ScreenProgram   `json:"screen_programs"`
	EdgeRules          EdgeRules         `json:"edge_rules"`
	DeviceInventory    DeviceInventory   `json:"device_inventory"`
	Schedule           struct{}          `json:"schedule"`
	RevocationAndSite  RevocationAndSite `json:"revocation_and_site"`
	PairingGrants      []PairingGrant    `json:"pairing_grants"`
	WorkflowGeneration any               `json:"workflow_generation"`
}

// ScreenProgram is one relay/1 `screen_programs` entry (REL-061).
// `Priority` is one of `scheduled` or `preempt`; `Display` is one of
// `content` or `blank`.
type ScreenProgram struct {
	ScreenID        string       `json:"screen_id"`
	ProgramRevision string       `json:"program_revision"`
	Priority        string       `json:"priority"`
	Display         string       `json:"display"`
	Content         []ContentRef `json:"content"`
}

// ContentRef is one signed content reference inside a ScreenProgram's
// `content` array (REL-061): `asset_ref` is a content-addressed `sha256:`
// URI in the same form `signhash.ContentID` produces; `url` is where a
// screen fetches the bytes directly (never through the relay, REL-140).
type ContentRef struct {
	AssetRef  string `json:"asset_ref"`
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expires_at"`
}

// EdgeRules is the relay/1 `edge_rules` section (REL-062): a
// `rules_minor_version` naming the `rules/1` minor this generation was
// compiled against, plus an array of `rules/1`'s own CompiledRuleEntry
// objects, carried opaquely — relay/1 does not own that entry shape, so
// Rules elements are left as raw JSON here.
type EdgeRules struct {
	RulesMinorVersion string            `json:"rules_minor_version"`
	Rules             []json.RawMessage `json:"rules"`
}

// DeviceInventory is the relay/1 `device_inventory` section
// (REL-063/064). Devices and PackMatchPatterns elements are left as raw
// JSON here — their producer (device adoption / pack manifests) is out of
// this package's scope.
type DeviceInventory struct {
	Devices           []json.RawMessage `json:"devices"`
	PackMatchPatterns []json.RawMessage `json:"pack_match_patterns"`
}

// RevocationAndSite is the relay/1 `revocation_and_site` section
// (REL-066): `revoked` an array of opaque identifier strings, and
// `site_effective` a persisted copy of the site's placement data.
type RevocationAndSite struct {
	Revoked       []string      `json:"revoked"`
	SiteEffective SiteEffective `json:"site_effective"`
}

// SiteEffective mirrors `{tz, lat, long}` — relay/1's persisted copy of a
// site's placement data (REL-066). Distinct from Hello's SiteBinding
// (REL-036), which additionally carries `scope_node`.
type SiteEffective struct {
	TZ   string  `json:"tz"`
	Lat  float64 `json:"lat"`
	Long float64 `json:"long"`
}

// PairingGrant is one relay/1 `pairing_grants` entry (REL-067,
// Player-credential authority): a pending pairing-grant record minted
// against this site.
type PairingGrant struct {
	GrantID                string `json:"grant_id"`
	Purpose                string `json:"purpose"`
	ResultingPrincipalKind string `json:"resulting_principal_kind"`
	TTL                    int64  `json:"ttl"`
	RedemptionMode         string `json:"redemption_mode"`
	IssuedAt               int64  `json:"issued_at"`
}
