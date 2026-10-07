package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/storage"

	"github.com/the-gopher/terraform-on-github/internal/apply"
	"github.com/the-gopher/terraform-on-github/internal/store"
)

// runApply implements M5: `tfgh apply verify --repo … --pr … --workspace …`.
// It runs the §6 verification gauntlet against the staged artifact and prints
// a verdict plus a would-apply summary. It applies nothing and holds no
// writer identity — the binary mirrors the service boundary (§7): the
// operator runs terraform apply themselves, with credentials this tool never
// held.
func runApply(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var f sharedFlags
	fs := newFlagSet("apply", stderr)
	f.registerRepo(fs)
	f.registerPR(fs)
	f.registerConfigRef(fs)
	workspace := fs.String("workspace", "", "Workspace name")
	plansBucket := fs.String("plans-bucket", "", "Plan-artifact bucket")
	kmsKey := fs.String("kms-key", "", "KMS cryptoKeyVersion for verifying meta.json (public-key access only)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	owner, repo, err := f.ownerRepo()
	if err != nil {
		return err
	}
	if err := f.requirePR(); err != nil {
		return err
	}
	if *workspace == "" {
		return usageError("--workspace is required")
	}
	if *plansBucket == "" || *kmsKey == "" {
		return usageError("apply verify requires --plans-bucket and --kms-key")
	}

	client, err := newGitHubClient(ctx)
	if err != nil {
		return err
	}

	// The base/head SHAs key the artifact (§5); resolve them from GitHub now,
	// exactly as the plan side did — never from a stored pointer (§6.1).
	pull, err := client.GetApplyPullRequest(ctx, owner, repo, f.pr)
	if err != nil {
		return fmt.Errorf("reading PR %d: %w", f.pr, err)
	}
	if pull.HeadSHA == "" {
		return fmt.Errorf("PR %d has no head SHA", f.pr)
	}
	// The artifact key composes from the base SHA the plan side scoped against
	// — the tip of the base ref at plan time. Re-derive it the same way rather
	// than trusting any pointer (§6.1).
	baseSHA, err := client.ResolveRef(ctx, owner, repo, pull.BaseRef)
	if err != nil {
		return fmt.Errorf("resolving base ref: %w", err)
	}

	gcs, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("gcs client: %w", err)
	}
	defer func() { _ = gcs.Close() }()

	kmsClient, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return fmt.Errorf("kms client: %w", err)
	}
	defer func() { _ = kmsClient.Close() }()

	v := &apply.Verifier{
		GitHub:    client,
		Artifacts: store.NewGCSBucket(gcs.Bucket(*plansBucket)),
		KMS:       store.NewKMSVerifier(kmsClient, *kmsKey),
		Trusted:   client,
		ConfigRef: f.configRef,
	}

	verdict, err := v.Verify(ctx, apply.Request{
		Owner:     owner,
		Repo:      repo,
		PR:        f.pr,
		Workspace: *workspace,
		BaseSHA:   baseSHA,
		HeadSHA:   pull.HeadSHA,
	})
	if err != nil {
		var rejected *apply.RejectedError
		if errors.As(err, &rejected) {
			return fmt.Errorf("NOT APPLICABLE: %w", err)
		}
		return fmt.Errorf("verifying: %w", err)
	}

	fmt.Fprintln(stdout, apply.WouldApply(verdict, *workspace, 0))
	return nil
}
