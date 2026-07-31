package mcp

import "testing"

// TestTheSurfaceIsExactlyTheTaggedOperations is API-070/071 as a test: the tags
// are the sole input, so an operation carrying neither must not appear and every
// one carrying exactly one must.
//
// It is checked against the document itself rather than a list written here — a
// list would be the "separate allowlist" API-071 says does not exist, and it would
// go stale the first time an operation was curated.
func TestTheSurfaceIsExactlyTheTaggedOperations(t *testing.T) {
	tools, err := Tools()
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("no tools derived: the document has curated operations, so an empty surface means nothing was read")
	}

	byName := map[string]Tool{}
	for _, tool := range tools {
		if tool.Name == "" {
			t.Errorf("a tool has no name (operationId): %+v", tool)
		}
		if _, dup := byName[tool.Name]; dup {
			t.Errorf("two tools named %q", tool.Name)
		}
		byName[tool.Name] = tool
	}

	// Spot the two ends of the curation: a read that must be there, an act that
	// must be there, and an operation that carries neither tag and must not be.
	for name, wantMutating := range map[string]bool{
		"listScopeNodes":  false,
		"createScopeNode": true,
	} {
		tool, present := byName[name]
		if !present {
			t.Errorf("%s is curated in the document but absent from the surface", name)
			continue
		}
		if tool.Mutating != wantMutating {
			t.Errorf("%s: mutating = %v, want %v", name, tool.Mutating, wantMutating)
		}
	}
	// Operations the document declares and does NOT curate. API-070: "An operation
	// carrying neither tag MUST NOT be exposed as an MCP tool."
	//
	// Named from the document rather than invented: `login` and `logout` are
	// credential-exchange operations and `enrollTotp` is a second-factor flow —
	// none is a tool a model should be able to call, and all three are real
	// operationIds here. An earlier version of this test asserted on a name the
	// document does not declare at all, which made it pass whatever the code did.
	for _, uncurated := range []string{"login", "logout", "enrollTotp", "redeemCredentialReset"} {
		if _, present := byName[uncurated]; present {
			t.Errorf("%s carries neither curation tag but was exposed as a tool (API-070)", uncurated)
		}
	}
}

// TestEveryMutatingPostReportsItsIdempotencyKey mirrors API-072 from the consumer
// side: an MCP client's retry-on-timeout must be able to carry a key, so the tool
// has to say the call accepts one. A caller that had to know the rule itself would
// be a second place the rule lives.
func TestEveryMutatingPostReportsItsIdempotencyKey(t *testing.T) {
	tools, err := Tools()
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	posts := 0
	for _, tool := range tools {
		if !tool.Mutating || tool.Method != "POST" {
			// A non-POST act operation must NOT claim to need one: API-072 is scoped
			// to POST because PATCH and DELETE are addressed by If-Match instead.
			if tool.RequiresIdempotencyKey {
				t.Errorf("%s (%s) claims an Idempotency-Key requirement outside API-072's scope", tool.Name, tool.Method)
			}
			continue
		}
		posts++
		if !tool.RequiresIdempotencyKey {
			t.Errorf("%s is a mutating POST that does not report accepting Idempotency-Key (API-072)", tool.Name)
		}
	}
	if posts == 0 {
		t.Fatal("no mutating POST tools found: this check would pass vacuously")
	}
}

// TestOrderIsStable: two derivations of the same document must produce the same
// surface. Go map iteration is randomised, so without the sort this passes and
// fails at random — and anything recording the surface would show a diff that is
// only iteration order.
func TestOrderIsStable(t *testing.T) {
	first, err := Tools()
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	for range 5 {
		again, err := Tools()
		if err != nil {
			t.Fatalf("Tools: %v", err)
		}
		for i := range first {
			if first[i].Name != again[i].Name {
				t.Fatalf("order differs between derivations at %d: %q vs %q", i, first[i].Name, again[i].Name)
			}
		}
	}
}

// TestEveryToolCanTellAModelWhatItDoes: a tool with no description is one a model
// will guess about, and guessing about a mutating call is the expensive case.
func TestEveryToolCanTellAModelWhatItDoes(t *testing.T) {
	tools, err := Tools()
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	for _, tool := range tools {
		if tool.Description == "" {
			t.Errorf("%s (%s %s) has neither description nor summary", tool.Name, tool.Method, tool.Path)
		}
	}
}
