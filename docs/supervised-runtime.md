# Supervised runtime image

## Ownership

The runtime publisher assembles a published Linux amd64 orchestrator host-binary
bundle with a published Cloud host-supervisor image. Both inputs are cell-local
ECR digest references. There is no source checkout from Cloud, floating tag or
host download in assembly. The output lives in the cell's existing
`e2b-orchestrator` repository and contains both binaries under `/opt/e2b`.

The original bundle publisher and Infra's existing host installation remain
available for legacy deployments. Infrastructure still supplies kernel,
Firecracker, envd, KVM/NBD/networking, hugepages, storage, tools and IAM.

## Publish

1. Apply the additive Infra publisher permission in each target cell. It grants
   read-only access to that cell's supervisor repository. No new host or node
   rollout is needed for this IAM change.
2. Publish the desired runtime bundle with `Klex Publish Orchestrator` and obtain
   its full source commit, `release.json` version and immutable image digest.
3. Obtain the cell-local supervisor image digest from Cloud publication and its
   full Cloud source commit. Use a supervisor build with retirement support when
   planning a later retirement-safety activation.
4. Dispatch `Klex Publish Supervised Runtime` on `klex/main`. Supply the target
   environment, bundle digest/commit/version and supervisor digest/commit.
   The selected environment supplies the existing ECR publisher identity.
5. Record the returned image digest, assembly commit and release artifact.

The publisher rejects wrong-cell repositories, invalid digests, unmerged runtime
commits, mismatched source labels and bundle identities. Checksums, ELF
architecture and library resolution are checked in the final supervisor base.
The supervisor's shutdown smoke test runs before publication. The pinned Node
base's built-in Mozilla CA roots are exported for Go and chart preflight use.

The immutable tag hashes all inputs plus the assembly commit. A rerun reuses an
existing image only after checking its recorded inputs and rerunning verification
and the supervisor smoke test. An unexpected ECR read failure is fatal.
Publication changes neither GitOps nor running hosts.

## GitOps cutover

Use the chart's opt-in `supervisor.runtimeDelivery: image` and separate
`supervisor.runtimeImage` repository, digest and assembly revision. The existing
`expectedRuntimeVersion` and `expectedRuntimeCommit` must match the image's
recorded runtime identity. Runtime startup rejects mismatches. Cloud's ordinary
supervisor promotion continues to update only its unused base-image pin.

Image-backed mode uses the same immutable image for preflight and the supervisor,
removes the host `/var/lib/e2b/bin` mount, and uses DaemonSet `OnDelete`.
Changing the image does not delete existing pods. This is not an automated
zero-downtime rollover implementation.

For initial adoption or rollback, hold broker admission and autoscaler mutations,
hold external producers, reconcile in-flight requests, and verify each affected
host is empty using matching provider, supervisor and host process evidence.
Only then replace that host's pod. Verify its image digest, runtime identity,
readiness and fresh resource summary before proceeding. Do not use node-label
removal or pod deletion to drain running work. Release maintenance only after
all selected hosts use the intended image and acceptance passes.

Retirement safety remains disabled unless separately activated in both the
supervisor and autoscaler. This change does not enable host retirement, placement
packing, automatic replacement capacity or release sequencing.

Rollback is another verified-empty cutover. Pin the prior assembled image and
matching identity, replace only verified-empty pods, then validate telemetry.
Returning to host-backed delivery also requires the matching bootstrap binary
and maintenance because legacy mode uses RollingUpdate. Reverting values alone
is not a safe busy-host rollback.

## Local tests

```sh
python3 -m unittest discover -s scripts -p test_supervised_runtime_release.py
bash scripts/test-supervised-runtime-image.sh
```

Fixture composition needs Docker but no AWS or private image credentials. The
protected publication job additionally validates and smoke-tests the real inputs.
