# Preview retirement telemetry failure

## Evidence

The preview retirement for instance `i-07f6d261ec5df8e87` failed on 2026-10-27.
Read-only inspection of its pod and supervisor marker established:

- The runtime source was `69e6d8df0ff3027b74b93018663124bc587512a4`.
- `OTEL_COLLECTOR_GRPC_ENDPOINT` was absent on the failed host and on a ready peer.
- At 12:34:19 UTC, runtime shutdown reached the sandbox observer.
- OTLP export logged `delegating_resolver: invalid target address "": missing address`.
- At 12:34:38 UTC, observer shutdown returned `context deadline exceeded`.
- The runtime logged `Stopping orchestrator, success: false` and exited with code 1.
- The supervisor marker recorded `admissionClosed: true`, `outstandingWork: 0`,
  `persistenceFailed: false`, `retirementExit: true`, and `cleanExit: false`.
- The controller rejected the marker with `invalid-supervisor-evidence`.

The supervisor remained running, so zero container restarts did not establish runtime health.
The failed host was unready. The controller's rejection was correct: an error exit is not a
clean supervised shutdown, even when the last retirement report showed no work.

## Cause

The shared telemetry client treats an absent collector endpoint as disabled telemetry.
`NewSandboxObserver` bypassed that decision and always constructed an OTLP exporter.
Its periodic reader attempted exports to an empty address. During retirement, exporter
shutdown waited on the failing export and returned a timeout. The orchestrator propagates
closer errors into its exit status, which made otherwise completed drain evidence insufficient.

The observer also retained only the exporter, not its meter provider. Closing the exporter
directly left the owning periodic reader running. That ownership error could race shutdown
with collection and export.

## Correction

With no collector endpoint, the observer now uses an in-process discard exporter. It retains
normal metric aggregation and the periodic callback because that callback also emits local
OOM, CPU, and memory warnings. Disabling the whole observer or dropping SDK aggregation would
remove those checks. A configured collector still uses the existing OTLP exporter and
sandbox-specific temporality.

The observer owns and closes its meter provider. Close unregisters its sandbox callback,
stops the reader, and shuts down the exporter under the existing deadline. Failed observer
initialization also closes a successfully constructed provider.

No controller, supervisor, retirement evidence contract, exit-code policy, or deployment
selection changes are part of this correction. Genuine collector shutdown failures still
propagate. This correction does not promise that every future retirement can complete.

## Regression checks

The Linux metrics suite covers:

- An absent collector in a fresh subprocess, even with a standard OTLP endpoint configured.
  Local callbacks still run, flush succeeds without network retries, and close succeeds.
- Unregistering the callback before final collection, flushing pending measurements, closing
  the exporter, and leaving the periodic reader shut down.
- Preserving exporter shutdown errors for the caller.
- Bounding a stalled final export by the caller's deadline and stopping the reader.

The initial regression against the unmodified observer confirmed it created a real metric
pipeline despite the absent collector. The final regression tests the required behavior
without removing local warning checks.

Local validation used Go 1.26.8 on Linux. Race-enabled metrics and shared telemetry tests,
factory compilation, metrics vet, formatting, and diff whitespace checks passed.
The repository-configured golangci-lint 2.14.0 reported one existing `epoch-naming` issue in
`packages/orchestrator/pkg/metrics/host.go:229`, outside this change. It reported no issues in
the changed metrics code or new tests. These checks do not replace live rollover acceptance.

## Rollout and recovery boundaries

This is a source correction only. It does not restart a pod, replace a host, clear a failed
operation, remove retirement fencing, publish an image, or deploy either environment.

Production PR #424 remains blocked on preview recovery and acceptance. Production must retain
`OnDelete`, with retirement, rollover, and scale-in disabled.

A merged correction needs a new pinned runtime bundle and combined image, followed by an
approved preview promotion. The already-failed host exited the old runtime. Publishing a new
image cannot rewrite its marker or retroactively establish a clean exit. Recover that host
only through the existing operator-recovery procedure after checking workload and persistence
state. Never clear the failed evidence just to let the controller continue.

Before production promotion, record a successful preview retirement and rollover on the fixed
runtime, including exact image and source identities, controller transitions, zero remaining
work, no persistence failure, a clean supervised exit, and final fleet readiness.
