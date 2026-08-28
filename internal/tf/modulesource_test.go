package tf

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scanner has the most cases in this repo because it is the one that failed first: a
// line-oriented version missed `module "vpc" { source = "./x" }`, and a missed module block is a
// module source that never reaches the allowlist.
func TestScanModuleSources(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"single-line block", `module "vpc" { source = "./modules/vpc" }`, []string{"./modules/vpc"}},
		{
			"multi-line block",
			"module \"vpc\" {\n  source  = \"registry.terraform.io/acme/vpc/google\"\n  version = \"1.0\"\n}\n",
			[]string{"registry.terraform.io/acme/vpc/google"},
		},
		{
			"several blocks",
			"module \"a\" { source = \"./a\" }\nmodule \"b\" {\n  source = \"./b\"\n}\n",
			[]string{"./a", "./b"},
		},
		{
			"nested block source is not the module's",
			"module \"a\" {\n  source = \"./a\"\n  provisioner \"x\" { source = \"./nope\" }\n}\n",
			[]string{"./a"},
		},
		{
			"non-module blocks ignored",
			"resource \"null_resource\" \"r\" { source = \"./nope\" }\nmodule \"m\" { source = \"./yes\" }",
			[]string{"./yes"},
		},
		{
			"an identifier ending in module is not a module block",
			"submodule \"a\" { source = \"./nope\" }\nmodule \"m\" { source = \"./yes\" }",
			[]string{"./yes"},
		},
		{
			"comments",
			"module \"a\" {\n  # source = \"./hash\"\n  // source = \"./slashes\"\n  /* source = \"./block\" */\n  source = \"./real\"\n}",
			[]string{"./real"},
		},
		{
			"heredoc with an unbalanced brace",
			"module \"a\" {\n  script = <<-EOT\n    if true { echo \"}\" }\n  EOT\n  source = \"./real\"\n}",
			[]string{"./real"},
		},
		{
			"braces inside a string",
			"module \"a\" {\n  name = \"a{b}c\"\n  source = \"./real\"\n}",
			[]string{"./real"},
		},
		{
			"escaped quotes inside a string",
			"module \"a\" {\n  msg = \"say \\\"hi\\\" {\"\n  source = \"./real\"\n}",
			[]string{"./real"},
		},
		{
			"a module block quoted inside a string is not a block",
			"locals { s = \"module \\\"x\\\" { source = \\\"./evil\\\" }\" }\nmodule \"m\" { source = \"./yes\" }",
			[]string{"./yes"},
		},
		{
			"source_dir is not source",
			"module \"a\" {\n  source_dir = \"./nope\"\n  source = \"./real\"\n}",
			[]string{"./real"},
		},
		{
			"git source with a query string",
			"module \"m\" {\n  source = \"git::ssh://git@github.com/acme/mods//vpc?ref=v1.2.3\"\n}",
			[]string{"git::ssh://git@github.com/acme/mods//vpc?ref=v1.2.3"},
		},
		{
			"for_each before source",
			"module \"a\" {\n  for_each = toset([\"x\"])\n  source   = \"./real\"\n}",
			[]string{"./real"},
		},
		{"no modules", "variable \"x\" {}\noutput \"y\" { value = 1 }", nil},
		{
			"a non-literal source is not something an allowlist can evaluate",
			"module \"a\" {\n  source = local.mod\n}",
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanModuleSources(tc.src)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
}

func TestIsLocalSource(t *testing.T) {
	for _, s := range []string{"./x", "../x", "./"} {
		if !IsLocalSource(s) {
			t.Errorf("%q should be local", s)
		}
	}
	for _, s := range []string{"x", "/abs", "registry.terraform.io/a/b/c", "git::ssh://x"} {
		if IsLocalSource(s) {
			t.Errorf("%q should not be local", s)
		}
	}
}

func TestCheckModuleRefs(t *testing.T) {
	local := []ModuleRef{{Source: "./modules/vpc", File: "a.tf"}}
	if err := CheckModuleRefs(local, nil); err != nil {
		t.Errorf("local paths are always permitted: %v", err)
	}

	remote := []ModuleRef{{Source: "registry.terraform.io/acme/vpc/google", File: "a.tf"}}
	if err := CheckModuleRefs(remote, nil); err == nil {
		t.Error("an empty allowlist permits local paths only")
	} else if !strings.Contains(err.Error(), "local paths only") {
		t.Errorf("the error should explain the empty allowlist; got %v", err)
	}
	if err := CheckModuleRefs(remote, []string{"registry.terraform.io/acme/*"}); err != nil {
		t.Errorf("a matching glob should permit it: %v", err)
	}

	// `*` crosses `/` for module sources, because these are URLs rather than paths — the opposite
	// convention from Watch globs, deliberately.
	pat := []string{"git::ssh://git@github.com/acme/terraform-modules//*?ref=v*"}
	nested := []ModuleRef{{Source: "git::ssh://git@github.com/acme/terraform-modules//vpc/core?ref=v1.2.0", File: "a.tf"}}
	if err := CheckModuleRefs(nested, pat); err != nil {
		t.Errorf("a nested module path should match: %v", err)
	}
	wrongOrg := []ModuleRef{{Source: "git::ssh://git@github.com/evil/mods//x?ref=v1", File: "a.tf"}}
	if err := CheckModuleRefs(wrongOrg, pat); err == nil {
		t.Error("a different org must not match")
	} else if !errors.Is(err, ErrModuleSourceDenied) {
		t.Errorf("callers branch on ErrModuleSourceDenied; got %v", err)
	}
}

func TestCollectFollowsLocalModulesTransitively(t *testing.T) {
	// A local module can pull a remote one, and that remote source is exactly what the allowlist
	// exists to gate.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "envs/prod/main.tf"), `module "vpc" { source = "../../modules/vpc" }`)
	mustWrite(t, filepath.Join(root, "modules/vpc/main.tf"), `module "inner" { source = "registry.terraform.io/evil/x/y" }`)

	refs, err := CollectModuleSources(root, filepath.Join(root, "envs/prod"))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range refs {
		if r.Source == "registry.terraform.io/evil/x/y" {
			found = true
			if r.File != "modules/vpc/main.tf" {
				t.Errorf("the error needs the file a human can open; got %q", r.File)
			}
		}
	}
	if !found {
		t.Fatalf("the remote source hidden in a local module must be collected; got %+v", refs)
	}
	if err := CheckModuleRefs(refs, nil); err == nil {
		t.Error("it must then be denied by the empty allowlist")
	}
}

func TestCollectRejectsLocalSourceEscapingTheRepo(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	mustWrite(t, filepath.Join(root, "envs/main.tf"), `module "x" { source = "../../../etc" }`)
	if _, err := CollectModuleSources(root, filepath.Join(root, "envs")); err == nil {
		t.Fatal("a local source outside the repository would pull code that is not in the reviewed tree")
	}
}

func TestMatchURLGlob(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"*", "anything", true},
		{"a*c", "abc", true},
		{"a*c", "ac", true},
		{"a*c", "abd", false},
		{"registry.terraform.io/acme/*", "registry.terraform.io/acme/vpc/google", true},
		{"registry.terraform.io/acme/*", "registry.terraform.io/other/vpc", false},
		{"?bc", "abc", true},
		{"?bc", "bc", false},
	}
	for _, tc := range cases {
		if got := matchURLGlob(tc.pat, tc.s); got != tc.want {
			t.Errorf("matchURLGlob(%q, %q) = %t, want %t", tc.pat, tc.s, got, tc.want)
		}
	}
}

func TestProviderLockDigest(t *testing.T) {
	dir := t.TempDir()
	// No lock file is a real answer, not a failure: the repo pins nothing.
	got, err := ProviderLockDigest(dir)
	if err != nil || got != "" {
		t.Fatalf("no lock file: got (%q, %v), want (\"\", nil)", got, err)
	}
	mustWrite(t, filepath.Join(dir, ".terraform.lock.hcl"), "provider \"x\" {}\n")
	got, err = ProviderLockDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 64 {
		t.Errorf("expected a hex sha256, got %q", got)
	}
}

func TestCheckDivergence(t *testing.T) {
	if err := CheckDivergence(Divergence{UpToDate: true}); err != nil {
		t.Errorf("an up-to-date head may proceed: %v", err)
	}
	err := CheckDivergence(Divergence{BehindBy: 3})
	if err == nil {
		t.Fatal("a behind head must be refused")
	}
	if !errors.Is(err, ErrBranchBehindBase) {
		t.Errorf("callers branch on ErrBranchBehindBase; got %v", err)
	}
	if !strings.Contains(err.Error(), "3 commit") {
		t.Errorf("the author needs to know how far behind; got %v", err)
	}
	if err := CheckDivergence(Divergence{BehindBy: 1, Conflicted: true}); !strings.Contains(err.Error(), "cleanly") {
		t.Errorf("a conflict should be reported in the same message; got %v", err)
	}
}

func TestInitRefusesUpgrade(t *testing.T) {
	// -upgrade would decouple the plan from .terraform.lock.hcl. There is no legitimate caller,
	// so this fails before running anything.
	r := &Runner{WorkDir: t.TempDir(), Binary: "/nonexistent-terraform"}
	err := r.Init(t.Context(), InitOpts{Upgrade: true})
	if err == nil || !strings.Contains(err.Error(), "-upgrade") {
		t.Fatalf("expected a refusal naming -upgrade, got %v", err)
	}
}

func TestWriteCLIConfigRequiresAMirror(t *testing.T) {
	// A provider_installation block with no mirror silently permits direct installs, which is the
	// opposite of the intent.
	if err := WriteCLIConfig(filepath.Join(t.TempDir(), "tf.rc"), ""); err == nil {
		t.Fatal("an empty mirror URL must be refused")
	}
	path := filepath.Join(t.TempDir(), "tf.rc")
	if err := WriteCLIConfig(path, "https://mirror.internal/"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "network_mirror") || strings.Contains(string(body), "direct") {
		t.Errorf("unexpected CLI config:\n%s", body)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
