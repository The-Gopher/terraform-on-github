package plan

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/the-gopher/terraform-on-github/internal/config"
	"github.com/the-gopher/terraform-on-github/internal/tf"
)

// mockExecutor stands in for tf.Executor; each hook records the call and returns canned
// values, so Run's orchestration — init before plan, both shows after — is observable.
type mockExecutor struct {
	initCalls int
	planCalls int
	showCalls int

	planResult tf.PlanResult
	planErr    error

	plan    *tfjson.Plan
	showErr error
}

func (m *mockExecutor) Init(ctx context.Context, workDir string, backendConfig map[string]string) error {
	m.initCalls++
	return nil
}

func (m *mockExecutor) Plan(ctx context.Context, workDir string, opts tf.PlanOptions) (tf.PlanResult, error) {
	m.planCalls++
	return m.planResult, m.planErr
}

func (m *mockExecutor) ShowPlanJSON(ctx context.Context, planFile string) (*tfjson.Plan, error) {
	m.showCalls++
	if m.showErr != nil {
		return nil, m.showErr
	}
	return m.plan, nil
}

func (m *mockExecutor) ShowPlanRaw(ctx context.Context, planFile string) (string, error) {
	m.showCalls++
	if m.showErr != nil {
		return "", m.showErr
	}
	return "raw plan", nil
}

func workspace() config.Workspace {
	return config.Workspace{
		Name:             "prod",
		Dir:              "envs/prod",
		TerraformVersion: "1.9.8",
		Backend:          config.Backend{Bucket: "b", Prefix: "p"},
		SummaryDetail:    config.SummaryAddresses,
	}
}

func changesPlan(t *testing.T) *tfjson.Plan {
	t.Helper()
	raw, err := os.ReadFile("../tf/testdata/plan-with-secret.json")
	if err != nil {
		t.Fatal(err)
	}
	var p tfjson.Plan
	if err := p.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	return &p
}

func TestRun_OrchestratesAndSummarizes(t *testing.T) {
	ex := &mockExecutor{
		planResult: tf.PlanResult{
			HasChanges: true,
			ExitCode:   2,
			PlanFile:   "tfplan",
		},
		plan: changesPlan(t),
	}

	res, err := Run(context.Background(), ex, workspace(), "/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ex.initCalls != 1 || ex.planCalls != 1 || ex.showCalls != 2 {
		t.Errorf("calls: init=%d plan=%d show=%d, want 1/1/2", ex.initCalls, ex.planCalls, ex.showCalls)
	}
	if res.Workspace != "prod" {
		t.Errorf("Workspace = %q", res.Workspace)
	}
	if !res.HasChanges || res.ExitCode != 2 {
		t.Errorf("HasChanges/ExitCode = %v/%d, want true/2", res.HasChanges, res.ExitCode)
	}
	if res.Failed() {
		t.Error("exit code 2 is a success per §4.3; Failed() = true")
	}
	if res.Summary == "" {
		t.Error("summary must not be empty")
	}
	if res.PlanJSON == nil || len(res.PlanJSON.ResourceChanges) == 0 {
		t.Error("PlanJSON must carry parsed resource changes")
	}
}

func TestRun_TimeoutSurfacesAsTimeout(t *testing.T) {
	ex := &mockExecutor{
		planResult: tf.PlanResult{TimedOut: true},
		planErr:    errors.New("terraform plan timed out after 1s"),
	}

	_, err := Run(context.Background(), ex, workspace(), "/repo")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, tf.ErrPlanTimeout) {
		t.Errorf("error %v should wrap tf.ErrPlanTimeout", err)
	}
}

func TestRun_PlanFailureNotMarkedTimeout(t *testing.T) {
	ex := &mockExecutor{
		planResult: tf.PlanResult{ExitCode: 1},
		planErr:    errors.New("boom"),
	}

	_, err := Run(context.Background(), ex, workspace(), "/repo")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, tf.ErrPlanTimeout) {
		t.Error("plain plan failure must not read as a timeout")
	}
}

func TestStage(t *testing.T) {
	p := changesPlan(t)
	res := WorkspaceResult{
		Workspace: "prod",
		PlanRaw:   "Plan: 2 to add.",
		PlanJSON:  p,
	}

	dst, err := res.Stage(t.TempDir())
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dst, "tfplan.txt"))
	if err != nil || string(raw) != res.PlanRaw {
		t.Errorf("tfplan.txt = %q err=%v", raw, err)
	}

	raw, err = os.ReadFile(filepath.Join(dst, "plan.json"))
	if err != nil {
		t.Fatalf("plan.json: %v", err)
	}
	var back tfjson.Plan
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Errorf("staged plan.json does not round-trip: %v", err)
	}
}