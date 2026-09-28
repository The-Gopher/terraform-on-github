package tf

import (
	"testing"
	"time"
)

func TestValidatePlanArgs_Allowed(t *testing.T) {
	args := []string{"-target=aws_instance.foo", "-var=foo=bar", "-parallelism=5"}
	err := ValidatePlanArgs(args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidatePlanArgs_Rejected(t *testing.T) {
	reserved := []string{
		"-lock=false",
		"-out=tfplan",
		"-input=false",
		"-state=foo",
		"-var-file=vars.tfvars",
		"-chdir=subdir",
		"--lock=false",
		"--out=tfplan",
	}
	for _, arg := range reserved {
		err := ValidatePlanArgs([]string{arg})
		if err == nil {
			t.Errorf("expected error for reserved arg %q, got nil", arg)
		}
	}
}

func TestPlanOptions_Defaults(t *testing.T) {
	opts := PlanOptions{
		TerraformVersion:   "1.9.0",
		PlanTimeout:        10 * time.Minute,
		VarFiles:           []string{"vars.tfvars"},
		PlanArgs:           []string{"-target=aws_instance.foo"},
		TerraformWorkspace: "prod",
		Lock:               false,
		BackendConfig:      map[string]string{"bucket": "my-bucket", "prefix": "prod/"},
	}

	if opts.TerraformVersion != "1.9.0" {
		t.Errorf("TerraformVersion = %q, want %q", opts.TerraformVersion, "1.9.0")
	}
	if opts.PlanTimeout != 10*time.Minute {
		t.Errorf("PlanTimeout = %v, want %v", opts.PlanTimeout, 10*time.Minute)
	}
	if len(opts.VarFiles) != 1 || opts.VarFiles[0] != "vars.tfvars" {
		t.Errorf("VarFiles = %v, want [vars.tfvars]", opts.VarFiles)
	}
	if len(opts.PlanArgs) != 1 || opts.PlanArgs[0] != "-target=aws_instance.foo" {
		t.Errorf("PlanArgs = %v, want [-target=aws_instance.foo]", opts.PlanArgs)
	}
	if opts.TerraformWorkspace != "prod" {
		t.Errorf("TerraformWorkspace = %q, want %q", opts.TerraformWorkspace, "prod")
	}
	if opts.Lock != false {
		t.Errorf("Lock = %v, want %v", opts.Lock, false)
	}
	if len(opts.BackendConfig) != 2 || opts.BackendConfig["bucket"] != "my-bucket" || opts.BackendConfig["prefix"] != "prod/" {
		t.Errorf("BackendConfig = %v, want {bucket:my-bucket, prefix:prod/}", opts.BackendConfig)
	}
}

func TestPlanResult_Fields(t *testing.T) {
	result := PlanResult{
		HasChanges: true,
		ExitCode:   2,
		PlanFile:   "/tmp/tfplan",
		Stdout:     "Plan: 1 to add, 0 to change, 0 to destroy.",
		Stderr:     "",
	}

	if !result.HasChanges {
		t.Error("HasChanges should be true")
	}
	if result.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", result.ExitCode)
	}
	if result.PlanFile != "/tmp/tfplan" {
		t.Errorf("PlanFile = %q, want %q", result.PlanFile, "/tmp/tfplan")
	}
	if result.Stdout != "Plan: 1 to add, 0 to change, 0 to destroy." {
		t.Errorf("Stdout = %q", result.Stdout)
	}
	if result.Stderr != "" {
		t.Errorf("Stderr = %q, want empty", result.Stderr)
	}
}