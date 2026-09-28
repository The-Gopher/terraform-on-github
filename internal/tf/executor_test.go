package tf

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeTerraform builds a shell script masquerading as terraform. tfexec shells out to
// whatever binary it is handed, so a script that exits with a chosen code — and optionally
// sleeps to force a timeout — exercises Plan's exit-code mapping and timeout detection
// against the real command plumbing.
func fakeTerraform(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "terraform")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func module(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// tfexec requires a directory with at least one .tf file to run plan.
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte("variable \"x\" {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPlan_ExitCodeNoChanges(t *testing.T) {
	// Exit 0, no changes.
	path := fakeTerraform(t, "#!/bin/sh\nexit 0\n")
	e := NewExecutor(path).(*tfexecExecutor)

	result, err := e.Plan(context.Background(), module(t), PlanOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.HasChanges {
		t.Error("HasChanges = true, want false")
	}
}

func TestPlan_ExitCodeChanges(t *testing.T) {
	// Exit 2, changes present — a success per §4.3, not an error.
	path := fakeTerraform(t, "#!/bin/sh\nexit 2\n")
	e := NewExecutor(path).(*tfexecExecutor)

	result, err := e.Plan(context.Background(), module(t), PlanOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", result.ExitCode)
	}
	if !result.HasChanges {
		t.Error("HasChanges = false, want true")
	}
}

func TestPlan_ExitCodeFailure(t *testing.T) {
	// Exit 1 — a real failure. ExitCode carries 1, not 0/2.
	path := fakeTerraform(t, "#!/bin/sh\necho boom >&2\nexit 1\n")
	e := NewExecutor(path).(*tfexecExecutor)

	result, err := e.Plan(context.Background(), module(t), PlanOptions{})
	if err == nil {
		t.Fatal("expected error for exit code 1, got nil")
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", result.ExitCode)
	}
	if result.TimedOut {
		t.Error("TimedOut = true, want false")
	}
}

func TestPlan_Timeout(t *testing.T) {
	// Sleeps longer than the deadline; the kill must surface as TimedOut, not a plain
	// terraform failure, and the error must wrap context.DeadlineExceeded.
	path := fakeTerraform(t, "#!/bin/sh\nsleep 30\n")
	e := NewExecutor(path).(*tfexecExecutor)

	result, err := e.Plan(context.Background(), module(t), PlanOptions{PlanTimeout: 2 * time.Second})
	if err == nil {
		t.Fatal("expected error for timed-out plan, got nil")
	}
	if !result.TimedOut {
		t.Error("TimedOut = false, want true")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
	}
}

func TestPlan_ZeroTimeoutRunsForever(t *testing.T) {
	// PlanTimeout unset (zero) means no deadline — must not time out immediately.
	path := fakeTerraform(t, "#!/bin/sh\nexit 0\n")
	e := NewExecutor(path).(*tfexecExecutor)

	result, err := e.Plan(context.Background(), module(t), PlanOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.TimedOut {
		t.Error("TimedOut = true, want false for zero timeout")
	}
}