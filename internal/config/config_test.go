package config

import (
	"testing"
)

func TestParse(t *testing.T) {
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
    impersonate: { plan: p@a.com, apply: a@com }
`,
			wantErr: true,
		},
		{
			name: "missing workspaces",
			yaml: `
version: 1
workspaces: []
`,
			wantErr: true,
		},
		{
			name: "invalid name",
			yaml: `
version: 1
defaults:
  terraform_version: "1.9.8"
workspaces:
  - name: "Invalid Name!"
    branch: main
    dir: envs/prod/networking
    backend: { bucket: b, prefix: p }
    impersonate: { plan: p@a.com, apply: a@a.com }
`,
			wantErr: true,
		},
		{
			name: "absolute dir",
			yaml: `
version: 1
defaults:
  terraform_version: "1.9.8"
workspaces:
  - name: prod-networking
    branch: main
    dir: /abs/path
    backend: { bucket: b, prefix: p }
    impersonate: { plan: p@a.com, apply: a@a.com }
`,
			wantErr: true,
		},
		{
			name: "non-exact version",
			yaml: `
version: 1
defaults:
  terraform_version: "~> 1.9"
workspaces:
  - name: prod-networking
    branch: main
    dir: envs/prod/networking
    backend: { bucket: b, prefix: p }
    impersonate: { plan: p@a.com, apply: a@a.com }
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
