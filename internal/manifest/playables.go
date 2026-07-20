package manifest

// validPlayableContentTypes is the MAN-080 contentType enum.
var validPlayableContentTypes = map[string]bool{
	"image": true, "video": true, "html_bundle": true, "stream": true, "composed": true,
}

// validDurationSemantics is the MAN-080 durationSemantics enum.
var validDurationSemantics = map[string]bool{
	"fixed": true, "source-driven": true, "operator-set": true,
}

// validatePlayable enforces MAN-080/081: when contributes.playable is
// declared, its contentType and durationSemantics are recognized enum members,
// contentId is present, and durationSeconds is present-and-positive iff
// durationSemantics is fixed (else it MUST be absent). A pack that declares no
// playable contribution is untouched — contributes.playable is optional.
func validatePlayable(m PackManifest) []Error {
	p := m.Contributes.Playable
	if p == nil {
		return nil
	}

	var errs []Error
	const at = "contributes.playable"

	if !validPlayableContentTypes[p.ContentType] {
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".contentType",
			`contributes.playable.contentType "` + p.ContentType +
				`" MUST be one of image, video, html_bundle, stream, composed (MAN-080)`})
	}

	if !validDurationSemantics[p.DurationSemantics] {
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".durationSemantics",
			`contributes.playable.durationSemantics "` + p.DurationSemantics +
				`" MUST be one of fixed, source-driven, operator-set (MAN-080)`})
	} else if p.DurationSemantics == "fixed" {
		if p.DurationSeconds == nil || *p.DurationSeconds <= 0 {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".durationSeconds",
				"contributes.playable.durationSeconds MUST be a positive number when durationSemantics is fixed (MAN-081)"})
		}
	} else if p.DurationSeconds != nil {
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".durationSeconds",
			`contributes.playable.durationSeconds MUST be absent when durationSemantics is "` +
				p.DurationSemantics + `" (MAN-081)`})
	}

	if p.ContentID == "" {
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".contentId",
			"contributes.playable.contentId is required (MAN-080)"})
	}

	return errs
}
