package store

import (
	"context"
	"errors"
)

// ErrBadSignature means a plan's provenance could not be verified. Always terminal: never
// respond by re-planning, because a plan that fails verification is either corrupt or hostile
// and neither case is improved by generating a fresh unreviewed plan.
var ErrBadSignature = errors.New("store: plan signature verification failed")

// Signer signs plan provenance. Only the plan service holds it (roles/cloudkms.signer on the
// plan-signing key).
type Signer interface {
	Sign(ctx context.Context, digest []byte) ([]byte, error)
}

// Verifier checks plan provenance. Only the apply service holds it, and only with
// roles/cloudkms.publicKeyViewer — it can verify and cannot sign.
type Verifier interface {
	Verify(ctx context.Context, digest, signature []byte) error
}

// KMSSigner uses a Cloud KMS asymmetric signing key (EC_SIGN_P256_SHA256).
//
// Asymmetric rather than HMAC so the two capabilities are genuinely different permissions. A
// shared HMAC secret would mean the apply service holds material sufficient to forge the
// provenance it is supposed to be checking, which defeats the purpose.
//
// The bucket IAM in §5.2 already prevents the plan service from swapping a published plan.
// This signature is the layer that still holds when someone widens a bucket binding by
// accident — the failure mode IAM review is worst at catching.
type KMSSigner struct {
	// KeyVersion e.g. projects/acme-tf/locations/us/keyRings/tf/cryptoKeys/plan-signing/cryptoKeyVersions/1
	KeyVersion string
}

func (s *KMSSigner) Sign(ctx context.Context, digest []byte) ([]byte, error) {
	// AsymmetricSign{Name: s.KeyVersion, Digest: &kmspb.Digest_Sha256{Sha256: digest}}
	return nil, errNotImplemented
}

// KMSVerifier verifies locally against the cached public key, so verification costs no KMS
// call and cannot be made to fail open by a KMS outage.
type KMSVerifier struct {
	KeyVersion string
	// pub is fetched once via GetPublicKey and cached for the instance lifetime.
}

func (v *KMSVerifier) Verify(ctx context.Context, digest, signature []byte) error {
	// ecdsa.VerifyASN1(v.pub, digest, signature) → ErrBadSignature on mismatch
	return errNotImplemented
}
