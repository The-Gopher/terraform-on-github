package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrAlreadyExists is what Create returns when the object exists — the 412
// precondition-failure case (§5.2). Callers treat it as the write-once cache
// hit, not as an error to report: "already planned this pair" means reuse.
var ErrAlreadyExists = errors.New("object already exists")

// ErrNotFound is what Read returns for a missing object.
var ErrNotFound = errors.New("object not found")

// Bucket is the storage surface both buckets expose. GCS gives it via
// Preconditions; the test double gives it via a map. Compare-and-swap needs
// both directions: create-if-absent (ifGenerationMatch=0) for claims and
// markers, generation-guarded overwrite for run-state transitions.
//
// The coordination bucket needs Read and List, which the plan bucket's IAM
// forbids — the interface is the union, and the IAM, not the type system, is
// what holds the split in production.
type Bucket interface {
	// Create writes name with create-if-absent semantics. Generation 0 →
	// ifGenerationMatch=0; generation N → the object must be at generation N.
	// Existing object on a 0-claim returns ErrAlreadyExists.
	Create(ctx context.Context, name string, content []byte, ifGenerationMatch int64) error

	// Read returns the object's content and current generation.
	Read(ctx context.Context, name string) (content []byte, generation int64, err error)

	// List returns object names under prefix, lexicographically sorted.
	List(ctx context.Context, prefix string) ([]string, error)

	// Delete removes name. Idempotent: deleting a missing object is a no-op.
	Delete(ctx context.Context, name string) error
}

// Signer signs the canonical meta digest with the Cloud KMS asymmetric key
// (§5.3). The plan service holds signer only; the apply service verifies
// against the public key. Sign returns base64.
type Signer interface {
	Sign(ctx context.Context, digest []byte) (string, error)
}

// Verifier checks a base64 signature against the canonical digest. Separate
// interface, because the apply side holds publicKeyViewer and never a signing
// key (§7.2).
type Verifier interface {
	Verify(ctx context.Context, digest []byte, signature string) error
}

// MapBucket is an in-memory Bucket for tests. It reproduces the two
// preconditions that matter: create-if-absent fails with ErrAlreadyExists, and
// a generation-guarded write fails when the generation has moved.
type MapBucket struct {
	objects map[string][]byte
	gens    map[string]int64
}

// NewMapBucket returns an empty MapBucket.
func NewMapBucket() *MapBucket {
	return &MapBucket{
		objects: map[string][]byte{},
		gens:    map[string]int64{},
	}
}

// Create implements Bucket.
func (b *MapBucket) Create(_ context.Context, name string, content []byte, ifGenerationMatch int64) error {
	gen, exists := b.gens[name]
	if ifGenerationMatch == 0 && exists {
		return ErrAlreadyExists
	}
	if ifGenerationMatch > 0 && (!exists || gen != ifGenerationMatch) {
		return fmt.Errorf("generation mismatch: have %d, want %d", gen, ifGenerationMatch)
	}
	b.objects[name] = bytes.Clone(content)
	b.gens[name] = gen + 1
	return nil
}

// Read implements Bucket. A missing object reads as ErrNotFound.
func (b *MapBucket) Read(_ context.Context, name string) ([]byte, int64, error) {
	content, ok := b.objects[name]
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return bytes.Clone(content), b.gens[name], nil
}

// List implements Bucket.
func (b *MapBucket) List(_ context.Context, prefix string) ([]string, error) {
	var names []string
	for name := range b.objects {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// Delete implements Bucket.
func (b *MapBucket) Delete(_ context.Context, name string) error {
	delete(b.objects, name)
	delete(b.gens, name)
	return nil
}

// FakeSigner signs with a constant prefix — enough to prove Stage signs meta
// and Verify calls through, without a KMS dependency in unit tests. The real
// verification path (crypto correctness) belongs to an integration test
// against KMS, not to the ordering tests here.
type FakeSigner struct{}

// Sign implements Signer.
func (FakeSigner) Sign(_ context.Context, digest []byte) (string, error) {
	return base64.StdEncoding.EncodeToString(digest), nil
}

// Verify implements Verifier.
func (FakeSigner) Verify(_ context.Context, digest []byte, signature string) error {
	got, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("decoding signature: %w", err)
	}
	if !bytes.Equal(got, digest) {
		return errors.New("signature does not match digest")
	}
	return nil
}
