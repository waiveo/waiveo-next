package identity

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

// Four guards found by mutation: each could be deleted with the whole TREE
// green, not merely this package's own suite.
//
// Two are "this row does not exist" branches and two are key-decoding refusals.
// They share the property that makes them easy to lose: the path a working relay
// takes never reaches any of them, so every test that pairs, polls or restarts
// successfully exercises none.

// TestPlayerSessionReportsAnUnknownTokenAsNotFound.
//
// Without the ErrNoRows branch, an unknown token hash falls to the generic error
// return — so "no such session" becomes "the store failed". The caller
// (playerserver.lookupSession) treats an error and a miss identically today, so
// nothing breaks visibly; what breaks is the distinction, and it is the one a
// relay needs to tell "this token was never minted" from "the database is
// unreadable". One is an ordinary refusal, the other is an outage.
func TestPlayerSessionReportsAnUnknownTokenAsNotFound(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	rec, found, err := s.PlayerSession(HashToken("a token nothing ever minted"))
	if err != nil {
		t.Fatalf("an unknown token reported an ERROR (%v) — a miss and a failure are different facts, and a relay "+
			"that cannot tell them apart cannot tell a bad credential from a broken store", err)
	}
	if found {
		t.Errorf("an unknown token reported found, carrying %+v", rec)
	}

	// The control: a session that IS stored resolves. Without it, a lookup that
	// reported not-found for everything would satisfy the assertion above.
	const screenID = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	hash := HashToken("a token this relay really minted")
	if err := s.SetPlayerSession(hash, screenID, 1_800_000_000_000); err != nil {
		t.Fatalf("SetPlayerSession: %v", err)
	}
	rec, found, err = s.PlayerSession(hash)
	if err != nil || !found {
		t.Fatalf("a stored session did not resolve: found=%v err=%v", found, err)
	}
	if rec.ScreenID != screenID {
		t.Errorf("screen_id = %q, want %q", rec.ScreenID, screenID)
	}
}

// TestPairingGrantRedeemedReportsAnUnredeemedGrantAsFalse.
//
// This one is the sharper of the two. Without the ErrNoRows branch, a grant that
// has NEVER been redeemed answers with an error instead of false — and the
// redemption path calls this to decide whether a one-time grant is already
// spent. An error there refuses the redemption, so every FRESH grant becomes
// unredeemable: the failure mode is not a stale grant slipping through, it is no
// screen at the site being able to pair at all.
func TestPairingGrantRedeemedReportsAnUnredeemedGrantAsFalse(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	redeemed, err := s.PairingGrantRedeemed("grant-never-redeemed-0000")
	if err != nil {
		t.Fatalf("an unredeemed grant reported an ERROR (%v) — this is the check a one-time redemption asks before "+
			"minting, so an error here refuses every fresh grant", err)
	}
	if redeemed {
		t.Error("a grant nothing has redeemed reported as redeemed")
	}

	// The control: a grant that WAS redeemed reports true, so the branch above
	// is not simply answering false to everything — which would be worse than
	// the error, since it makes a one-time grant redeemable forever.
	if err := s.MarkPairingGrantRedeemed("grant-spent-0000", 1_700_000_000_000); err != nil {
		t.Fatalf("MarkPairingGrantRedeemed: %v", err)
	}
	redeemed, err = s.PairingGrantRedeemed("grant-spent-0000")
	if err != nil {
		t.Fatalf("PairingGrantRedeemed: %v", err)
	}
	if !redeemed {
		t.Error("a redeemed grant reported as not redeemed — a one-time grant would be redeemable a second time")
	}
}

// TestUnmarshalPrivateKeyRefusesWhatIsNotAnEd25519PrivateKey covers both
// decoding guards.
//
// The relay's private key signs its own identity material. A malformed PEM
// reaching x509.ParsePKCS8PrivateKey with a nil block panics; a well-formed
// PKCS8 key of the WRONG algorithm passes the parse and fails the type
// assertion — and without that assertion the function returns a nil
// ed25519.PrivateKey as though it were valid, which signs nothing and fails at
// whatever later moment first needs a signature.
func TestUnmarshalPrivateKeyRefusesWhatIsNotAnEd25519PrivateKey(t *testing.T) {
	// A real PKCS8 key of the wrong algorithm: parses cleanly, is not ed25519.
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate an ecdsa key: %v", err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatalf("marshal the ecdsa key: %v", err)
	}
	ecPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})

	for _, tc := range []struct {
		name string
		pem  []byte
	}{
		{"empty", nil},
		{"not PEM at all", []byte("this is not a key")},
		{"a PEM block of the wrong type", []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")},
		{"a PRIVATE KEY block whose body is not a key", []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")},
		{"a well-formed PKCS8 key of the wrong algorithm", ecPEM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, err := unmarshalPrivateKey(tc.pem)
			if err == nil {
				t.Errorf("unmarshalPrivateKey accepted %s and returned a %d-byte key — a nil or wrong-algorithm key "+
					"returned as valid signs nothing, and fails at whatever later moment first needs a signature "+
					"rather than here", tc.name, len(key))
			}
		})
	}

	// The control: a real ed25519 key round-trips. Without it, a function that
	// refused everything would satisfy every case above while making the relay
	// unable to load its own identity.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate an ed25519 key: %v", err)
	}
	pemBytes, err := marshalPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshalPrivateKey: %v", err)
	}
	got, err := unmarshalPrivateKey(pemBytes)
	if err != nil {
		t.Fatalf("unmarshalPrivateKey refused a real ed25519 key: %v", err)
	}
	if !got.Equal(priv) {
		t.Error("the round-tripped key is not the one that was marshaled")
	}
}
