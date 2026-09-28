package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GCSBucket adapts a *storage.Client handle to Bucket. Preconditions map
// directly: ifGenerationMatch=0 is create-if-absent (the claim), a concrete
// generation is compare-and-swap (§5.4).
type GCSBucket struct {
	handle *storage.BucketHandle
}

// NewGCSBucket wraps a bucket handle.
func NewGCSBucket(h *storage.BucketHandle) *GCSBucket {
	return &GCSBucket{handle: h}
}

// Create implements Bucket. ifGenerationMatch=0 → DoesNotExist; N → N.
func (b *GCSBucket) Create(ctx context.Context, name string, content []byte, ifGenerationMatch int64) error {
	var preconds storage.Conditions
	if ifGenerationMatch == 0 {
		preconds.DoesNotExist = true
	} else {
		preconds.GenerationMatch = ifGenerationMatch
	}

	w := b.handle.Object(name).If(preconds).NewWriter(ctx)
	if _, err := w.Write(content); err != nil {
		_ = w.Close()
		return translate(err, name)
	}
	if err := w.Close(); err != nil {
		return translate(err, name)
	}
	return nil
}

// Read implements Bucket.
func (b *GCSBucket) Read(ctx context.Context, name string) ([]byte, int64, error) {
	attrs, err := b.handle.Object(name).Attrs(ctx)
	if err != nil {
		return nil, 0, translate(err, name)
	}
	r, err := b.handle.Object(name).NewReader(ctx)
	if err != nil {
		return nil, 0, translate(err, name)
	}
	defer func() { _ = r.Close() }()
	content, err := readAll(r)
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s: %w", name, err)
	}
	return content, attrs.Generation, nil
}

// List implements Bucket. GCS listing is lexicographically ordered and
// strongly consistent — the property §5.5's marker-queue depends on.
func (b *GCSBucket) List(ctx context.Context, prefix string) ([]string, error) {
	var names []string
	it := b.handle.Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", prefix, err)
		}
		names = append(names, attrs.Name)
	}
	return names, nil
}

// Delete implements Bucket.
func (b *GCSBucket) Delete(ctx context.Context, name string) error {
	err := b.handle.Object(name).Delete(ctx)
	if err != nil && !errors.Is(translate(err, name), ErrNotFound) {
		return err
	}
	return nil
}

// translate maps GCS errors onto the package's sentinels. FailedPrecondition
// on a DoesNotExist write is the write-once 412 (§5.2); NotFound is
// ErrNotFound.
func translate(err error, name string) error {
	switch {
	case errors.Is(err, storage.ErrObjectNotExist):
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	case status.Code(err) == codes.FailedPrecondition:
		return fmt.Errorf("%w: %s", ErrAlreadyExists, name)
	case status.Code(err) == codes.NotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	default:
		return fmt.Errorf("%s: %w", name, err)
	}
}

func readAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

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
