package playerserver

import (
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
)

// Three guards on this package's least-travelled paths, found by mutation: each
// could be deleted and the whole suite stayed green.
//
// They share a shape worth naming. None of the three is on the path a working
// pairing takes, so every test that pairs successfully exercises none of them —
// and each one, disabled, fails in a way that reads on the wire as an ordinary
// refusal rather than as the fault it is.

// TestNewServerRefusesACertificateItCannotDecode is the construction guard.
//
// A relay's own enrolled identity is read from its certificate's subject, and
// REL-121b refuses every grant bound to a different relay. So a server built
// with an unusable certificate has an empty relayID and refuses every bound
// grant — which on the wire is indistinguishable from an invalid pairing code,
// for every screen at the site, until someone reads the relay's own logs.
//
// Refusing at construction is what turns that into a boot failure an operator
// sees immediately.
func TestNewServerRefusesACertificateItCannotDecode(t *testing.T) {
	for _, tc := range []struct {
		name string
		pem  []byte
	}{
		{"empty", nil},
		{"not PEM at all", []byte("this is not a certificate")},
		{"a PEM block of the wrong type", []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")},
		{"a CERTIFICATE block whose body is not a certificate", []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewServer(tc.pem, nil, WallClockMs); err == nil {
				t.Errorf("NewServer accepted %s — the relay's own identity comes from this certificate, and a server "+
					"that ran without one would refuse every relay-bound grant while answering exactly as it would "+
					"for a bad pairing code", tc.name)
			}
		})
	}

	// The control: a real certificate still constructs. Without it, a NewServer
	// that refused everything would satisfy every case above.
	certPEM, _ := testRelayCert(t)
	if _, err := NewServer(certPEM, nil, WallClockMs); err != nil {
		t.Fatalf("NewServer refused a valid certificate: %v", err)
	}
}

// TestAnUnknownTokenDoesNotResolveFromTheDurableStore.
//
// The durable lookup returns (record, found, err), and the guard is what turns a
// miss into "no session". Deleted, a token nobody ever minted resolves to the
// ZERO record — screen id "", expiry 0, not terminated — so an unknown bearer
// token becomes a session for a screen whose id is the empty string.
//
// Nothing else on the path catches it: the empty screen id is not compared
// against anything that would reject it, and an expiry of 0 is in the past only
// if something checks it against the clock.
func TestAnUnknownTokenDoesNotResolveFromTheDurableStore(t *testing.T) {
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	srv, _, _ := newTestServer(t, testGrant())
	srv.EnablePersistence(store)

	for _, token := range []string{
		"never-minted-by-anyone",
		"",
		// Deliberately NOT shaped like a credential. The first version of this
		// case used a plausible-looking prefixed hex token, and GitHub's push
		// protection refused the commit: it matched a vendor's API-key pattern.
		// The scanner was right to be conservative, and the case loses nothing —
		// the lookup hashes whatever it is handed, so the SHAPE of an unknown
		// token has never been what it is testing.
		"a token this relay has never issued",
	} {
		if screenID, _, ok := srv.LookupChannelToken(token); ok {
			t.Errorf("token %q resolved to a session for screen %q — an unknown token that resolves is a bearer "+
				"credential the relay never issued, carrying whatever screen id the zero value has", token, screenID)
		}
	}

	// The control: a token this relay DID mint resolves. Without it, a lookup
	// that refused everything would satisfy the assertions above while breaking
	// every screen's program poll.
	resp, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-unknown-token",
		GrantSelector: testGrant().GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("pairing to mint a real token: status = %d, body = %v", resp.StatusCode, raw)
	}
	var pr PairingResponse
	remarshal(t, raw, &pr)
	if _, _, ok := srv.LookupChannelToken(pr.ChannelToken); !ok {
		t.Error("a token this relay just minted did not resolve")
	}
}

// TestAnEmptySelectorRedeemsNothingEvenIfAGrantIsKeyedByOne.
//
// Grants are indexed by their own `grant_id`, so a snapshot carrying a grant
// with an EMPTY id keys the index at "". Without the early guard, an empty
// pairing code then matches it, and the empty string becomes a working
// credential for whatever screen that grant names.
//
// The guard looked equivalent when the mutation survived — an empty key would
// not normally resolve — and it is not: the malformed grant is exactly the input
// that makes it matter, and no test supplied one.
func TestAnEmptySelectorRedeemsNothingEvenIfAGrantIsKeyedByOne(t *testing.T) {
	malformed := testGrant()
	malformed.GrantID = "" // what a snapshot with a missing grant_id produces
	srv, _, _ := newTestServer(t, malformed)

	if _, err := srv.redeem(""); err == nil {
		t.Fatal("an EMPTY pairing code redeemed a grant — a grant whose own id is missing must not make the empty " +
			"string a working credential for the screen it names")
	}

	// And the ordinary miss still refuses, so the guard above is not the only
	// thing standing between a wrong code and a token.
	if _, err := srv.redeem("no-such-grant"); err == nil {
		t.Error("an unknown selector redeemed something")
	}
}
