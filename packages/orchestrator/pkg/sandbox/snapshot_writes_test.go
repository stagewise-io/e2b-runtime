//go:build linux

package sandbox

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/build"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

func TestSnapshotFailureSeparatesArtifactAndCleanup(t *testing.T) {
	t.Parallel()
	artifactErr := errors.New("artifact failed")
	cleanupErr := errors.New("cleanup failed")
	for _, err := range []error{nil, cleanupErr} {
		failure := snapshotFailure(artifactErr, err)
		require.ErrorIs(t, failure, artifactErr)
		require.Equal(t, err, snapshotCleanupError(t.Context(), failure))
	}
	require.ErrorIs(t, snapshotCleanupError(t.Context(), artifactErr), artifactErr)
}

func TestSnapshotWritesJoinTeardownAfterArtifactFailure(t *testing.T) {
	t.Parallel()
	for _, cleanupFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleaned", true: "uncertain"}[cleanupFails], func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				cleanup := NewCleanup()
				teardown := make(chan struct{})
				promise := utils.NewSetOnce[build.Diff]()
				artifactErr := errors.New("artifact publication failed")
				cleanupErr := errors.New("cache close failed")
				cleanup.snapshotWriters.start(func() error {
					_ = promise.SetError(artifactErr)
					<-teardown
					if cleanupFails {
						return cleanupErr
					}

					return nil
				})
				cleanup.snapshotWriters.seal()
				snapshot := &Snapshot{
					cleanup:        cleanup,
					RootfsDiff:     build.NewDeferredDiff("test-rootfs", 4096, promise),
					MemorySnapshot: MemorySnapshot{Diff: &build.NoDiff{}},
				}
				done := make(chan error, 1)
				go func() { done <- snapshot.WaitForWrites(t.Context()) }()
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("released before producer teardown completed")
				default:
				}
				close(teardown)
				synctest.Wait()
				if cleanupFails {
					require.ErrorIs(t, <-done, cleanupErr)
				} else {
					require.NoError(t, <-done, "failed artifact is safe once producer cleanup completes")
				}
			})
		})
	}
}

func TestSnapshotWritesCanceledWaitDoesNotProveCompletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	snapshot := &Snapshot{RootfsDiff: &build.NoDiff{}}
	require.ErrorIs(t, snapshot.WaitForWrites(ctx), context.Canceled)
}
