# Resource admission

## Implementation status

The shared CPU/disk policy, protobuf contract, configuration types, host controller,
and Linux filesystem sampler are implemented and unit-tested. Startup now shares one
controller across runtime roles and InfoService. Enabled startup requires the host
process lock and successful reclaim. Public creates reserve before template fetching;
common VM constructors reconcile actual rootfs size and bind leases to cleanup.
Tracked construction workers retain commitments across late cleanup registration.
Resume-fresh checkpoints reserve replacement capacity before destructive work.
Pause and both checkpoint routes share the create ledger for snapshot writes;
reservations remain until deferred writers and upload completion settle. Harvest
VMs use common factory admission and skip the optimization on refusal. Bounded
controller telemetry and API placement filters are implemented. Implementation
validation is complete within the test scope below. Privileged KVM/NBD and database
integration checks remain unvalidated. Both switches remain disabled, and deployment
values have not been selected.

No deployment settings, artifact publishers, database schema, or external services
change in this implementation stage.

## Configuration contract

| Setting | Default | Meaning |
| --- | --- | --- |
| `RESOURCE_ADMISSION_ENABLED` | `false` | Enable the node-wide policy. Keep disabled pending deployment validation. |
| `RESOURCE_ADMISSION_CPU_PERCENT` | Unset | Positive integer allocation percentage. Values above 100 permit shared-vCPU overcommit. |
| `RESOURCE_ADMISSION_DISK_PERCENT` | Unset | Logical-rootfs commitment budget, from 1 through 100 percent of filesystem total bytes. |
| `RESOURCE_ADMISSION_DISK_HEADROOM_MIB` | Unset | Positive free-space margin in MiB. Conversion to bytes must not overflow. |
| `PLACEMENT_RESOURCE_ADMISSION` | `false` | Environment fallback for the API's `placement-resource-admission` flag. Node enforcement remains independent. |

Enabled policies require explicit valid quantities. Disabled controllers need no
CPU or filesystem readers and preserve no-op reservation behavior. Deployment
percentages and headroom have not been selected.

## Quantities and checks

CPU capacity is `floor(hostCPUCount * CPUPercent / 100)`. A lifecycle commits its
allocated `vcpu`, not its `max_vcpus`. The existing quota path must constrain those
vCPUs before guest execution. Architecture/model compatibility and hugepage policy
remain separate checks.

Logical disk capacity is `floor(rootfsFilesystemTotalBytes * DiskPercent / 100)`.
Each physical VM lifecycle commits its full logical rootfs size. Exactly fitting
requests pass. Requested template free space and allocated filesystem blocks are
not substitutes for logical rootfs size.

For current operation-space admission, outstanding write estimates plus headroom
and the incoming estimate must fit the filesystem's available bytes. The initial
start estimate is the full logical rootfs size. Logical commitments are not added
to measured filesystem-used bytes, because those quantities overlap.

Percentages use checked 128-bit intermediate arithmetic. Byte/MiB conversion,
additions, and fit comparisons reject overflow instead of wrapping.

## Filesystem sampling

The controller requires the `rootfs`, `build`, and `template` role paths. Integration
must supply `StorageConfig.SandboxCacheDir`, `DefaultCacheDir`, and
`StorageConfig.TemplateCacheDir`, respectively. Startup supplies those paths. `SandboxDir` does not identify the
writable rootfs cache.

The Linux sampler opens a directory and obtains its device identity and `Statfs`
values from the same descriptor. Free space uses `Bavail`, not `Bfree`. Paths and
bind aliases on the same device share one ledger. Separate devices keep separate
headroom and write reservations; their capacities are never summed. Every report
checks all configured role paths. Invalid measurements or device changes reject
new enforced admission without discarding existing commitments.

## Lease contract

One controller must belong to the single orchestrator process and cover every
runtime role. Its mutex serializes measurement, fit checks, and ledger mutations.
VM setup, network operations, object-storage work, and cleanup callbacks must run
outside that mutex.

A start lease reserves CPU, logical rootfs bytes, and the start write estimate
atomically. `Claim` transfers it to one physical lifecycle owner. A repeated claim
by that owner is harmless; another owner cannot reuse it. `Grow` atomically raises
an underestimated rootfs reservation before VM setup. Growth failure changes
nothing. `Started` drops only the start write estimate. CPU and logical disk stay
charged until physical cleanup completes.

`Release(nil)` returns a lease's commitments once. Cleanup failure retains the
commitments, marks the controller uncertain, and prevents further admission. Later
stale callbacks cannot undo that failure. Operation-write leases follow the same
release rule. Their estimates may decrease after bytes materialize or persistence
completes, but cannot increase without new admission.

Lease identity is independent of sandbox/execution IDs. Replacement VMs need a
second lease before destructive checkpoint work, even if the replacement reuses
the original identity. Startup must reject enabled admission when old runtimes
have not been safely reclaimed. Startup and common constructors now enforce these
requirements. Resume-fresh checkpoints obtain replacement leases before the
original leaves the live map. In-place checkpoints reserve writes without a
second CPU lease. A failed or busy checkpoint releases unclaimed replacement
capacity; constructor-owned leases remain bound to physical cleanup.

## Snapshot-write ownership

Enabled admission reserves snapshot writes even when legacy pause admission is
disabled. Build diffs, template snapfiles, and a local-storage second copy are
charged to their actual destination devices. Shared devices use the same ledger
as starts; separate devices are checked independently. Once all diff writers
finish, measured bytes replace the build and template estimates. A local second
copy remains reserved through upload completion or abandonment.

Upload completion alone cannot release a reservation while deferred memory or
rootfs writers remain. Every writer is joined, including after another writer
fails. If completion cannot be established, commitments remain and the controller
becomes uncertain. Disabled mode preserves the legacy pause accounting.

## Reporting and API integration

InfoService returns a fresh controller snapshot in the optional protobuf report.
It distinguishes missing support from a supported reporter's enabled state. It carries a policy version, validity and uncertainty,
effective CPU limits and commitments, and per-filesystem roles, capacities,
logical commitments, write reservations, and headroom. Valid zero commitment is
idle, not a missing sample. Conversion copies mutable slices and rejects malformed
policy, duplicate filesystem identities/roles, and invalid measurements.

The shared fit predicate accepts conservative API-local pending quantities and
checks each filesystem's headroom. With `placement-resource-admission` enabled,
the API applies it during best-of-K sampling and the final atomic pending insert,
including preferred-node resumes. CPU-model, feature, label and hugepage checks
remain separate. Disk demand comes from `SandboxConfig.total_disk_size_mb`.

Absent, unsupported-version or enforcement-disabled reports are capability
mismatches. An enabled reporter with invalid, uncertain, future-dated or stale
measurements is temporarily unavailable. Freshness uses the API-local request
start time with a 30-second maximum age, not a node wall clock. Slower responses
requested before the retained sample cannot replace it.

Local overlays include CPU and full logical disk. Successful overlays stay until
a report requested after that success arrives, even if this temporarily counts a
request twice. Attempt tokens prevent stale completions from deleting newer work
with the same sandbox ID. API deletion does not subtract authoritative commitments.
Only physical cleanup on the node releases those commitments. Independent API
replicas still race, so node admission remains authoritative.

Node refusals use gRPC `ResourceExhausted` with a bounded `ErrorInfo` reason and no
host quantities. API retries retain the existing jittered backoff and attempt
accounting. Capacity-only failures use HTTP 503 `sandbox_capacity_unavailable` with
retryability. An unsupported fleet uses `sandbox_feature_unsupported` without a
retry hint. Earlier ambiguous create errors are not reclassified as capacity.

## Storage guarantee limits

The intended guarantees are a hard logical-rootfs commitment ceiling and
serialized operation-space admission. Sampling free space does not reserve extents,
cap template caches, stop unrelated host writers, or guarantee guest growth while
unbounded caches and snapshots accumulate. Ordinary RAM admission is not added by
this controller. Full physical-storage isolation requires additional controls.

## Release and enablement checklist

Merging this implementation does not enable admission. The fork's current
`Klex Publish API` workflow publishes only the API image on changes to `klex/main`.
It does not publish a matching orchestrator. An API-only release cannot provide
node-authoritative enforcement or resource reports from older orchestrators.

Before enabling either switch:

- Obtain matching API and orchestrator artifacts from the merged commit through
  the approved release process. Record immutable image digests and keep rollback
  artifacts. Publishing the orchestrator is a separate release prerequisite.
- Complete database integration checks and privileged Linux lifecycle checks with
  KVM/NBD. Exercise create, cleanup, pause, both checkpoint paths, harvest, restart
  and startup reclaim. The local results below do not establish these guarantees.
- Select explicit CPU and disk percentages and disk headroom for each node pool.
  Confirm the rootfs, build and template paths refer to the intended filesystems.
  Do not infer safe production values from the unit tests.
- Deploy compatible artifacts with both switches disabled. Verify the existing
  sandbox lifecycle and legacy live-allocation metrics before enabling admission.
- Enable node admission on a canary with the chosen policy. Validate host-lock
  ownership, startup reclaim, valid versioned reports, physical cleanup, and bounded
  refusal reasons. Verify rejected requests do not leak commitments. Expand only
  after the canary checks pass.
- Enable API placement filtering only when every node eligible for that traffic
  reports the supported, enabled policy. Older or disabled nodes are capability
  mismatches when filtering is enabled. Verify capacity failures return retryable
  HTTP 503 responses rather than capability errors.
- Update the autoscaler separately after reports are available. Treat live
  allocations, in-flight/stopping commitments, write reservations, and invalid or
  stale measurements as distinct quantities. Do not subtract deleted sandboxes
  from authoritative commitments or sum capacities across separate filesystems.
  This PR does not change the autoscaler contract or its scaling policy.

For rollback, disable API filtering before returning nodes to a legacy or disabled
reporter. Node configuration is fixed until restart. Restarting with enforcement
disabled must follow the existing drain/reclaim procedure; changing an environment
variable is not a live release of outstanding commitments.

## Validation

Targeted Linux Docker tests run with the race detector for `resourcecontroller`,
`cfg`, shared `resourceadmission`, and shared `featureflags`. The controller tests
cover concurrent admission, exact fits, filesystem sharing/separation, lease
growth and ownership, delayed release, cleanup uncertainty, write reservations,
invalid samples, and disabled behavior. Linux sampling tests exercise real
directories and symlink aliases.

These tests do not prove KVM/NBD-backed lifecycle enforcement or real startup
reclaim. Those require a privileged runtime environment. API node-accounting and
placement race tests cover freshness, overflow, pending/success overlays,
out-of-order reports, attempt ownership, capability versus capacity, and sampled,
preferred and final-check routes. Independent API-view tests use two placement
contexts against one real controller, with barriers forcing stale idle reports
on both clients, for CPU, logical disk and operation-space contention.
Linux server race tests cover shared/separate snapshot destinations, delayed
sealing and upload, failed sealing with a rootfs writer still active,
pre-destructive replacement refusal, busy-checkpoint rollback, in-place refusal,
and harvest capacity refusal. Admission and cleanup tests pass ten repeated
race-enabled runs.

The root Docker sandbox run hangs in an existing NBD-backed constructor fixture:
rollback waits for a device path while NBD startup remains blocked. That fixture
requires working NBD device access, not merely UID 0. The full sandbox race suite
passes as an unprivileged Linux user, with privileged fixtures skipped. This does
not validate KVM/NBD lifecycles. ServiceInfo race tests pass ten repeated runs and
cover report presence, idle zeroes, in-flight and non-routable commitments, cleanup
release and uncertainty, invalid measurements, response isolation, and unchanged
legacy live-only fields. Test subprocess builds require `GOWORK=off` when the
parent uses a temporary workspace outside the source mount.

The recorded Linux race regression command exited 0 for `resourcecontroller`,
`cfg`, `server`, `service`, `factories`, shared `resourceadmission`, and shared
`featureflags`. API node accounting, placement, and API error-translation tests
passed ten consecutive race-enabled runs, with recorded exit 0. The server suite
checks all five refusal reasons and their `ErrorInfo` protobuf round trips.
The broader API short-mode race suite now passes for the orchestrator, node-manager,
and placement packages, including the Redis-backed winning-placement test. The
initial run failed because the test container lacked a Docker socket. The successful
runner mounts that socket, uses host networking, and sets
`TESTCONTAINERS_HOST_OVERRIDE=127.0.0.1`. This was a runner failure, not an admission
regression. Short mode still skips database integration tests; it does not replace
them.

Final checks have explicit results:

| Check | Result |
| --- | --- |
| Linux race regression for the seven packages listed above | Exit 0 |
| API orchestrator, node-manager and placement, `-race -short -timeout=120s` | Exit 0 |
| `go vet -p=1` across the changed API, orchestrator and shared packages, including sandbox | Exit 0 |
| All 77 configured linters on changed packages, with `--new-from-rev=HEAD` | Zero changed-line issues, exit 0 |
| `gofmt -l` on modified and untracked Go files | No output, exit 0 |
| Two `info.proto` regenerations through the existing generation directive | Unchanged SHA-256 sums for both generated files, exit 0 |
| `git diff --check` | Exit 0 |

The lint run uses the pinned `golangci/golangci-lint:v2.14.0` image and repository
configuration. `--new-from-rev=HEAD` excludes pre-existing findings; this is not a
claim that the entire repository has no lint findings. Bounded-memory attempts
previously timed out or were killed. The successful run uses `GOMAXPROCS=2`,
`GOMEMLIMIT=900MiB`, `GOGC=100`, `GOFLAGS=-p=1`, `--concurrency=1`, and a persistent
lint cache. No lint rules were disabled.

To reproduce the API short-mode result from the repository root on the same local
Docker setup:

```bash
docker run --rm --network host \
  -e DOCKER_HOST=unix:///var/run/docker.sock \
  -e TESTCONTAINERS_HOST_OVERRIDE=127.0.0.1 \
  -e GOMAXPROCS=2 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD":/src:ro \
  -v e2b-admission-gomod:/go/pkg/mod \
  -v e2b-admission-gobuild:/root/.cache/go-build \
  -w /tmp golang:1.26.8-bookworm sh -c '
    go work init \
      /src/packages/api /src/packages/auth /src/packages/clickhouse \
      /src/packages/client-proxy /src/packages/dashboard-api /src/packages/db \
      /src/packages/envd /src/packages/local-dev /src/packages/nomad-nodepool-apm \
      /src/packages/orchestrator /src/packages/shared /src/tests/integration &&
    cd /src &&
    GOWORK=/tmp/go.work go test -p=1 -race -short -timeout=120s \
      ./packages/api/internal/orchestrator \
      ./packages/api/internal/orchestrator/nodemanager \
      ./packages/api/internal/orchestrator/placement
  '
```

The temporary Go workspace avoids host-only paths without modifying the checked-in
workspace. To reproduce contract generation, run
`MISE_AUTO_INSTALL=false GOWORK=off go generate -run 'info.proto' generate.go`
from the orchestrator package with the pinned generation tools installed. The
narrow directive regenerates the changed contract without unrelated mock updates.
Run it twice and compare SHA-256 sums of `info.pb.go` and `info_grpc.pb.go`.

Neither admission switch has been enabled. The final diff contains only runtime,
API, contract, test and documentation changes. Publishing, deployment and
infrastructure configuration are unchanged.
