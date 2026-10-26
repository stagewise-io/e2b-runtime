"""Resolve cell-local immutable inputs for a supervised runtime release."""

import hashlib
import json
import os
import re


def release_plan(env):
    environment = env["ENVIRONMENT"]
    if environment not in ("preview", "production"):
        raise ValueError("unknown environment")
    account = "347282156621" if environment == "preview" else "925213028165"
    registry = f"{account}.dkr.ecr.eu-central-1.amazonaws.com"
    repository = f"{registry}/stagewise-e2b-{environment}-e2b-orchestrator"
    if json.loads(env["REPOSITORY_URLS"])["orchestrator"] != repository:
        raise ValueError("orchestrator repository must belong to the selected cell")
    for name in ("BUNDLE_DIGEST", "SUPERVISOR_DIGEST"):
        if (
            not re.fullmatch(r"sha256:[a-f0-9]{64}", env[name])
            or env[name] == "sha256:" + "0" * 64
        ):
            raise ValueError(f"invalid {name}")
    for name in ("BUNDLE_COMMIT", "SUPERVISOR_COMMIT", "ASSEMBLY_COMMIT"):
        if not re.fullmatch(r"[a-f0-9]{40}", env[name]) or env[name] == "0" * 40:
            raise ValueError(f"invalid {name}")
    version = env["BUNDLE_VERSION"]
    if not re.fullmatch(
        r"[0-9]+\.[0-9]+\.[0-9]+\+klex\." + env["BUNDLE_COMMIT"][:12], version
    ):
        raise ValueError("bundle version must match its source commit")
    plan = {
        "schemaVersion": 1,
        "cell": f"{environment}-machines",
        "assemblyCommit": env["ASSEMBLY_COMMIT"],
        "runtimeCommit": env["BUNDLE_COMMIT"],
        "runtimeVersion": version,
        "supervisorCommit": env["SUPERVISOR_COMMIT"],
        "bundleImage": f"{repository}@{env['BUNDLE_DIGEST']}",
        "supervisorImage": f"{registry}/stagewise-e2b-{environment}-machine-host-supervisor@{env['SUPERVISOR_DIGEST']}",
        "os": "linux",
        "arch": "amd64",
    }
    identity = hashlib.sha256(
        json.dumps(plan, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()
    return {**plan, "image": f"{repository}:supervised-{identity}"}


if __name__ == "__main__":
    print(json.dumps(release_plan(os.environ), sort_keys=True, indent=2))
