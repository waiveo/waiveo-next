package deviceclass

// builtinMediaPlayer is the platform's byte-exact, contract-pinned media-player
// ClassEntry (device-class-registry/1 REG-060-066). Its content — the eight-member
// state vocabulary in order (REG-061), off as the unknown-state fallback (REG-062),
// the on/off semantic groups (REG-063), the six typed attributes with their
// types/nullability/values/change_emission (REG-064), and the four commands with
// their params (REG-066) — MUST equal the contract's own normative tables AND the
// frozen corpus oracle conformance/corpora/device-class-registry/roku-media-player.json
// field-for-field. TestBuiltinMediaPlayerMatchesCorpus enforces that equality; any
// added, missing, renamed, or reordered member is a defect. Growth is exclusively a
// minor-version change, never a silent edit.
//
// home takes no parameters: its Params is the empty, non-nil slice REG-051 requires,
// mirroring the corpus's "params": [] exactly (a nil Params would drift from it).
var builtinMediaPlayer = ClassEntry{
	Origin:               "built-in",
	States:               []string{"on", "playing", "paused", "buffering", "idle", "off", "standby", "unavailable"},
	UnknownStateFallback: "off",
	SemanticGroups: map[string][]string{
		"on":  {"on", "playing", "idle", "paused", "buffering"},
		"off": {"off", "standby", "unavailable"},
	},
	Attributes: []Attribute{
		{Name: "active_app", Type: "string", Nullable: true, ChangeEmission: "significant"},
		{Name: "active_app_id", Type: "string", Nullable: true, ChangeEmission: "significant"},
		{Name: "app_type", Type: "enum", Values: []string{"app", "screensaver", "home", "menu"}, Nullable: true, ChangeEmission: "significant"},
		{Name: "power_mode", Type: "string", Nullable: true, ChangeEmission: "significant"},
		{Name: "is_screensaver", Type: "boolean", Nullable: false, ChangeEmission: "significant"},
		{Name: "app_version", Type: "string", Nullable: true, ChangeEmission: "cosmetic"},
	},
	Commands: []Command{
		{Name: "launch", Params: []Param{{Name: "channel", Type: "string", Required: true}}},
		{Name: "home", Params: []Param{}},
		{Name: "keypress", Params: []Param{{Name: "key", Type: "string", Required: true}}},
		{Name: "power", Params: []Param{{Name: "state", Type: "enum", Values: []string{"on", "off"}, Required: true}}},
	},
}

// builtinRawRegistry is the platform's built-in device-class registry document: the
// single canonical media-player class device-class-registry/1 requires (REG-060), the
// one place its vocabulary is defined. The resolution API (New/Builtin) indexes it,
// and every consumer — the rules engine's registry surface and the relay device
// plane's command vocabulary — resolves device commands and matches device states
// against this one canonical source rather than a hand-maintained duplicate.
var builtinRawRegistry = RawRegistry{
	DeviceClasses: map[string]ClassEntry{
		"media-player": builtinMediaPlayer,
	},
}
