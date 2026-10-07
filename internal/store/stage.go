package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/the-gopher/terraform-on-github/internal/plan"
)

// Staged is what Stage reports back: the keys it wrote and whether the run was
// a fresh plan upload or a cache hit on an existing artifact (§5.2 — a 412
// means "already planned this pair", reuse it and move on).
type Staged struct {
	CacheHit bool
	RunKey   string
	PlanKey  string // prefix of the artifact objects under the plan bucket
	MetaKey  string
	Meta     Meta
}

// countChanges summarizes the plan's resource changes the way meta.json
// records them. A replace counts once under "replace" — Terraform's own
// "1 to add, 1 to destroy" line renders the two halves separately, but the
// design's counts table (§5) is by what the reviewer sees.
func countChanges(p *tfjson.Plan) map[string]int {
	counts := map[string]int{}
	if p == nil {
		return counts
	}
	for _, rc := range p.ResourceChanges {
		a := rc.Change.Actions
		switch {
		case a.Create() && a.Delete():
			counts["replace"]++
		case a.Create():
			counts["create"]++
		case a.Update():
			counts["update"]++
		case a.Delete():
			counts["delete"]++
		case a.Replace():
			counts["replace"]++
		case a.NoOp():
			// uncounted
		default:
			counts["other"]++
		}
	}
	return counts
}

// Stage uploads one workspace's plan artifacts and coordination state (§5.5).
//
// Ordering is the one thing this function exists to get right — there is no
// transaction across two objects, so the sequence is chosen so that every
// crash window fails toward the recoverable side:
//
//  1. artifact objects + signed meta.json  (plan bucket, write-once)
//  2. pending-apply/ marker                (coordination bucket)
//  3. run object flipped to "planned"      (coordination bucket, CAS)
//
// A crash after 1 and before 3 leaves "artifacts exist, run says planning":
// reconciliation re-verifies and re-publishes the check — recoverable. The
// reverse order leaves "run says planned, artifacts missing": nothing ever
// turns red for that. Kill points are asserted by TestStage_CrashWindows.
//
// Write-once reuse: every artifact Create runs with ifGenerationMatch=0. An
// ErrAlreadyExists on meta.json means this (base, head) pair was already
// planned — Stage returns the existing run as a cache hit instead of treating
// the collision as a crash (§5.2).
func Stage(
	ctx context.Context,
	artifacts, coordination Bucket,
	signer Signer,
	res plan.WorkspaceResult,
	in StageInput,
) (*Staged, error) {
	counts := countChanges(res.PlanJSON)

	// The plan bytes must be read before meta is constructed: the digest binds
	// the signature to the exact tfplan content.
	planJSON, err := json.Marshal(res.PlanJSON)
	if err != nil {
		return nil, fmt.Errorf("encoding plan.json: %w", err)
	}
	planText := []byte(res.PlanRaw)
	planBytes, err := os.ReadFile(res.PlanFile)
	if err != nil {
		return nil, fmt.Errorf("reading plan file %s: %w", res.PlanFile, err)
	}

	meta := Meta{
		Schema:               1,
		Kind:                 KindPR,
		Repo:                 in.Repo,
		PR:                   in.PR,
		Workspace:            in.Workspace,
		BaseSHA:              in.BaseSHA,
		HeadSHA:              in.HeadSHA,
		PlannedTreeSHA:       in.PlannedTreeSHA,
		TerraformVersion:     in.TerraformVersion,
		ProviderLockSHA256:   in.ProviderLockSHA256,
		PlanSHA256:           PlanSHA(planBytes),
		HasChanges:           res.HasChanges,
		ResourceChangeCounts: counts,
		PlannedAt:            in.Now.UTC().Format(time.RFC3339),
		ConfigSHA:            in.ConfigSHA,
	}

	// Step 1 — artifacts, write-once. Order inside the prefix does not matter
	// for §5.5 (the marker is the completion marker), but meta signs the plan
	// digest, so meta's bytes are computed before any write.
	artifactsPayload := map[string][]byte{
		NamePlan:     planBytes,
		NamePlanJSON: planJSON,
		NamePlanText: planText,
	}
	// Deterministic upload order keeps test assertions and crash windows stable.
	names := slices.Sorted(maps.Keys(artifactsPayload))

	collision := false
	for _, name := range names {
		if err := artifacts.Create(ctx, Key(in.Owner, in.Repo, in.Workspace, in.BaseSHA, in.HeadSHA, name), artifactsPayload[name], 0); err != nil {
			if isCollision(err) {
				// §5.2: the 412 path. Some other worker planned this exact pair.
				collision = true
				break
			}
			return nil, fmt.Errorf("uploading %s: %w", name, err)
		}
	}

	if collision {
		// Reuse: refresh the coordination state instead of failing (§5.2). The
		// run object already exists from the first pass — flip it to planned if
		// it isn't yet, and make sure the pending marker is in place.
		if err := refreshRun(ctx, coordination, in); err != nil {
			return nil, err
		}
		return &Staged{CacheHit: true, RunKey: RunKey(in.Owner, in.Repo, in.Workspace, in.BaseSHA, in.HeadSHA)}, nil
	}

	// Sign and upload meta.json last among artifacts: the signature covers the
	// plan digest, and meta's presence is what makes the artifact set complete.
	digest, err := meta.Digest()
	if err != nil {
		return nil, err
	}
	sig, err := signer.Sign(ctx, digest)
	if err != nil {
		return nil, fmt.Errorf("signing meta: %w", err)
	}
	meta.Signature = sig
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("encoding meta: %w", err)
	}
	if err := artifacts.Create(ctx, Key(in.Owner, in.Repo, in.Workspace, in.BaseSHA, in.HeadSHA, NameMeta), metaBytes, 0); err != nil {
		if isCollision(err) {
			// meta.json already there but tfplan wasn't? Impossible under
			// §5.5 ordering — treat as a cache hit for safety.
			if err := refreshRun(ctx, coordination, in); err != nil {
				return nil, err
			}
			return &Staged{CacheHit: true, RunKey: RunKey(in.Owner, in.Repo, in.Workspace, in.BaseSHA, in.HeadSHA)}, nil
		}
		return nil, fmt.Errorf("uploading meta: %w", err)
	}

	// Step 2 — pending-apply marker (before the run flip; §5.5).
	marker := PendingKey(in.Owner, in.Repo, in.PR, in.BaseSHA, in.HeadSHA)
	if err := coordination.Create(ctx, marker, []byte(RunKey(in.Owner, in.Repo, in.Workspace, in.BaseSHA, in.HeadSHA)), 0); err != nil {
		if !isCollision(err) {
			return nil, fmt.Errorf("writing pending-apply marker: %w", err)
		}
		// Marker exists from a previous partial pass — idempotent re-stage.
	}

	// Step 3 — run object → planned, via compare-and-swap.
	if err := flipRunPlanned(ctx, coordination, in); err != nil {
		return nil, err
	}

	return &Staged{
		RunKey:  RunKey(in.Owner, in.Repo, in.Workspace, in.BaseSHA, in.HeadSHA),
		PlanKey: Key(in.Owner, in.Repo, in.Workspace, in.BaseSHA, in.HeadSHA, ""),
		MetaKey: Key(in.Owner, in.Repo, in.Workspace, in.BaseSHA, in.HeadSHA, NameMeta),
		Meta:    meta,
	}, nil
}

// StageInput is everything Stage needs that the plan result doesn't carry.
type StageInput struct {
	Owner              string
	Repo               string
	PR                 int
	Workspace          string
	BaseSHA            string
	HeadSHA            string
	PlannedTreeSHA     string
	TerraformVersion   string
	ProviderLockSHA256 string
	ConfigSHA          string

	// Now is the timestamp recorded in meta.PlannedAt. Injectable for tests.
	Now time.Time
}

// runObjectState is the coordination-bucket run record (§5.4).
type runObjectState struct {
	Schema    int    `json:"schema"`
	Status    string `json:"status"` // planning | planned
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	PR        int    `json:"pr"`
	Workspace string `json:"workspace"`
	BaseSHA   string `json:"base_sha"`
	HeadSHA   string `json:"head_sha"`
	UpdatedAt string `json:"updated_at"`
}

// Status values for the run object.
const (
	RunPlanning = "planning"
	RunPlanned  = "planned"
)

// ensureRun creates the run object in "planning" state, or reads back the
// existing one. Returns the object's current generation for the later CAS flip.
func ensureRun(ctx context.Context, b Bucket, in StageInput) (int64, error) {
	key := RunKey(in.Owner, in.Repo, in.Workspace, in.BaseSHA, in.HeadSHA)
	created := runObjectState{
		Schema: 1, Status: RunPlanning,
		Owner: in.Owner, Repo: in.Repo, PR: in.PR, Workspace: in.Workspace,
		BaseSHA: in.BaseSHA, HeadSHA: in.HeadSHA,
		UpdatedAt: in.Now.UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(created)
	if err != nil {
		return 0, err
	}
	if err := b.Create(ctx, key, raw, 0); err != nil {
		if isCollision(err) {
			_, gen, rerr := b.Read(ctx, key)
			if rerr != nil {
				return 0, rerr
			}
			return gen, nil
		}
		return 0, fmt.Errorf("claiming run object: %w", err)
	}
	return 1, nil
}

// refreshRun handles the §5.2 cache hit: ensure the pending marker and the
// planned flip exist without touching write-once artifacts again.
func refreshRun(ctx context.Context, coordination Bucket, in StageInput) error {
	marker := PendingKey(in.Owner, in.Repo, in.PR, in.BaseSHA, in.HeadSHA)
	if err := coordination.Create(ctx, marker, []byte(RunKey(in.Owner, in.Repo, in.Workspace, in.BaseSHA, in.HeadSHA)), 0); err != nil && !isCollision(err) {
		return fmt.Errorf("writing pending-apply marker: %w", err)
	}
	return flipRunPlanned(ctx, coordination, in)
}

// flipRunPlanned transitions the run object to "planned" by CAS on generation.
func flipRunPlanned(ctx context.Context, b Bucket, in StageInput) error {
	key := RunKey(in.Owner, in.Repo, in.Workspace, in.BaseSHA, in.HeadSHA)
	if _, err := ensureRun(ctx, b, in); err != nil {
		return err
	}

	raw, gen, rerr := b.Read(ctx, key)
	if rerr != nil {
		return rerr
	}
	var state runObjectState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("decoding run object: %w", err)
	}
	if state.Status == RunPlanned {
		return nil // idempotent
	}
	state.Status = RunPlanned
	state.UpdatedAt = in.Now.UTC().Format(time.RFC3339)
	updated, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := b.Create(ctx, key, updated, gen); err != nil {
		return fmt.Errorf("flipping run to planned: %w", err)
	}
	return nil
}

// isCollision reports whether err is the write-once 412 (§5.2).
func isCollision(err error) bool {
	return errors.Is(err, ErrAlreadyExists)
}
