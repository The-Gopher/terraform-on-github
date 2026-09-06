package config

import (
	"testing"
)

func TestValidationRules(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name: "valid config",
			yaml: `
version: 1
defaults:
  terraform_version: "1.9.8"
workspaces:
  - name: prod-networking
    branch: main
    dir: envs/prod/networking
    backend: { bucket: b, prefix: p }
    impersonate: { plan: p@a.com, apply: a@a.com }
`,
			wantErr: false,
		},
		{
			name: "invalid version",
			yaml: `
version: 2
workspaces:
  - name: prod-networking
    branch: main
    dir: envs/prod/networking
    backend: { bucket: b, prefix: p }
    impersonate: { plan: p@a.com, apply: a@a.com }
`,
			wantErr: true,
		},
		{
			name: "duplicate name",
			yaml: `
version: 1
defaults:
  terraform_version: "1.9.8"
workspaces:
  - name: ws1
    branch: main
    dir: d1
    backend: { bucket: b, prefix: p }
    impersonate: { plan: p@a.com, apply: a@a.com }
  - name: ws1
    branch: main
    dir: d2
    backend: { bucket: b, prefix: p }
    impersonate: { plan: p@a.com, apply: a@a.com }
`,
			wantErr: true,
		},
		{
			name: "dir contains ..",
			yaml: `
version: 1
defaults:
  terraform_version: "1.9.8"
workspaces:
  - name: prod-networking
    branch: main
    dir: ../secret
    backend: { bucket: b, prefix: p }
    impersonate: { plan: p@a.com, apply: a@a.com }
`,
			wantErr: true,
		},
		{
			name: "invalid email",
			yaml: `
version: 1
defaults:
  terraform_version: "1.9.8"
workspaces:
  - name: prod-networking
    branch: main
    dir: envs/prod/networking
    backend: { bucket: b, prefix: p }
    impersonate: { plan: not-an-email, apply: a@a.com }
`,
			wantErr: true,
		},
		{
			name: "on_stale replan_if_equivalent",
			yaml: `
version: 1
defaults:
  terraform_version: "1.9.8"
workspaces:
  - name: prod-networking
    branch: main
    dir: envs/prod/networking
    backend: { bucket: b, prefix: p }
    impersonate: { plan: p@a.com, apply: a@a.com }
    apply:
      on_stale: replan_if_equivalent
`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
