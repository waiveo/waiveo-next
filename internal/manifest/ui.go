package manifest

import "strconv"

// validSizeHints is the MAN-061 sizeHint enum a fragment: card page MUST carry.
var validSizeHints = map[string]bool{"small": true, "medium": true, "large": true}

// validateUI enforces MAN-060-063: every ui.pages entry has a pack-unique path,
// a pageType that is itself a compat.renderer member, and a msg-ref titleMsg;
// a fragment: card page carries a recognized sizeHint (and a page without
// fragment: card carries no sizeHint); ui.slots entries are named; and every
// ui.surfaces entry has a pack-unique name and an entry naming a file present
// in the host's bundled file set.
func validateUI(m PackManifest, host HostRegistries) []Error {
	var errs []Error

	renderer := make(map[string]bool, len(m.Compat.Renderer))
	for _, r := range m.Compat.Renderer {
		renderer[r] = true
	}

	seenPaths := map[string]bool{}
	for i, p := range m.UI.Pages {
		at := "ui.pages[" + strconv.Itoa(i) + "]"

		if p.Path == "" {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".path",
				"ui.pages[].path is required (MAN-060)"})
		} else if seenPaths[p.Path] {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".path",
				`page path "` + p.Path + `" is declared more than once (MAN-060)`})
		}
		seenPaths[p.Path] = true

		if !renderer[p.PageType] {
			errs = append(errs, Error{"UNKNOWN_PAGE_TYPE", at + ".pageType",
				`page type "` + p.PageType + `" is not a member of compat.renderer (MAN-060)`})
		}

		if !IsMsgRef(p.TitleMsg) {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".titleMsg",
				"ui.pages[].titleMsg MUST be a msg: locale-catalog reference (MAN-060)"})
		}

		switch p.Fragment {
		case "":
			if p.SizeHint != "" {
				errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".sizeHint",
					"ui.pages[].sizeHint MUST NOT be declared without fragment: card (MAN-061)"})
			}
		case "card":
			if !validSizeHints[p.SizeHint] {
				errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".sizeHint",
					`ui.pages[].sizeHint MUST be one of "small", "medium", "large" when fragment is card, got "` +
						p.SizeHint + `" (MAN-061)`})
			}
		default:
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".fragment",
				`ui.pages[].fragment MUST be "card" when declared, got "` + p.Fragment + `" (MAN-061)`})
		}
	}

	// MAN-062: a slot point MUST be named. accepts is validated for shape only
	// here — the binding-time accepts-membership check (a fragment page's
	// pageType against a target pack's slot) is a render-registration-time
	// concern this contract does not itself execute.
	for i, s := range m.UI.Slots {
		at := "ui.slots[" + strconv.Itoa(i) + "]"
		if s.Name == "" {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".name",
				"ui.slots[].name is required (MAN-062)"})
		}
	}

	// MAN-063: a surface's name MUST be a pack-unique identifier, and its entry
	// MUST name a file present in the host's bundled file set.
	seenSurfaceNames := map[string]bool{}
	for i, s := range m.UI.Surfaces {
		at := "ui.surfaces[" + strconv.Itoa(i) + "]"
		if s.Name == "" {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".name",
				"ui.surfaces[].name is required (MAN-063)"})
		} else if seenSurfaceNames[s.Name] {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".name",
				`surface name "` + s.Name + `" is declared more than once (MAN-063)`})
		}
		seenSurfaceNames[s.Name] = true

		if !host.BundleFiles[s.Entry] {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".entry",
				`ui.surfaces[].entry "` + s.Entry + `" does not name a file in the pack's bundle (MAN-063)`})
		}
	}

	return errs
}
