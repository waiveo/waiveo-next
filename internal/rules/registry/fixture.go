package registry

// FixtureRegistry is the small, fixed built-in registry the rules-1 corpus
// documents beside itself (rules-1 Conformance notes: "corpus cases use a
// small fixed fixture registry documented beside the corpus, the same
// convention manifest/1's corpus notes establish"). It carries exactly one
// device class, media-player, whose states, semantic groups, and commands
// are device-class-registry/1's REG-061/063/066-pinned built-in content —
// kept in lockstep by hand with that contract's own corpus and with
// conformance/corpora/rules-1/RUL-321-scalar-expands-array-exact.json, per
// device-class-registry/1's own cross-contract-alignment note — plus a
// couple of typed attributes this contract's own corpus cases exercise
// beyond REG-064's pinned list, so a corpus fixture referencing them (e.g.
// RUL-023-attribute-only-vs-state-change's "volume") resolves without
// inventing a second device class.
//
// media-player semantic groups (device-class-registry/1 REG-063):
//
//	"on":  on, playing, idle, paused, buffering
//	"off": off, standby, unavailable
//
// media-player typed attributes:
//
//	active_app      String   significant  (REG-064)
//	active_app_id   String   significant  (REG-064)
//	app_type        String   significant  (REG-064; enum app/screensaver/home/menu, compared as String — RUL-260/263)
//	power_mode      String   significant  (REG-064)
//	is_screensaver  Boolean  significant  (REG-064)
//	app_version     String   cosmetic     (REG-064)
//	volume          Number   significant  (corpus-only, not REG-064-pinned)
//	media_title     String   cosmetic     (corpus-only, not REG-064-pinned)
//
// media-player commands (device-class-registry/1 REG-066): launch, home,
// keypress, power.
//
// FixtureRegistry has no extension-registered content of its own: every
// SemanticGroups call resolves only against the built-in groups above.
type FixtureRegistry struct{}

var _ Registry = FixtureRegistry{}

// fixtureGroup is one device class's one built-in semantic group: a name and
// its member states, in the fixed declaration order device-class-registry/1
// pins for media-player (REG-063).
type fixtureGroup struct {
	name    string
	members []string
}

var fixtureBuiltinGroups = map[string][]fixtureGroup{
	"media-player": {
		{name: "on", members: []string{"on", "playing", "idle", "paused", "buffering"}},
		{name: "off", members: []string{"off", "standby", "unavailable"}},
	},
}

type fixtureAttr struct {
	valueType      ValueType
	changeEmission string
}

var fixtureAttributes = map[string]map[string]fixtureAttr{
	"media-player": {
		"active_app":     {valueType: String, changeEmission: "significant"},
		"active_app_id":  {valueType: String, changeEmission: "significant"},
		"app_type":       {valueType: String, changeEmission: "significant"},
		"power_mode":     {valueType: String, changeEmission: "significant"},
		"is_screensaver": {valueType: Boolean, changeEmission: "significant"},
		"app_version":    {valueType: String, changeEmission: "cosmetic"},
		"volume":         {valueType: Number, changeEmission: "significant"},
		"media_title":    {valueType: String, changeEmission: "cosmetic"},
	},
}

var fixtureCommands = map[string]map[string]bool{
	"media-player": {"launch": true, "home": true, "keypress": true, "power": true},
}

// SemanticGroups implements Registry.
func (FixtureRegistry) SemanticGroups(deviceClass, state string) []string {
	var out []string
	for _, g := range fixtureBuiltinGroups[deviceClass] {
		for _, m := range g.members {
			if m == state {
				out = append(out, g.name)
				break
			}
		}
	}
	// FixtureRegistry has no extension-registered groups; built-in groups
	// (above) are all there is (RUL-321 ordering).
	return out
}

// AttributeType implements Registry.
func (FixtureRegistry) AttributeType(deviceClass, attr string) ValueType {
	if a, ok := fixtureAttributes[deviceClass][attr]; ok {
		return a.valueType
	}
	return Unknown
}

// ChangeEmission implements Registry.
func (FixtureRegistry) ChangeEmission(deviceClass, attr string) string {
	if a, ok := fixtureAttributes[deviceClass][attr]; ok {
		return a.changeEmission
	}
	return ""
}

// CommandExists implements Registry.
func (FixtureRegistry) CommandExists(deviceClass, command string) bool {
	return fixtureCommands[deviceClass][command]
}
