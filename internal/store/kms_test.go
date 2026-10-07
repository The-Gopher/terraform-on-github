package store

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"
)

// pemOf encodes pub the way KMS GetPublicKey returns it.
func pemOf(t *testing.T, pub crypto.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func verifierFor(t *testing.T, pub crypto.PublicKey, fetches *int) *KMSVerifier {
	t.Helper()
	p := pemOf(t, pub)
	return &KMSVerifier{fetchPEM: func(context.Context) (string, error) {
		*fetches++
		return p, nil
	}}
}

func TestKMSVerifier_ECDSA(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := []byte(`{"kind":"pr"}`)
	sum := sha256.Sum256(digest)
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	// KMS's P1363 encoding: r||s, each left-padded to the coordinate size.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	fetches := 0
	v := verifierFor(t, &key.PublicKey, &fetches)
	ctx := context.Background()
	if err := v.Verify(ctx, digest, base64.StdEncoding.EncodeToString(sig)); err != nil {
		t.Fatalf("valid signature: %v", err)
	}
	if err := v.Verify(ctx, []byte(`{"kind":"drift"}`), base64.StdEncoding.EncodeToString(sig)); err == nil {
		t.Error("signature over a different digest must not verify")
	}
	if err := v.Verify(ctx, digest, base64.StdEncoding.EncodeToString(sig[:63])); err == nil {
		t.Error("a truncated signature must not verify")
	}
	if fetches != 1 {
		t.Errorf("public key fetched %d times, want 1 (cached)", fetches)
	}
}

func TestKMSVerifier_RSA(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	digest := []byte(`{"kind":"pr"}`)
	sum := sha256.Sum256(digest)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}

	fetches := 0
	v := verifierFor(t, &key.PublicKey, &fetches)
	if err := v.Verify(context.Background(), digest, base64.StdEncoding.EncodeToString(sig)); err != nil {
		t.Fatalf("valid signature: %v", err)
	}
	sig[0] ^= 0xff
	if err := v.Verify(context.Background(), digest, base64.StdEncoding.EncodeToString(sig)); err == nil {
		t.Error("a corrupted signature must not verify")
	}
}

func TestKMSVerifier_BadPEM(t *testing.T) {
	v := &KMSVerifier{fetchPEM: func(context.Context) (string, error) { return "not pem", nil }}
	if err := v.Verify(context.Background(), []byte("d"), base64.StdEncoding.EncodeToString([]byte("s"))); err == nil {
		t.Error("an unparseable public key must fail verification")
	}
}
