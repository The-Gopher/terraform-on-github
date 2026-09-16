module github.com/sampleserve/terraform-on-github

go 1.26.0

// The local commands (cmd/tfog-plan, cmd/tfog-apply) need exactly one dependency: a YAML
// decoder. GitHub is reached through the `gh` CLI, module sources through a scanner in
// internal/tf, and Watch globs through internal/scope — so no go-github, no HCL library and no
// doublestar until the services need them.
//
// Dependencies the two-service scaffold still anticipates:
//
//	github.com/google/go-github/v66        GitHub REST (checks, deployments, contents, compare)
//	github.com/golang-jwt/jwt/v5           App JWT for minting installation tokens
//	cloud.google.com/go/storage            plan + apply-log buckets, signed URLs
//	cloud.google.com/go/firestore          run index, workspace leases, delivery dedupe
//	cloud.google.com/go/kms                asymmetric sign (plan svc) / public key (apply svc)
//	cloud.google.com/go/cloudtasks         task enqueue
//	cloud.google.com/go/secretmanager      App private key, webhook secret
//	google.golang.org/api/idtoken          OIDC verification on /tasks/*
//	github.com/bmatcuk/doublestar/v4       Watch globs — if internal/scope's matcher outgrows itself
//	github.com/hashicorp/hcl/v2            module `source` allowlist — if internal/tf's scanner does

require (
	golang.org/x/oauth2 v0.37.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/google/go-github/v66 v66.0.0
	github.com/google/go-querystring v1.1.0 // indirect
)
