# Shared build for both services. SERVICE selects the entrypoint; TF_VERSION is baked in.
#
# Terraform is baked into the image rather than downloaded at runtime for two reasons: the
# sandbox has no egress to release.hashicorp.com by design (see DESIGN.md §8), and an unverified
# runtime download inside a process holding live credentials is exactly what we are trying to
# avoid. Supporting several Terraform versions means publishing several image tags and routing
# by Workspace.TerraformVersion, not fetching on demand.

ARG TF_VERSION=1.9.8

# ---- terraform ------------------------------------------------------------
FROM hashicorp/terraform:${TF_VERSION} AS tf

# ---- build ----------------------------------------------------------------
FROM golang:1.23-alpine AS build
ARG SERVICE
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/service ./cmd/${SERVICE}

# ---- runtime --------------------------------------------------------------
# git is needed for the checkout, so this cannot be distroless/static. Alpine plus a non-root
# user is the compromise; the meaningful containment is the egress allowlist and the read-only
# credentials, not the base image.
FROM alpine:3.20

RUN apk add --no-cache git ca-certificates openssh-client \
    && adduser -D -u 10001 runner \
    && mkdir -p /workspace && chown runner:runner /workspace

COPY --from=tf /bin/terraform /usr/local/bin/terraform
COPY --from=build /out/service /usr/local/bin/service

ARG TF_VERSION
ENV TERRAFORM_VERSION=${TF_VERSION} \
    TF_IN_AUTOMATION=1 \
    TF_INPUT=0 \
    CHECKPOINT_DISABLE=1 \
    TF_PLUGIN_CACHE_DIR=/workspace/.plugin-cache

# /workspace is a tmpfs mount at runtime (see deploy/services.tf); everything else is read-only.
USER runner
WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/service"]
