package scope

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-github/v60/github"
	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/ghapp"
)
type mockClient struct {
	pr        *github.PullRequest
	refSHA    string
	comparison *ghapp.Comparison
}

func (m *mockClient) GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error) {
	return m.pr, nil
}

func (m *mockClient) ResolveRef(ctx context.Context, owner, repo, ref string) (string, error) {
	return m.refSHA, nil
}

func (m *mockClient) CompareCommits(ctx context.Context, owner, repo, base, head string) (*ghapp.Comparison, error) {
	return m.comparison, nil
}

func TestScope(t *testing.T) {
	cfg := &config.Config{
		Workspaces: []config.Workspace{
			{
				Name:   "prod",
				Branch: "release/*",
				Dir:    "envs/prod",
				Watch:  []string{"modules/shared/**"},
			},
			{
				Name:   "dev",
				Branch: "dev",
				Dir:    "envs/dev",
			},
		},
	}

	tests := []struct {
		name     string
		baseRef  string
		files    []*github.CommitFile
		ahead    int
		behind   int
		wantWS   []string
		wantStat []Status
		wantCfg  bool
	}{
		{
			name:    "branch glob match",
			baseRef: "release/v1.0",
			files:   []*github.CommitFile{{Filename: new("envs/prod/main.tf")}},
			wantWS:  []string{"prod"},
			wantStat: []Status{StatusAhead},
			ahead:   1,
			wantCfg: false,
		},
		{
			baseRef: "release/v1.0",
			files:   []*github.CommitFile{{Filename: new("README.md")}},
			wantWS:  []string{},
			wantCfg: false,
		},
		{
			baseRef: "release/v1.0",
			files:   []*github.CommitFile{{Filename: new("envs/prod/main.tf")}},
			wantWS:  []string{"prod"},
			wantStat: []Status{StatusAhead},
			ahead:   1,
			wantCfg: false,
		},
		{
			baseRef: "release/v1.0",
			files:   []*github.CommitFile{{Filename: new("modules/shared/vpc/main.tf")}},
			wantWS:  []string{"prod"},
			wantStat: []Status{StatusAhead},
			ahead:   1,
			wantCfg: false,
		},
		{
			baseRef: "release/v1.0",
			files:   []*github.CommitFile{{Filename: new(config.Filename)}},
			wantWS:  []string{},
			wantCfg: true,
		},
		{
			baseRef: "release/v1.0",
			files:   []*github.CommitFile{{Filename: new("envs/prod/main.tf")}},
			wantWS:  []string{"prod"},
			wantStat: []Status{StatusBehind},
			behind:   1,
			wantCfg: false,
		},
		{
			baseRef: "release/v1.0",
			files:   []*github.CommitFile{{Filename: new("envs/prod/main.tf")}},
			wantWS:  []string{"prod"},
			wantStat: []Status{StatusDiverged},
			ahead:   1,
			behind:   1,
			wantCfg: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &mockClient{
				pr: &github.PullRequest{
					Base: &github.PullRequestBranch{Ref: new(tt.baseRef)},
					Head: &github.PullRequestBranch{SHA: new("head")},
				},
				refSHA: "base",
				comparison: &ghapp.Comparison{
					Files:    tt.files,
					AheadBy:  tt.ahead,
					BehindBy: tt.behind,
				},
			}

			s := NewScoper(client, cfg)
			res, err := s.Scope(context.Background(), "owner", "repo", 1)
			if err != nil {
				t.Fatalf("Scope failed: %v", err)
			}

			if res.ConfigChanged != tt.wantCfg {
				t.Errorf("ConfigChanged = %v, want %v", res.ConfigChanged, tt.wantCfg)
			}

			if len(res.Workspaces) != len(tt.wantWS) {
				t.Fatalf("got %d workspaces, want %d", len(res.Workspaces), len(tt.wantWS))
			}

			for i, w := range res.Workspaces {
				if w.Workspace.Name != tt.wantWS[i] {
					t.Errorf("ws[%d] = %s, want %s", i, w.Workspace.Name, tt.wantWS[i])
				}
				if len(tt.wantStat) > 0 && w.Status != tt.wantStat[i] {
					t.Errorf("ws[%d] status = %s, want %s", i, w.Status, tt.wantStat[i])
				}
			}
		})
	}
}
func TestScopeJSON(t *testing.T) {
	cfg := &config.Config{
		Workspaces: []config.Workspace{{Name: "prod", Branch: "main", Dir: "envs/prod"}},
	}
	client := &mockClient{
		pr: &github.PullRequest{
			Base: &github.PullRequestBranch{Ref: new("main")},
			Head: &github.PullRequestBranch{SHA: new("head")},
		},
		refSHA: "base",
		comparison: &ghapp.Comparison{
			Files: []*github.CommitFile{{Filename: new("envs/prod/main.tf")}},
			AheadBy: 1,
		},
	}
	s := NewScoper(client, cfg)
	res, _ := s.Scope(context.Background(), "owner", "repo", 1)
	
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("json marshal failed: %v", err)
	}
	
	out := string(data)
	if !strings.Contains(out, `"ConfigChanged":false`) {
		t.Errorf("expected ConfigChanged:false in json, got %s", out)
	}
	if !strings.Contains(out, `"Workspace":{"Name":"prod"`) {
		t.Errorf("expected prod workspace in json, got %s", out)
	}
	if !strings.Contains(out, `"Status":"ahead"`) {
		t.Errorf("expected status ahead in json, got %s", out)
	}
}
