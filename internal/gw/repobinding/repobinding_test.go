package repobinding

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

func TestVerifyValidSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	verifier := &Verifier{PublicKey: pub}

	repoID := "test-repo"
	machineID := "machine-1"
	msg := repoID + ":" + machineID
	sig := ed25519.Sign(priv, []byte(msg))
	sigHex := hex.EncodeToString(sig)

	if err := verifier.Verify(repoID, machineID, sigHex); err != nil {
		t.Errorf("expected valid signature, got error: %v", err)
	}
}

func TestVerifyInvalidSignature(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	verifier := &Verifier{PublicKey: pub}

	// Wrong key pair
	_, wrongPriv, _ := ed25519.GenerateKey(nil)
	msg := "test-repo:machine-1"
	sig := ed25519.Sign(wrongPriv, []byte(msg))
	sigHex := hex.EncodeToString(sig)

	if err := verifier.Verify("test-repo", "machine-1", sigHex); err == nil {
		t.Fatal("expected verification failure, got nil")
	}
}

func TestVerifyTamperedMessage(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	verifier := &Verifier{PublicKey: pub}

	// Sign for one message, verify with a different one
	msg := "correct-repo:machine-1"
	sig := ed25519.Sign(priv, []byte(msg))
	sigHex := hex.EncodeToString(sig)

	if err := verifier.Verify("wrong-repo", "machine-1", sigHex); err == nil {
		t.Fatal("expected verification failure for tampered message, got nil")
	}
}

func TestNewVerifierInvalidKey(t *testing.T) {
	_, err := NewVerifier("not-hex-string")
	if err == nil {
		t.Fatal("expected error for invalid hex key, got nil")
	}

	_, err = NewVerifier("deadbeef") // too short (4 bytes)
	if err == nil {
		t.Fatal("expected error for short key, got nil")
	}
}
