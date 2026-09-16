package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/ghapp"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: tfgh <command> [args]")
		fmt.Println("Commands: config")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "config":
		handleConfig()
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

func handleConfig() {
	showCmd := flag.NewFlagSet("show", flag.ExitOnError)

	repo := showCmd.String("repo", "", "Repository (owner/repo)")
	configRef := showCmd.String("config-ref", "", "Trusted ref override")
	configPath := showCmd.String("config", "", "Local config file override")

	if len(os.Args) < 3 {
		fmt.Println("Usage: tfgh config <subcommand>")
		fmt.Println("Subcommands: show")
		os.Exit(1)
	}
	switch os.Args[2] {
	case "show":
		showCmd.Parse(os.Args[3:])
		if *repo == "" && *configPath == "" {
			fmt.Println("Error: either --repo or --config must be provided")
			showCmd.Usage()
			os.Exit(1)
		}

		var cfg *config.Config
		var err error

		if *configPath != "" {
			cfg, err = config.LoadFromFile(*configPath)
			if err == nil {
				fmt.Printf("Loaded config from local file: %s\n", *configPath)
			}
		} else {
			// M1: Resolve the trusted ref, fetch config, parse, validate, print.
			ctx := context.Background()
			token := os.Getenv("GITHUB_TOKEN")
			if token == "" {
				fmt.Println("Error: GITHUB_TOKEN environment variable is required")
				os.Exit(1)
			}

			client := ghapp.NewClient(ctx, token)
			
			// For v0.1 CLI, we treat the trusted ref as the default branch unless overridden.
			// In v0.2 this mapping moves to a registry file/database.
			ref := *configRef
			if ref == "" {
				ref = "heads/main" // Fallback to default branch
			}

			owner, repoName, err := splitRepo(*repo)
			if err != nil {
				fmt.Printf("Error parsing repo %q: %v\n", *repo, err)
				os.Exit(1)
			}

			sha, err := client.ResolveRef(ctx, owner, repoName, ref)
			if err != nil {
				fmt.Printf("Error resolving ref %q: %v\n", ref, err)
				os.Exit(1)
			}

			raw, err := client.GetContents(ctx, owner, repoName, config.Filename, sha)
			if err != nil {
				fmt.Printf("Error fetching config %q at %s: %v\n", config.Filename, sha, err)
				os.Exit(1)
			}

			cfg, err = config.ParseAndValidate(raw)
			if err != nil {
				fmt.Printf("Config validation failed:\n%v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Loaded config for %s at %s\n", *repo, sha)
		}

		if err != nil {
			fmt.Printf("Error loading config: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Normalized Workspaces:\n")
		for _, w := range cfg.Workspaces {
			fmt.Printf("- %-20s (branch: %-15s, dir: %s)\n", w.Name, w.Branch, w.Dir)
		}

	default:
		fmt.Printf("Unknown config subcommand: %s\n", os.Args[2])
		os.Exit(1)
	}
}

func splitRepo(repo string) (string, string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("repo must be in format owner/repo")
	}
	return parts[0], parts[1], nil
}
