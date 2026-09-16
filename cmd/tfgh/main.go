package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/ghapp"
	"github.com/sampleserve/terraform-on-github/internal/scope"
	"gopkg.in/yaml.v3"
)

func main() {
	repo := flag.String("repo", "", "Repository (owner/repo)")
	prNum := flag.Int("pr", 0, "Pull Request number")
	configRef := flag.String("config-ref", "", "Config ref override")
	jsonOut := flag.Bool("json", false, "Output as JSON")
	flag.Parse()

	if *repo == "" || *prNum == 0 {
		fmt.Fprintln(os.Stderr, "Error: --repo and --pr are required")
		os.Exit(1)
	}

	ctx := context.Background()
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "Error: GITHUB_TOKEN environment variable is required")
		os.Exit(1)
	}

	var parts = strings.Split(*repo, "/")
	if len(parts) != 2 {
		fmt.Fprintln(os.Stderr, "Error: --repo must be in owner/repo format")
		os.Exit(1)
	}
	owner, repoName := parts[0], parts[1]

	client := ghapp.NewClient(ctx, token)

	ref := "refs/heads/main"
	if *configRef != "" {
		ref = *configRef
	}

	cfgBytes, err := client.GetContents(ctx, owner, repoName, config.Filename, ref)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	var cfg config.Config
	if err := yaml.Unmarshal(cfgBytes, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing config: %v\n", err)
		os.Exit(1)
	}

	scoper := scope.NewScoper(client, &cfg)
	res, err := scoper.Scope(ctx, owner, repoName, *prNum)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error scoping PR: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(out))
		return
	}

	if res.ConfigChanged {
		fmt.Println("Advisory: .terraform-on-github.yaml has changed in this PR. Changes take effect after merge to trusted ref.")
	}

	if len(res.Workspaces) == 0 {
		fmt.Println("No workspaces in scope.")
		return
	}

	for _, w := range res.Workspaces {
		fmt.Printf("- %s [%s]: %s\n", w.Workspace.Name, w.Status, w.Reason)
	}
}
