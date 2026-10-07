package store

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
)

// KMSSigner signs the meta digest with a Cloud KMS asymmetric key. The plan
// service holds cloudkms.signer on this key and nothing more (§7.1); the apply
// side verifies via publicKeyViewer.
type KMSSigner struct {
	client  *kms.KeyManagementClient
	keyName string // projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/v
}

// NewKMSSigner wraps a KMS client and fully-qualified key version.
func NewKMSSigner(client *kms.KeyManagementClient, keyName string) *KMSSigner {
	return &KMSSigner{client: client, keyName: keyName}
}

// Sign implements Signer. KMS wants a pre-hashed SHA-256 digest, which is
// exactly what Meta.Digest plus one hash round gives; the key version's
// algorithm selects SHA-256 by key configuration, not by request field.
func (s *KMSSigner) Sign(ctx context.Context, digest []byte) (string, error) {
	sum := sha256.Sum256(digest)
	resp, err := s.client.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{
		Name: s.keyName,
		Digest: &kmspb.Digest{
			Digest: &kmspb.Digest_Sha256{Sha256: sum[:]},
		},
	})
	if err != nil {
		return "", fmt.Errorf("kms sign: %w", err)
	}
	return base64.StdEncoding.EncodeToString(resp.Signature), nil
}

// KMSVerifier verifies KMSSigner signatures locally: it fetches the key
// version's public key (publicKeyViewer — the only KMS grant the apply side
// holds, §7.2) and verifies the ECDSA/RSA signature in-process. There is no
// AsymmetricVerify RPC; KMS signs, the client verifies against the fetched PEM.
type KMSVerifier struct {
	// fetchPEM returns the key version's PEM-encoded public key.
	fetchPEM func(ctx context.Context) (string, error)

	mu  sync.Mutex
	key crypto.PublicKey // cached: a key version's public key never changes
}

// NewKMSVerifier returns a Verifier for the fully-qualified key version keyName.
func NewKMSVerifier(client *kms.KeyManagementClient, keyName string) *KMSVerifier {
	return &KMSVerifier{fetchPEM: func(ctx context.Context) (string, error) {
		pub, err := client.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: keyName})
		if err != nil {
			return "", fmt.Errorf("kms get public key: %w", err)
		}
		return pub.GetPem(), nil
	}}
}

// Verify implements Verifier: it checks a base64 signature over the SHA-256 of digest.
func (v *KMSVerifier) Verify(ctx context.Context, digest []byte, signature string) error {
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("decoding signature: %w", err)
	}
	pub, err := v.publicKey(ctx)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(digest)
	switch key := pub.(type) {
	case *ecdsa.PublicKey:
		// KMS encodes EC signatures as the IEEE P1363 r||s concatenation, each
		// half the coordinate size — not ASN.1.
		size := (key.Params().N.BitLen() + 7) / 8
		if len(sig) != 2*size {
			return fmt.Errorf("ecdsa signature is %d bytes, want %d (r||s)", len(sig), 2*size)
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(key, sum[:], r, s) {
			return errors.New("ecdsa signature does not verify")
		}
		return nil
	case *rsa.PublicKey:
		if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
			return fmt.Errorf("rsa signature does not verify: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported kms key type %T", pub)
	}
}

// publicKey fetches and parses the public key once, then serves it from the cache. A failed
// fetch is not cached, so a transient KMS error is retried on the next Verify.
func (v *KMSVerifier) publicKey(ctx context.Context) (crypto.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.key != nil {
		return v.key, nil
	}
	pemText, err := v.fetchPEM(ctx)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("kms public key is not valid PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing kms public key: %w", err)
	}
	v.key = key
	return key, nil
}
