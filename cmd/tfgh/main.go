package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/ghapp"
)

func main() {
	repo := flag.String("repo", "", "GitHub repository (owner/repo)")
	configRef := flag.String("config-ref", "", "Trusted ref for config (override)")
	token := flag.String("token", os.Getenv("GITHUB_TOKEN"), "GitHub token")
	flag.Parse()

	if *repo == "" {
		log.Fatal("missing -repo")
	}
	if *token == "" {
		log.Fatal("missing -token or GITHUB_TOKEN env")
	}

	ctx := context.Background()
	client, err := ghapp.NewClient(ctx, *token)
	if err != nil {
		log.Fatal(err)
	}

	reg := config.NewRegistry("trusted-refs.json")
	trustedRefs, err := reg.Load()
	if err != nil {
		log.Fatalf("registry load: %v", err)
	}

	if err := runConfigShow(ctx, client, trustedRefs, *repo, *configRef); err != nil {
		log.Fatal(err)
	}
}

func runConfigShow(ctx context.Context, reader config.ContentsReader, trustedRefs config.TrustedRefs, repo, configRef string) error {
	var owner, repoName string
	fmt.Sscanf(repo, "%s/%s", &owner, &repoName)
	if owner == "" || repoName == "" {
		return fmt.Errorf("repo must be in form owner/repo")
	}

	ref := configRef
	if ref == "" {
		ref = trustedRefs[repo]
		if ref == "" {
			ref = "refs/heads/main" // Default
		}
	}

	sha, err := reader.ResolveRef(ctx, owner, repoName, ref)
	if err != nil {
		return fmt.Errorf("resolve ref: %w", err)
	}

	raw, err := reader.ReadFileAtSHA(ctx, owner, repoName, config.Filename, sha)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	cfg, err := config.ParseAndValidate(raw)
	if err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	fmt.Printf("Config for %s (at %s)\n", repo, sha[:8])
	for _, w := range cfg.Workspaces {
		fmt.Printf("- %s: %s (dir: %s)\n", w.Name, w.Branch, w.Dir)
	}
	return nil
}
