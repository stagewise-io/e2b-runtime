#!/usr/bin/env bash
# Run from the trusted workflow checkout before changing the source tree.
set -euo pipefail

if [[ -z "${SOURCE_COMMIT:-}" ]]; then
  exit 0
fi
[[ "${EVENT_NAME:-}" == workflow_dispatch && "${WORKFLOW_REF:-}" == refs/heads/klex/main ]] || {
  echo 'Pinned runtime source requires a dispatch from klex/main' >&2
  exit 1
}
[[ "$SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ && "$SOURCE_COMMIT" != 0000000000000000000000000000000000000000 ]] || {
  echo 'Runtime source must be a full nonzero commit SHA' >&2
  exit 1
}
git cat-file -e "$SOURCE_COMMIT^{commit}"
git merge-base --is-ancestor "$SOURCE_COMMIT" "$WORKFLOW_COMMIT"
git checkout --detach "$SOURCE_COMMIT"
