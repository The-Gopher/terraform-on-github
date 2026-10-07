package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestRun_Usage covers the invocations that fail before any network call: they must come back
// as errors with the right exit status, never exit the process.
func TestRun_Usage(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "") // a usage error must win over a missing token

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string // substring of the returned error; "" = don't check
	}{
		{"no command", nil, 2, "no command"},
		{"unknown command", []string{"bogus"}, 2, `unknown command "bogus"`},
		{"help", []string{"help"}, 0, ""},
		{"subcommand help", []string{"scope", "-h"}, 0, ""},
		{"undefined flag", []string{"scope", "--nope"}, 2, ""},
		{"missing repo", []string{"config"}, 2, "owner/repo"},
		{"malformed repo", []string{"config", "--repo", "a/b/c"}, 2, "owner/repo"},
		{"missing pr", []string{"scope", "--repo", "a/b"}, 2, "--pr"},
		{"non-numeric pr", []string{"scope", "--repo", "a/b", "--pr", "abc"}, 2, ""},
		{"negative pr", []string{"plan", "--repo", "a/b", "--pr", "-3"}, 2, "--pr"},
		{"stage conflict", []string{"plan", "--repo", "a/b", "--pr", "1", "--stage", "d", "--stage-gcs",
			"--plans-bucket", "p", "--runs-bucket", "r", "--kms-key", "k"}, 2, "mutually exclusive"},
		{"apply needs workspace", []string{"apply", "--repo", "a/b", "--pr", "1"}, 2, "--workspace"},
		{"missing token", []string{"config", "--repo", "a/b"}, 1, "GITHUB_TOKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(context.Background(), tt.args, &stdout, &stderr)
			if got := exitCode(err); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d (err = %v)", got, tt.wantCode, err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestRun_HelpGoesToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Usage:") || stderr.Len() != 0 {
		t.Errorf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}
