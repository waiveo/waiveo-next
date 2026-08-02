package packsig

import (
	"strings"
	"testing"
)

// statementfields_test.go pins fieldsSane, the check that keeps the
// domain-separated signing statement unambiguous.
//
// The statement is LINE-ORIENTED — context, artifact_id, kind, subtype,
// version, content_digest, each on its own line — so a field containing a line
// break moves every boundary after it. fieldsSane refuses that before any sign
// or verify, and the whole line structure rests on it.
//
// A mutation sweep found it pinned by nothing: each of its rules could be
// deleted with the package green. That is worth more than a coverage number,
// because the failure it prevents is not "a malformed envelope is accepted" but
// "one artifact's signature verifies a different artifact" — see the collision
// test below, which is the reason this file exists.

// baseEnvelope is a well-formed envelope every case mutates one field of.
func baseEnvelope() Envelope {
	return Envelope{
		Format:        Format,
		ArtifactID:    "acme/menu-board",
		Kind:          KindPack,
		Version:       "1.0.0",
		ContentDigest: digestPrefix + "0000000000000000000000000000000000000000000000000000000000000001",
		KeyID:         "abcdef0123456789",
	}
}

// TestStatementFieldsRejectALineBreak is the security case, and the collision
// below is what makes it one.
//
// Without this rule, an envelope whose artifact_id carries a newline renders a
// statement byte-identical to a DIFFERENT envelope's — so a signature issued
// over the second verifies the first. The test asserts the collision exists
// rather than merely that the input is refused, because "refused" would also be
// true of a rule that rejected the wrong thing.
func TestStatementFieldsRejectALineBreak(t *testing.T) {
	// Two envelopes that differ in artifact_id and kind, yet whose statements
	// collide if line breaks are allowed through: the newline in the first one's
	// artifact_id supplies the line boundary the second gets from its own fields.
	forged := baseEnvelope()
	forged.ArtifactID = "attacker/pack\npack"
	forged.Kind = KindPack

	honest := baseEnvelope()
	honest.ArtifactID = "attacker/pack"
	honest.Kind = KindPack

	forgedStmt := statement(forged.ArtifactID, forged.Kind, forged.Subtype, forged.Version, forged.ContentDigest)
	honestStmt := statement(honest.ArtifactID, honest.Kind, honest.Subtype, honest.Version, honest.ContentDigest)
	if string(forgedStmt) == string(honestStmt) {
		// If they collide, the guard is the only thing standing between them.
		if err := fieldsSane(forged); err == nil {
			t.Fatal("an artifact_id containing a line break was accepted, and its statement is byte-identical to a " +
				"different envelope's — a signature over one would verify the other")
		}
	}

	// Every field that reaches the statement must refuse a break, in both forms.
	for _, tc := range []struct {
		field string
		set   func(*Envelope, string)
	}{
		{"artifact_id", func(e *Envelope, v string) { e.ArtifactID = "acme/" + v }},
		{"kind", func(e *Envelope, v string) { e.Kind = v }},
		{"subtype", func(e *Envelope, v string) { e.Subtype = v }},
		{"version", func(e *Envelope, v string) { e.Version = v }},
		{"content_digest", func(e *Envelope, v string) { e.ContentDigest = digestPrefix + v }},
	} {
		for _, brk := range []string{"a\nb", "a\rb"} {
			e := baseEnvelope()
			tc.set(&e, brk)
			err := fieldsSane(e)
			if err == nil {
				t.Errorf("%s containing %q was accepted — the statement's line structure is no longer unambiguous",
					tc.field, brk)
				continue
			}
			if !strings.Contains(err.Error(), "line break") {
				t.Errorf("%s containing %q was refused with %q, want the line-break rule — a refusal for another "+
					"reason leaves this one unheld", tc.field, brk, err)
			}
		}
	}

	// The control: the well-formed envelope passes, so none of the above is a
	// rule that refuses everything.
	if err := fieldsSane(baseEnvelope()); err != nil {
		t.Errorf("a well-formed envelope was refused: %v", err)
	}
}

// TestStatementFieldsRejectEmptyAndMalformed covers fieldsSane's other rules.
// Each asserts the SPECIFIC reason: they share a return type and no code, so a
// test satisfied by any error passes when a different rule fires.
func TestStatementFieldsRejectEmptyAndMalformed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Envelope)
		wantMsg string
	}{
		{"empty artifact_id", func(e *Envelope) { e.ArtifactID = "" }, "artifact_id is empty"},
		{"empty kind", func(e *Envelope) { e.Kind = "" }, "kind is empty"},
		{"empty version", func(e *Envelope) { e.Version = "" }, "version is empty"},
		{"empty content_digest", func(e *Envelope) { e.ContentDigest = "" }, "content_digest is empty"},
		{"empty key_id", func(e *Envelope) { e.KeyID = "" }, "key_id is empty"},

		// The digest prefix is what ties the signed value to the algorithm that
		// produced it; without it a hex string of any provenance would do.
		{"content_digest without its prefix", func(e *Envelope) {
			e.ContentDigest = strings.TrimPrefix(e.ContentDigest, digestPrefix)
		}, "prefix"},

		// The namespace split is what the trust anchors are keyed on, so an id
		// that does not split cannot be authorized against any publisher.
		{"artifact_id with no namespace", func(e *Envelope) { e.ArtifactID = "menu-board" }, "<publisher>/<name>"},
		{"artifact_id with an empty publisher", func(e *Envelope) { e.ArtifactID = "/menu-board" }, "<publisher>/<name>"},
		{"artifact_id with an empty name", func(e *Envelope) { e.ArtifactID = "acme/" }, "<publisher>/<name>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := baseEnvelope()
			tc.mutate(&e)
			err := fieldsSane(e)
			if err == nil {
				t.Fatalf("accepted an envelope with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("refused with %q, want one naming %q", err, tc.wantMsg)
			}
		})
	}
}
