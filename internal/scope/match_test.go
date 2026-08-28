package scope

import (
	"testing"

	"github.com/sampleserve/terraform-on-github/internal/config"
)

func ws(name, branch, dir string, watch ...string) config.Workspace {
	return config.Workspace{Name: name, Branch: branch, Dir: dir, Watch: watch}
}

func TestMatchBranch(t *testing.T) {
	cases := []struct {
		pattern, ref string
		want         bool
	}{
		{"main", "main", true},
		{"main", "mainline", false},
		{"main", "", false},
		{"", "main", false},
		{"release/*", "release/2026-08", true},
		// One trailing segment only. A pattern that quietly matched "release/a/b" would widen
		// which identity a run may assume.
		{"release/*", "release/a/b", false},
		{"release/*", "release/", false},
		{"release/*", "release", false},
		{"release/*", "releases/x", false},
		{"release/*", "prerelease/x", false},
	}
	for _, tc := range cases {
		if got := matchBranch(tc.pattern, tc.ref); got != tc.want {
			t.Errorf("matchBranch(%q, %q) = %t, want %t", tc.pattern, tc.ref, got, tc.want)
		}
	}
}

func TestTouchesDirOnPathBoundary(t *testing.T) {
	w := ws("prod", "main", "envs/prod")
	if !touches(w, []string{"envs/prod/main.tf"}) {
		t.Error("a file inside the dir must count")
	}
	if !touches(w, []string{"envs/prod"}) {
		t.Error("the dir itself must count")
	}
	if touches(w, []string{"envs/production/main.tf"}) {
		t.Error("envs/prod must not match envs/production")
	}
	if touches(w, []string{"README.md"}) {
		t.Error("an unrelated file must not count")
	}
}

func TestTouchesWatchGlobs(t *testing.T) {
	deep := ws("prod", "main", "envs/prod", "modules/vpc/**")
	if !touches(deep, []string{"modules/vpc/a/b/c.tf"}) {
		t.Error("** must cross separators")
	}
	if touches(deep, []string{"modules/dns/main.tf"}) {
		t.Error("a different module must not match")
	}

	shallow := ws("prod", "main", "envs/prod", "modules/*")
	if !touches(shallow, []string{"modules/main.tf"}) {
		t.Error("* must match within one segment")
	}
	if touches(shallow, []string{"modules/vpc/main.tf"}) {
		t.Error("* must not cross a separator — that is the whole distinction from **")
	}
}

func TestMatchPathGlob(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"modules/**", "modules/a", true},
		{"modules/**", "modules/a/b/c", true},
		{"modules/*", "modules/a", true},
		{"modules/*", "modules/a/b", false},
		{"**/main.tf", "main.tf", true},
		{"**/main.tf", "a/b/main.tf", true},
		{"**/main.tf", "a/b/other.tf", false},
		{"modules/vpc/*.tf", "modules/vpc/main.tf", true},
		{"modules/vpc/*.tf", "modules/vpc/sub/main.tf", false},
		{"envs/?/main.tf", "envs/a/main.tf", true},
		{"envs/?/main.tf", "envs/ab/main.tf", false},
		{"**", "anything/at/all", true},
		{"modules/a*/x", "modules/abc/x", true},
		{"modules/a*/x", "modules/b/x", false},
	}
	for _, tc := range cases {
		if got := matchPathGlob(tc.pattern, tc.path); got != tc.want {
			t.Errorf("matchPathGlob(%q, %q) = %t, want %t", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func cfg() *config.Config {
	return &config.Config{
		Version: config.SchemaVersion,
		Workspaces: []config.Workspace{
			ws("prod-networking", "main", "envs/prod/networking", "modules/vpc/**"),
			ws("integration", "integration", "envs/integration", "modules/**"),
			ws("prod-next", "release/*", "envs/prod/networking"),
		},
	}
}

func TestMatchSelectsByBranchThenFiltersByPath(t *testing.T) {
	r, err := Match(cfg(), "main", []string{"envs/prod/networking/main.tf"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.InScope) != 1 || r.InScope[0].Name != "prod-networking" {
		t.Fatalf("in scope: %+v", names(r.InScope))
	}
	if len(r.Candidates) != 1 {
		t.Errorf("candidates should be the branch-bound set: %v", names(r.Candidates))
	}

	r, _ = Match(cfg(), "main", []string{"README.md"})
	if len(r.InScope) != 0 {
		t.Errorf("nothing should be in scope: %v", names(r.InScope))
	}
	if len(r.Candidates) != 1 {
		t.Errorf("candidates explain why nothing planned: %v", names(r.Candidates))
	}
}

func TestMatchWatchGlobPullsAWorkspaceIn(t *testing.T) {
	r, _ := Match(cfg(), "main", []string{"modules/vpc/subnets.tf"})
	if len(r.InScope) != 1 || r.InScope[0].Name != "prod-networking" {
		t.Fatalf("a watched shared module must scope the workspace: %v", names(r.InScope))
	}
}

func TestMatchBranchGlob(t *testing.T) {
	r, _ := Match(cfg(), "release/2026-08", []string{"envs/prod/networking/x.tf"})
	if len(r.InScope) != 1 || r.InScope[0].Name != "prod-next" {
		t.Fatalf("release/* should select prod-next: %v", names(r.InScope))
	}
}

func TestMatchAllCandidatesIgnoresTheDiff(t *testing.T) {
	// The truncated-file-list path: over-planning is recoverable, silently skipping a workspace
	// whose files changed is the failure nobody notices.
	r, err := MatchAllCandidates(cfg(), "main", []string{"README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.InScope) != 1 || r.InScope[0].Name != "prod-networking" {
		t.Fatalf("every branch-bound workspace should be in scope: %v", names(r.InScope))
	}
}

func TestConfigChangeIsFlagged(t *testing.T) {
	r, _ := Match(cfg(), "main", []string{config.Filename})
	if !r.ConfigChanged {
		t.Error("editing the config file must be flagged")
	}
	r, _ = Match(cfg(), "main", []string{"a.tf"})
	if r.ConfigChanged {
		t.Error("false positive on an unrelated file")
	}
}

func TestMatchRejectsBadInput(t *testing.T) {
	if _, err := Match(nil, "main", nil); err == nil {
		t.Error("a nil config must be an error, not an empty result")
	}
	if _, err := Match(cfg(), "", nil); err == nil {
		t.Error("an empty base ref must be an error: it would match nothing and look like 'no changes'")
	}
}

func names(ws []config.Workspace) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Name
	}
	return out
}
