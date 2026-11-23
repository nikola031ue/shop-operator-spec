# shop-operator image (spec 002, research R16). Both base images are pinned by digest;
# bump them deliberately in a PR. The runtime image is distroless and runs as non-root.

# Go toolchain that builds the manager binary.
ARG BUILDER_IMAGE=golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea
# Minimal runtime: no shell, no package manager, user 65532.
ARG RUNTIME_IMAGE=gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5

FROM ${BUILDER_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

# Go sources only (.dockerignore excludes everything else, including tests).
COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o manager ./cmd

FROM ${RUNTIME_IMAGE}
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
