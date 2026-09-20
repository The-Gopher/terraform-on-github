package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exampleConfig is the file shipped at the repo root, so the worked example and the parser cannot
// drift apart.
func exampleConfig(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".terraform-on-github.example.yaml"))
	if err != nil {
		t.Fatalf("reading the example config: %v", err)
	}
	return b
}

func TestParseExample(t *testing.T) {
	c, err := ParseAndValidate(exampleConfig(t))
	if err != nil {
		t.Fatalf("the shipped example config must validate: %v", err)
	}
	want := []string{"prod-networking", "integration", "prod-networking-next"}
	if len(c.Workspaces) != len(want) {
		t.Fatalf("got %d workspaces, want %d", len(c.Workspaces), len(want))
	}
	for i, n := range want {
		if c.Workspaces[i].Name != n {
			t.Errorf("workspace %d: got %q, want %q", i, c.Workspaces[i].Name, n)
		}
	}
}

func TestDurationsDecode(t *testing.T) {
	c, err := ParseAndValidate(exampleConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	w, _ := c.Workspace("prod-networking")
	if w.PlanTimeout.Duration() != 20*time.Minute {
		t.Errorf("plan_timeout: got %s, want 20m", w.PlanTimeout)
	}
	if w.ApplyTimeout.Duration() != 45*time.Minute {
		t.Errorf("apply_timeout: got %s, want 45m", w.ApplyTimeout)
	}
	if w.Apply.ApprovalTimeout.Duration() != 24*time.Hour {
		t.Errorf("approval_timeout: got %s, want 24h", w.Apply.ApprovalTimeout)
	}
}

func TestDurationAcceptsCompoundForm(t *testing.T) {
	// time.ParseDuration handles this; a hand-rolled regexp would not.
	c, err := ParseAndValidate(withReplacement(t, "plan_timeout: 20m", "plan_timeout: 1h30m"))
	if err != nil {
		t.Fatal(err)
	}
	w, _ := c.Workspace("prod-networking")
	if w.PlanTimeout.Duration() != 90*time.Minute {
		t.Errorf("got %s, want 1h30m", w.PlanTimeout)
	}
}

func TestDefaultsInheritAndOverride(t *testing.T) {
	c, err := ParseAndValidate(exampleConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	prod, _ := c.Workspace("prod-networking")
	integration, _ := c.Workspace("integration")
	if prod.SummaryDetail != SummaryAddresses {
		t.Errorf("prod-networking should inherit summary_detail addresses, got %q", prod.SummaryDetail)
	}
	if integration.SummaryDetail != SummaryFull {
		t.Errorf("integration should override summary_detail to full, got %q", integration.SummaryDetail)
	}
}

func TestApplyDefaults(t *testing.T) {
	c, err := ParseAndValidate(exampleConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	integration, _ := c.Workspace("integration")
	next, _ := c.Workspace("prod-networking-next")
	// Apply is now always enabled for all workspaces.
	if got := integration.Environment(); got != "integration" {
		t.Errorf("environment should default to the workspace name, got %q", got)
	}
}

func withReplacement(t *testing.T, old, new string) []byte {
	t.Helper()
	s := string(exampleConfig(t))
	if !strings.Contains(s, old) {
		t.Fatalf("example config no longer contains %q; update this test", old)
	}
	return []byte(strings.Replace(s, old, new, 1))
}

func TestRejects(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
		wantIn   string
	}{
		{"wrong version", "version: 1", "version: 2", "version"},
		{"dir escapes the repo", "dir: envs/prod/networking", "dir: ../../etc", "escapes"},
		{"absolute dir", "dir: envs/prod/networking", "dir: /etc", "repo-relative"},
		{"dir escapes after cleaning", "dir: envs/prod/networking", "dir: envs/../../etc", "escapes"},
		{"bad name", "name: prod-networking\n", "name: Prod_Networking\n", "name"},
		{"duplicate name", "name: integration", "name: prod-networking", "duplicate"},
		{"version range", `terraform_version: "1.9.8"`, `terraform_version: "~> 1.9"`, "exact version"},
		{"bad service account", "tf-prod-networking-plan@acme-tf.iam.gserviceaccount.com", "nope", "service account"},
		{"bad duration", "plan_timeout: 20m", "plan_timeout: 20 minutes", "duration"},
		{"missing bucket", "      bucket: acme-tfstate-prod\n", "", "backend"},
		{"cut on_stale policy", "on_stale: fail", "on_stale: replan_if_equivalent", "replan_if_equivalent"},
		{"unknown summary detail", "summary_detail: addresses", "summary_detail: verbose", "summary_detail"},
		{"missing terraform version", `terraform_version: "1.9.8"`, `terraform_version: ""`, "required"},
		{"bad on_stale", "on_stale: fail", "on_stale: replan_if_equivalent", "removed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAndValidate(withReplacement(t, tc.old, tc.new))
			if err == nil {
				t.Fatal("expected the config to be rejected")
			}
			if tc.wantIn != "" && !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error should mention %q; got: %v", tc.wantIn, err)
			}
		})
	}
}

// A misspelled security-relevant key must fail rather than silently default. This is the whole
// reason Parse sets KnownFields.
func TestRejectsUnknownKeys(t *testing.T) {
	for _, typo := range []string{"impersonat:", "require_protected_bases: true", "modul_sources: []"} {
		_, err := ParseAndValidate(withReplacement(t, "version: 1", "version: 1\n"+typo))
		if err == nil {
			t.Errorf("%q should be rejected as an unknown field", typo)
		}
	}
}

func TestRejectsReservedPlanArgs(t *testing.T) {
	for _, arg := range []string{"-out=/tmp/x", "-lock=true", "-state=other.tfstate", "-var-file=x.tfvars", "-input=true", "-chdir=/"} {
		_, err := ParseAndValidate(withReplacement(t,
			"summary_detail: addresses", "summary_detail: addresses\n  plan_args: [\""+arg+"\"]"))
		if err == nil {
			t.Errorf("plan_args %q should be rejected as reserved", arg)
		}
	}
}

func TestRejectsOverlappingDirsOnOneBranch(t *testing.T) {
	s := string(exampleConfig(t))
	s = strings.Replace(s, "dir: envs/integration", "dir: envs/prod/networking/sub", 1)
	s = strings.Replace(s, "branch: integration", "branch: main", 1)
	_, err := ParseAndValidate([]byte(s))
	if err == nil || !strings.Contains(err.Error(), "overlapping") {
		t.Fatalf("overlapping dirs on one branch must be rejected; got %v", err)
	}
}

func TestDirsOverlapRespectsPathBoundaries(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"envs/prod", "envs/prod", true},
		{"envs/prod", "envs/prod/networking", true},
		{"envs/prod", "envs/production", false},
		{"envs/prod", "envs/staging", false},
	}
	for _, tc := range cases {
		if got := dirsOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("dirsOverlap(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestIsServiceAccountEmail(t *testing.T) {
	valid := []string{
		"tf-prod-networking-plan@acme-tf.iam.gserviceaccount.com",
		"tf-integration-apply@my-project-123.iam.gserviceaccount.com",
	}
	invalid := []string{
		"", "nope", "someone@example.com",
		"tf-plan@acme-tf.iam.gserviceaccount.com.evil.com",
		"TF-PLAN@acme-tf.iam.gserviceaccount.com",
		"x@acme-tf.iam.gserviceaccount.com", // too short to be a real SA id
	}
	for _, s := range valid {
		if !isServiceAccountEmail(s) {
			t.Errorf("%q should be accepted", s)
		}
	}
	for _, s := range invalid {
		if isServiceAccountEmail(s) {
			t.Errorf("%q should be rejected", s)
		}
	}
}

func TestParseRejectsSecondDocument(t *testing.T) {
	_, err := Parse([]byte("version: 1\nworkspaces: []\n---\nversion: 1\n"))
	if err == nil {
		t.Fatal("two documents are ambiguous about which one governs; must be rejected")
	}
}
