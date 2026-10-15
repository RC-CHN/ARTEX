#!/usr/bin/env bash
# Run against the exact runtime image under test; fixtures contain no user data.
set -euo pipefail
export ARTEX_RUNNER_MANAGER_IMAGE="${1:?runtime image required}"
export ARTEX_RUNNER_DOCKER_IMAGE=python@sha256:423ed6ab25b1921a477529254bfeeabf5855151dc2c3141699a1bfc852199fbf
export ARTEX_RUNNER_BINARY=/tmp/artex-runner-integration
CGO_ENABLED=0 go build -o "$ARTEX_RUNNER_BINARY" ./cmd/artex-runner
docker pull "$ARTEX_RUNNER_DOCKER_IMAGE"
go test -race ./integration -run TestManagedRunnerEnvironments -count=1 -timeout=5m
POSTGRES_PASSWORD=ci-validation docker compose --profile runners config --quiet
docker run --rm --entrypoint kubectl -v "$PWD:/src:ro" "$ARTEX_RUNNER_MANAGER_IMAGE" kustomize /src/deploy/runner/k8s > /tmp/runner-k8s.yaml
