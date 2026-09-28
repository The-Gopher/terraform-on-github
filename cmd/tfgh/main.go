// Command tfgh runs the terraform-on-github steps by hand, one verb per step, against a
// repository's .terraform-on-github.yaml. See docs/ROADMAP.md §3.
package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/storage"

	"github.com/the-gopher/terraform-on-github/internal/apply"
	"github.com/the-gopher/terraform-on-github/internal/config"
	"github.com/the-gopher/terraform-on-github/internal/ghapp"
	"github.com/the-gopher/terraform-on-github/internal/plan"
	"github.com/the-gopher/terraform-on-github/internal/scope"
	"github.com/the-gopher/terraform-on-github/internal/store"
	"github.com/the-gopher/terraform-on-github/internal/tf"
)

const usage = `tfgh — terraform-on-github

Usage:
  tfgh <command> [flags]

Commands:
  config   Fetch, validate and print a repository's normalized workspace set
  scope    Given a PR, print the workspaces it affects and why
  plan     Run terraform plan for the workspaces a PR affects (--stage-gcs to stage §5 artifacts)
  apply    Verify that a merged PR's plan may be applied ("tfgh apply verify")

Run "tfgh <command> -h" for a command's flags.

Environment:
  GITHUB_TOKEN   required by every command
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	args := os.Args[2:]
	switch os.Args[1] {
	case "config":
		runConfig(args)
	case "scope":
		runScope(args)
	case "plan":
		runPlan(args)
	case "apply":
		runApply(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

// runConfig implements M1: resolve the config ref, fetch the file at *that* ref, parse,
// validate, and print the normalized workspaces.
func runConfig(args []string) {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	repo := fs.String("repo", "", "Repository (owner/repo)")
	configRef := fs.String("config-ref", "", "Config ref override (default refs/heads/main)")
	configFile := fs.String("config", "", "Path to local config file")
	jsonOut := fs.Bool("json", false, "Output as JSON")
	_ = fs.Parse(args)

	if *repo == "" {
		fatalf("--repo is required")
	}

	ctx := context.Background()
	client, owner, repoName := setup(*repo)

	cfg := loadConfig(ctx, client, owner, repoName, *configRef, *configFile, validate)

	if *jsonOut {
		printJSON(cfg)
		return
	}

	printConfig(os.Stdout, cfg)
}

// printConfig renders the normalized workspace set — the "what did the defaults actually
// resolve to" view that makes the command worth running over reading the YAML.
// nolint:errcheck
func printConfig(w io.Writer, cfg *config.Config) {
	fmt.Fprintf(w, "version: %d\n", cfg.Version)
	if len(cfg.ModuleSources) > 0 {
		fmt.Fprintf(w, "module_sources: %s\n", strings.Join(cfg.ModuleSources, ", "))
	}
	if len(cfg.Workspaces) == 0 {
		fmt.Fprintln(w, "No workspaces defined.")
		return
	}
	for _, ws := range cfg.Workspaces {
		fmt.Fprintf(w, "\n- %s\n", ws.Name)
		fmt.Fprintf(w, "    branch:      %s\n", ws.Branch)
		fmt.Fprintf(w, "    dir:         %s\n", ws.Dir)
		if len(ws.Watch) > 0 {
			fmt.Fprintf(w, "    watch:       %s\n", strings.Join(ws.Watch, ", "))
		}
		if ws.TerraformWorkspace != "" {
			fmt.Fprintf(w, "    tf_workspace: %s\n", ws.TerraformWorkspace)
		}
		fmt.Fprintf(w, "    terraform:   %s\n", ws.TerraformVersion)
		fmt.Fprintf(w, "    backend:     gs://%s/%s\n", ws.Backend.Bucket, ws.Backend.Prefix)
		fmt.Fprintf(w, "    impersonate: plan=%s apply=%s\n", ws.Impersonate.Plan, ws.Impersonate.Apply)
		fmt.Fprintf(w, "    apply:       enabled, environment=%s on_stale=%s\n", ws.Environment(), ws.Apply.OnStale)
	}
}

// runScope implements M2: filter the config's workspaces by the PR's base branch and changed
// paths, printing the path that pulled each one in.
func runScope(args []string) {
	fs := flag.NewFlagSet("scope", flag.ExitOnError)
	repo := fs.String("repo", "", "Repository (owner/repo)")
	pr := fs.String("pr", "", "PR number")
	configRef := fs.String("config-ref", "", "Config ref override (default refs/heads/main)")
	configFile := fs.String("config", "", "Path to local config file")
	jsonOut := fs.Bool("json", false, "Output as JSON")
	_ = fs.Parse(args)

	if *repo == "" || *pr == "" {
		fatalf("--repo and --pr are required")
	}

	ctx := context.Background()
	client, owner, repoName := setup(*repo)

	// Normalize only. Validate here would be the stricter, better behavior, but scope did not
	// validate before subcommands existed and turning it on now breaks every repo that uses the
	// `terraform_workspace` escape hatch — see validateNoOverlap and DESIGN.md §13.1.
	cfg := loadConfig(ctx, client, owner, repoName, *configRef, *configFile, normalizeOnly)

	scoper := scope.NewScoper(client, cfg)
	res, err := scoper.Scope(ctx, owner, repoName, parsePR(*pr))
	if err != nil {
		fatalf("scoping PR: %v", err)
	}

	if *jsonOut {
		printJSON(res)
		return
	}

	if res.ConfigChanged {
		fmt.Printf("Advisory: %s has changed in this PR. Changes take effect after merge to trusted ref.\n", config.Filename)
	}

	if len(res.Workspaces) == 0 {
		fmt.Println("No workspaces in scope.")
		return
	}

	for _, w := range res.Workspaces {
		fmt.Printf("- %s [%s]: %s\n", w.Workspace.Name, w.Status, w.Reason)
	}
}

// runPlan implements M3: run terraform plan for the workspaces a PR affects. With
// --workspace it plans that one workspace (verified in scope); without it, every
// workspace the PR touches.
func runPlan(args []string) {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	repo := fs.String("repo", "", "Repository (owner/repo)")
	pr := fs.String("pr", "", "PR number")
	workspace := fs.String("workspace", "", "Workspace name (optional, plans all affected if omitted)")
	configRef := fs.String("config-ref", "", "Config ref override (default refs/heads/main)")
	configFile := fs.String("config", "", "Path to local config file")
	root := fs.String("root", ".", "Path to the repository root")
	stage := fs.String("stage", "", "Directory to write plan artifacts (optional, for local testing)")
	stageGCS := fs.Bool("stage-gcs", false, "Stage artifacts to the §5 GCS buckets and sign meta.json with KMS")
	plansBucket := fs.String("plans-bucket", "", "Plan-artifact bucket (required with --stage-gcs)")
	runsBucket := fs.String("runs-bucket", "", "Coordination bucket (required with --stage-gcs)")
	kmsKey := fs.String("kms-key", "", "KMS cryptoKeyVersion for signing meta.json (required with --stage-gcs)")
	jsonOut := fs.Bool("json", false, "Output plan summaries as JSON")
	_ = fs.Parse(args)

	if *repo == "" || *pr == "" {
		fs.Usage()
		os.Exit(1)
	}
	if *stageGCS && (*plansBucket == "" || *runsBucket == "" || *kmsKey == "") {
		fatalf("--stage-gcs requires --plans-bucket, --runs-bucket and --kms-key")
	}
	if *stageGCS && *stage != "" {
		fatalf("--stage and --stage-gcs are mutually exclusive")
	}

	ctx := context.Background()
	client, owner, repoName := setup(*repo)

	cfg := loadConfig(ctx, client, owner, repoName, *configRef, *configFile, validate)

	scoper := scope.NewScoper(client, cfg)
	scopeRes, err := scoper.Scope(ctx, owner, repoName, parsePR(*pr))
	if err != nil {
		fatalf("scoping PR: %v", err)
	}

	// §4.1: plan only against an up-to-date head. A base that has moved means the plan would
	// describe a merge that is not the one under review. behind/diverged must rebase first.
	for _, w := range scopeRes.Workspaces {
		if w.Status == scope.StatusBehind || w.Status == scope.StatusDiverged {
			fatalf("PR is %s against the base for workspace %q; update the branch and re-plan", w.Status, w.Workspace.Name)
		}
	}

	var targets []string
	if *workspace != "" {
		ws, ok := cfg.Workspace(*workspace)
		if !ok {
			fatalf("workspace %q not found in config", *workspace)
		}
		inScope := false
		for _, w := range scopeRes.Workspaces {
			if w.Workspace.Name == ws.Name {
				inScope = true
				break
			}
		}
		if !inScope {
			fatalf("workspace %q is not in scope for this PR", *workspace)
		}
		targets = []string{*workspace}
	} else {
		for _, w := range scopeRes.Workspaces {
			targets = append(targets, w.Workspace.Name)
		}
	}
	if len(targets) == 0 {
		fmt.Println("No workspaces in scope to plan.")
		return
	}

	terraformPath, err := tf.FindTerraformBinary()
	if err != nil {
		fatalf("%v", err)
	}
	executor := tf.NewExecutor(terraformPath)

	var results []plan.WorkspaceResult
	failed := false

	for _, name := range targets {
		ws, ok := cfg.Workspace(name)
		if !ok {
			fatalf("workspace %q not found in config", name)
		}

		fmt.Fprintf(os.Stderr, "Planning workspace %q...\n", ws.Name)
		res, err := plan.Run(ctx, executor, ws, *root)
		if err != nil {
			// A timeout is not a plan failure: the fix is a bigger budget, not the repo.
			fatalf("%v (workspace %q)", err, ws.Name)
		}

		if *jsonOut {
			results = append(results, res)
		} else {
			fmt.Printf("Workspace: %s\n", res.Workspace)
			fmt.Printf("Has Changes: %v\n", res.HasChanges)
			fmt.Printf("Exit Code: %d\n", res.ExitCode)
			fmt.Printf("Plan File: %s\n", res.PlanFile)
			fmt.Println()
			fmt.Println(res.Summary)
		}

		if res.Failed() {
			failed = true
		}

		if *stage != "" {
			dst, err := res.Stage(*stage)
			if err != nil {
				fatalf("%v", err)
			}
			fmt.Fprintf(os.Stderr, "Artifacts written to %s\n", dst)
		}

		if *stageGCS {
			headTree, err := client.GetCommitTree(ctx, owner, repoName, scopeRes.HeadSHA)
			if err != nil {
				fatalf("resolving head tree: %v", err)
			}
			staged, err := stageToGCS(ctx, *plansBucket, *runsBucket, *kmsKey, owner, repoName, parsePR(*pr), ws.Name, headTree, ws.TerraformVersion, res, scopeRes)
			if err != nil {
				fatalf("staging %q: %v", ws.Name, err)
			}
			if staged.CacheHit {
				fmt.Fprintf(os.Stderr, "Plan for this (base, head) already staged — reusing artifact (§5.2)\n")
			} else {
				fmt.Fprintf(os.Stderr, "Staged %s · run %s\n", staged.MetaKey, staged.RunKey)
			}
		}
	}

	if *jsonOut {
		printJSON(results)
	}
	if failed {
		os.Exit(1)
	}
}

// runApply implements M5: `tfgh apply verify --repo … --pr … --workspace …`.
// It runs the §6 verification gauntlet against the staged artifact and prints
// a verdict plus a would-apply summary. It applies nothing and holds no
// writer identity — the binary mirrors the service boundary (§7): the
// operator runs terraform apply themselves, with credentials this tool never
// held.
func runApply(args []string) {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	repo := fs.String("repo", "", "Repository (owner/repo)")
	pr := fs.String("pr", "", "PR number")
	workspace := fs.String("workspace", "", "Workspace name")
	configRef := fs.String("config-ref", "", "Config ref override (default refs/heads/main)")
	plansBucket := fs.String("plans-bucket", "", "Plan-artifact bucket")
	kmsKey := fs.String("kms-key", "", "KMS cryptoKeyVersion for verifying meta.json (public-key access only)")
	_ = fs.Parse(args)

	if *repo == "" || *pr == "" || *workspace == "" {
		fs.Usage()
		os.Exit(1)
	}
	if *plansBucket == "" || *kmsKey == "" {
		fatalf("apply verify requires --plans-bucket and --kms-key")
	}

	ctx := context.Background()
	client, owner, repoName := setup(*repo)

	// The base/head SHAs key the artifact (§5); resolve them from GitHub now,
	// exactly as the plan side did — never from a stored pointer (§6.1).
	prNum := parsePR(*pr)
	pull, err := client.GetApplyPullRequest(ctx, owner, repoName, prNum)
	if err != nil {
		fatalf("reading PR %s: %v", *pr, err)
	}
	if pull.HeadSHA == "" {
		fatalf("PR %s has no head SHA", *pr)
	}
	// The artifact key composes from the base SHA the plan side scoped against
	// — the tip of the base ref at plan time. Re-derive it the same way rather
	// than trusting any pointer (§6.1).
	baseSHA, err := client.ResolveRef(ctx, owner, repoName, pull.BaseRef)
	if err != nil {
		fatalf("resolving base ref: %v", err)
	}

	gcs, err := storage.NewClient(ctx)
	if err != nil {
		fatalf("gcs client: %v", err)
	}
	defer func() { _ = gcs.Close() }()

	kmsClient, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		fatalf("kms client: %v", err)
	}
	defer func() { _ = kmsClient.Close() }()

	v := &apply.Verifier{
		GitHub:    client,
		Artifacts: store.NewGCSBucket(gcs.Bucket(*plansBucket)),
		KMS:       &kmsVerifier{client: kmsClient, keyName: *kmsKey},
		Trusted:   client,
		ConfigRef: *configRef,
	}

	verdict, err := v.Verify(ctx, owner, repoName, prNum, *workspace, baseSHA, pull.HeadSHA)
	if err != nil {
		if verdict.Reason != "" {
			fmt.Fprintf(os.Stderr, "NOT APPLICABLE: %s\n", verdict.Reason)
		}
		fatalf("%v", err)
	}

	fmt.Println(apply.WouldApply(verdict, *workspace, 0))
}

// kmsVerifier verifies the KMS signature locally: it fetches the key version's
// public key (publicKeyViewer — the only KMS grant the apply side holds, §7.2)
// and verifies the ECDSA/RSA signature in-process. There is no AsymmetricVerify
// RPC; KMS signs, the client verifies against the fetched PEM.
type kmsVerifier struct {
	client  *kms.KeyManagementClient
	keyName string
}

// Verify checks a base64 signature over the SHA-256 of digest.
func (v *kmsVerifier) Verify(ctx context.Context, digest []byte, signature string) error {
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("decoding signature: %w", err)
	}
	pub, err := v.client.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v.keyName})
	if err != nil {
		return fmt.Errorf("kms get public key: %w", err)
	}
	block, _ := pem.Decode([]byte(pub.GetPem()))
	if block == nil {
		return errors.New("kms public key is not valid PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("parsing kms public key: %w", err)
	}
	keySum := sha256.Sum256(digest)
	switch key := parsed.(type) {
	case *ecdsa.PublicKey:
		// KMS encodes EC signatures as the IEEE P1363 r||s concatenation, each
		// half the coordinate size — not ASN.1.
		size := (key.Params().N.BitLen() + 7) / 8
		if len(sig) != 2*size {
			return fmt.Errorf("ecdsa signature is %d bytes, want %d (r||s)", len(sig), 2*size)
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(key, keySum[:], r, s) {
			return errors.New("ecdsa signature does not verify")
		}
		return nil
	case *rsa.PublicKey:
		if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, keySum[:], sig); err != nil {
			return fmt.Errorf("rsa signature does not verify: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported kms key type %T", parsed)
	}
}

// stageToGCS opens the two §5 buckets and the KMS signer, then runs store.Stage
// for one workspace result. The planned_tree_sha is the head's tree (§4.1): with
// up-to-dateness enforced, every merge strategy produces the head's tree.
func stageToGCS(
	ctx context.Context,
	plansBucket, runsBucket, kmsKey, owner, repo string,
	pr int, workspaceName, plannedTreeSHA, terraformVersion string,
	res plan.WorkspaceResult, scopeRes *scope.ScopeResults,
) (*store.Staged, error) {
	gcs, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs client: %w", err)
	}
	defer func() { _ = gcs.Close() }()

	kmsClient, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("kms client: %w", err)
	}
	defer func() { _ = kmsClient.Close() }()

	return store.Stage(ctx,
		store.NewGCSBucket(gcs.Bucket(plansBucket)),
		store.NewGCSBucket(gcs.Bucket(runsBucket)),
		store.NewKMSSigner(kmsClient, kmsKey),
		res,
		store.StageInput{
			Owner:     owner,
			Repo:      repo,
			PR:        pr,
			Workspace: workspaceName,
			BaseSHA:   scopeRes.BaseSHA,
			HeadSHA:   scopeRes.HeadSHA,
			// §4.1: the head already contains base, so the head's tree is the
			// merge tree under every strategy. Resolving it here (not from the
			// plan) keeps §6.2's comparison well-defined.
			PlannedTreeSHA:   plannedTreeSHA,
			TerraformVersion: terraformVersion,
			Now:              time.Now(),
		})
}

// setup validates the shared flags every command takes and builds an authenticated client.
func setup(repo string) (*ghapp.Client, string, string) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fatalf("GITHUB_TOKEN environment variable is required")
	}

	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		fatalf("--repo must be in owner/repo format")
	}

	return ghapp.NewClient(context.Background(), token), parts[0], parts[1]
}

// Whether loadConfig runs Config.Validate after normalizing.
const (
	validate      = true
	normalizeOnly = false
)

// loadConfig fetches the config at ref, always normalizing it — an un-normalized config reads
// back defaults as unset — and validating it when the caller asks.
func loadConfig(ctx context.Context, client *ghapp.Client, owner, repo, ref, localFile string, check bool) *config.Config {
	var raw []byte
	var err error

	if localFile != "" {
		raw, err = os.ReadFile(localFile)
		if err != nil {
			fatalf("reading local config: %v", err)
		}
	} else {
		if ref == "" {
			ref = "refs/heads/main"
		}
		raw, err = client.GetContents(ctx, owner, repo, config.Filename, ref)
		if err != nil {
			fatalf("loading config from GitHub: %v", err)
		}
	}

	cfg, err := config.Parse(raw)
	if err != nil {
		fatalf("parsing config: %v", err)
	}
	cfg.Normalize()

	if check {
		if err := cfg.Validate(); err != nil {
			fatalf("invalid config: %v", err)
		}
	}
	return cfg
}

func printJSON(v any) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fatalf("encoding JSON: %v", err)
	}
	fmt.Println(string(out))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

// parsePR accepts a PR number as a string flag value.
func parsePR(s string) int {
	var pr int
	_, err := fmt.Sscanf(s, "%d", &pr)
	if err != nil {
		return 0
	}
	return pr
}
