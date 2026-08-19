package store

import (
	"context"
	"io"
	"time"
)

// PlanWriter uploads plan artifacts. Implemented against GCS and held only by the plan service.
//
// tf-plan@ has roles/storage.objectCreator on the plan bucket and nothing else. That role can
// create an object but cannot read one and cannot overwrite an existing name, which gives
// write-once semantics with no application logic: the plan service cannot swap a plan after
// publishing the check, and cannot read plans belonging to other workspaces or repos.
//
// The consequence is that cache-hit detection cannot be an existence check against *this* bucket
// — the plan service is not allowed to look. The coordination bucket is the authority on "already
// planned"; this one is append-only cold storage. Keeping them separate is what lets both hold at
// once: artifacts unreadable to their writer, coordination readable and CAS-able by both
// services. See DESIGN.md §4.2 and internal/store/index.go.
type PlanWriter interface {
	// Put writes one artifact. Fails if the object already exists (precondition
	// DoesNotExist), which turns a duplicate worker into a clean error rather than a race.
	Put(ctx context.Context, k PlanKey, artifact string, r io.Reader) error

	// PutMeta signs and writes meta.json. Last write of the sequence, so its presence means
	// the plan is complete.
	PutMeta(ctx context.Context, k PlanKey, m Meta) error
}

// PlanReader fetches plan artifacts. Held only by the apply service (roles/storage.objectViewer).
type PlanReader interface {
	// Get opens an artifact for reading.
	Get(ctx context.Context, k PlanKey, artifact string) (io.ReadCloser, error)

	// GetMeta reads meta.json and verifies its signature. It MUST NOT return an unverified
	// Meta under any circumstance — an unsigned or mis-signed plan is a hard failure that
	// pages, never something to work around by re-planning.
	GetMeta(ctx context.Context, k PlanKey) (Meta, error)
}

// SignedURLer mints short-lived read URLs for the human-readable plan text.
//
// The check summary renders resource addresses only by default, because a plan file embeds a
// state snapshot and Terraform's `sensitive` marking does not cover a secret someone pasted
// into an unmarked attribute (DESIGN.md §8). The full diff lives behind a signed URL with a
// ~15 minute TTL instead of in the PR timeline forever.
type SignedURLer interface {
	SignedURL(ctx context.Context, k PlanKey, artifact string, ttl time.Duration) (string, error)
}

// GCS implements PlanWriter, PlanReader and SignedURLer against one bucket.
//
// Bucket configuration is not optional, because a plan file is exactly as sensitive as the
// state it snapshots: CMEK, uniform bucket-level access, public access prevention, object
// versioning, a 90-day lifecycle delete, and data-read audit logging. See deploy/storage.tf.
type GCS struct {
	Bucket string
	Signer Signer // nil on the read side
}

func (g *GCS) Put(ctx context.Context, k PlanKey, artifact string, r io.Reader) error {
	// w := g.client.Bucket(g.Bucket).Object(k.Object(artifact)).
	//     If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	return errNotImplemented
}

func (g *GCS) PutMeta(ctx context.Context, k PlanKey, m Meta) error {
	// digest, _ := m.Digest(); m.Signature, _ = g.Signer.Sign(ctx, digest)
	return errNotImplemented
}

func (g *GCS) Get(ctx context.Context, k PlanKey, artifact string) (io.ReadCloser, error) {
	return nil, errNotImplemented
}

func (g *GCS) GetMeta(ctx context.Context, k PlanKey) (Meta, error) {
	return Meta{}, errNotImplemented
}

func (g *GCS) SignedURL(ctx context.Context, k PlanKey, artifact string, ttl time.Duration) (string, error) {
	return "", errNotImplemented
}

// ApplyLogWriter stores apply output. A separate bucket from plans: the apply service needs
// write access here and must not have write access there.
type ApplyLogWriter interface {
	PutApplyLog(ctx context.Context, k PlanKey, attempt int, r io.Reader) (objectName string, err error)
}
