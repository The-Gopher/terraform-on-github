module github.com/sampleserve/terraform-on-github

go 1.23

// Dependencies the scaffold anticipates, added when the stubs are filled in:
//
//	github.com/google/go-github/v66        GitHub REST (checks, deployments, contents, compare)
//	github.com/golang-jwt/jwt/v5           App JWT for minting installation tokens
//	cloud.google.com/go/storage            plan + apply-log buckets, signed URLs
//	cloud.google.com/go/firestore          run index, workspace leases, delivery dedupe
//	cloud.google.com/go/kms                asymmetric sign (plan svc) / public key (apply svc)
//	cloud.google.com/go/cloudtasks         task enqueue
//	cloud.google.com/go/secretmanager      App private key, webhook secret
//	google.golang.org/api/idtoken          OIDC verification on /tasks/*
//	github.com/bmatcuk/doublestar/v4       Watch globs ("modules/vpc/**")
//	github.com/hashicorp/hcl/v2            module `source` allowlist pre-pass before init
//	sigs.k8s.io/yaml                       strict config decode (KnownFields)
