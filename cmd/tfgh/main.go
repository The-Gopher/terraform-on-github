package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/The-Gopher/terraform-on-github/internal/config"
	"github.com/The-Gopher/terraform-on-github/internal/ghapp"
	"github.com/spf13/cobra"
)

var (
	repoFlag      string
	configRefFlag string
	tokenFlag     string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "tfgh",
		Short: "Terraform on GitHub CLI",
	}

	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration",
	}

	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Show the configuration for a repository",
		Run:   runConfigShow,
	}

	showCmd.Flags().StringVar(&repoFlag, "repo", "", "Repository in owner/repo format (required)")
	showCmd.Flags().StringVar(&configRefFlag, "config-ref", "refs/heads/main", "Trusted ref to read config from")
	showCmd.Flags().StringVar(&tokenFlag, "token", os.Getenv("GITHUB_TOKEN"), "GitHub API token")
	showCmd.MarkFlagRequired("repo")

	configCmd.AddCommand(showCmd)
	rootCmd.AddCommand(configCmd)

	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func runConfigShow(cmd *cobra.Command, args []string) {
	ctx := context.Background()
	if tokenFlag == "" {
		log.Fatal("GITHUB_TOKEN is required")
	}

	client := ghapp.NewClient(ctx, tokenFlag)
	
	parts := strings.Split(repoFlag, "/")
	if len(parts) != 2 {
		log.Fatal("repo must be in owner/repo format")
	}
	owner, repo := parts[0], parts[1]

	data, err := client.GetFileContent(ctx, owner, repo, ".terraform-on-github.yaml", configRefFlag)
	if err != nil {
		log.Fatalf("failed to fetch config: %v", err)
	}

	cfg, err := config.Parse(data)
	if err != nil {
		log.Fatalf("failed to parse config: %v", err)
	}

	fmt.Printf("Configuration for %s @ %s:\n", repoFlag, configRefFlag)
	for _, w := range cfg.Workspaces {
		fmt.Printf("- %s (branch: %s, dir: %s)\n", w.Name, w.Branch, w.Dir)
	}
}
