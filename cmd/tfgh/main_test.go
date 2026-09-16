package main

import (
	"context"
	"testing"

	"github.com/sampleserve/terraform-on-github/internal/config"
)

type mockReader struct {
	resolveFunc func(ctx context.Context, owner, repo, ref string) (string, error)
	readFunc    func(ctx context.Context, owner, repo, path, sha string) ([]byte, error)
}

func (m *mockReader) ResolveRef(ctx context.Context, owner, repo, ref string) (string, error) {
	return m.resolveFunc(ctx, owner, repo, ref)
}

func (m *mockReader) ReadFileAtSHA(ctx context.Context, owner, repo, path, sha string) ([]byte, error) {
	return m.readFunc(ctx, owner, repo, path, sha)
}

func TestRunConfigShow_RepoParsing(t *testing.T) {
	ctx := context.Background()
	reader := &mockReader{
		resolveFunc: func(ctx context.Context, owner, repo, ref string) (string, error) {
			return "1234567890abcdef", nil
		},
		readFunc: func(ctx context.Context, owner, repo, path, sha string) ([]byte, error) {
			return []byte("version: 1\nworkspaces: [{name: test, branch: main, dir: ., backend: {bucket: b, prefix: p}, impersonate: {plan: plan-service@project.iam.gserviceaccount.com, apply: apply-service@project.iam.gserviceaccount.com}, terraform_version: 1.9.8}]"), nil
		},
	}
	trustedRefs := make(config.TrustedRefs)

	cases := []struct {
		name    string
		repo    string
		wantErr bool
	}{
		{"valid repo", "SampleServe/iac", false},
		{"missing slash", "iac", true},
		{"empty repo", "", true},
		{"too many slashes", "owner/repo/extra", false}, // SplitN(..., 2) should handle this as owner="owner", repo="repo/extra"
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runConfigShow(ctx, reader, trustedRefs, tc.repo, "")
			if (err != nil) != tc.wantErr {
				t.Errorf("runConfigShow(%q) error = %v, wantErr %v", tc.repo, err, tc.wantErr)
			}
		})
	}
}
