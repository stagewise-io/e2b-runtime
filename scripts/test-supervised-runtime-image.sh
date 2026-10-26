#!/usr/bin/env bash
# Credential-free fixture test of actual Dockerfile composition and startup checks.
set -euo pipefail
context="$(mktemp -d)"
trap 'rm -rf "$context"' EXIT
export ENVIRONMENT=preview
export REPOSITORY_URLS='{"orchestrator":"347282156621.dkr.ecr.eu-central-1.amazonaws.com/stagewise-e2b-preview-e2b-orchestrator"}'
BUNDLE_DIGEST="sha256:$(printf '1%.0s' {1..64})"
SUPERVISOR_DIGEST="sha256:$(printf '2%.0s' {1..64})"
BUNDLE_COMMIT="$(printf '3%.0s' {1..40})"
SUPERVISOR_COMMIT="$(printf '4%.0s' {1..40})"
ASSEMBLY_COMMIT="$(printf '5%.0s' {1..40})"
export BUNDLE_DIGEST SUPERVISOR_DIGEST BUNDLE_COMMIT SUPERVISOR_COMMIT ASSEMBLY_COMMIT
export BUNDLE_VERSION=0.16.1+klex.333333333333
python3 scripts/supervised_runtime_release.py >"$context/runtime-release.json"
cp packages/orchestrator/verify-supervised-runtime.cjs "$context/"
for target in fixture-bundle fixture-supervisor; do
  docker build --platform linux/amd64 --target "$target" --tag "supervised-runtime:$target" --file scripts/supervised-runtime-fixture.Dockerfile .
done
docker build --platform linux/amd64 --file packages/orchestrator/supervised-runtime.Dockerfile \
  --build-arg BUNDLE_IMAGE=supervised-runtime:fixture-bundle \
  --build-arg SUPERVISOR_IMAGE=supervised-runtime:fixture-supervisor \
  --build-arg ASSEMBLY_COMMIT="$ASSEMBLY_COMMIT" --build-arg BUNDLE_COMMIT="$BUNDLE_COMMIT" \
  --build-arg SUPERVISOR_COMMIT="$SUPERVISOR_COMMIT" --tag supervised-runtime:fixture "$context"
docker run --rm --platform linux/amd64 -e EXPECTED_ASSEMBLY_COMMIT="$ASSEMBLY_COMMIT" supervised-runtime:fixture | grep -q 'Fixture supervisor started'
if docker run --rm --platform linux/amd64 -e EXPECTED_ASSEMBLY_COMMIT=wrong supervised-runtime:fixture; then
  echo 'Mismatched assembly identity was accepted' >&2; exit 1
fi
if docker run --rm --platform linux/amd64 -e EXPECTED_RUNTIME_COMMIT=wrong supervised-runtime:fixture; then
  echo 'Mismatched runtime identity was accepted' >&2; exit 1
fi
if docker run --rm --platform linux/amd64 -e CELL_NAME=production-machines supervised-runtime:fixture; then
  echo 'Cross-cell runtime was accepted' >&2; exit 1
fi
docker run --rm --platform linux/amd64 --entrypoint sh supervised-runtime:fixture -ec \
  'printf corrupt >> /opt/e2b/orchestrator; if node /opt/e2b/verify-supervised-runtime.cjs; then exit 1; fi'
echo 'Fixture composition, startup, cross-cell rejection and checksum rejection passed'
