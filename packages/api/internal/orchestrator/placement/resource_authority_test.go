package placement

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/resourcecontroller"
	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

// Independent API replicas can both pass their local final checks against stale
// idle reports. Only the shared node controller may commit physical resources.
func TestResourceAdmissionIndependentAPIViews(t *testing.T) {
	t.Parallel()
	for _, limiting := range []string{"CPU", "logical disk", "operation space"} {
		t.Run(limiting, func(t *testing.T) {
			t.Parallel()
			cpus, total, available := uint64(4), uint64(1000), uint64(1000)
			switch limiting {
			case "CPU":
				cpus = 2
			case "logical disk":
				total, available = 200, 200
			case "operation space":
				available = 101
			}
			controller, err := resourcecontroller.New(resourcecontroller.Options{
				Policy:   resourceadmission.Policy{Enabled: true, CPUPercent: 100, DiskPercent: 80, HeadroomBytes: resourceadmission.MiB},
				CPUCount: cpus,
				Paths:    map[string]string{"rootfs": "cache", "build": "cache", "template": "cache"},
				Sample: func(string) (resourcecontroller.Sample, error) {
					return resourcecontroller.Sample{ID: "fs", TotalBytes: total * resourceadmission.MiB, AvailableBytes: available * resourceadmission.MiB}, nil
				},
			})
			require.NoError(t, err)
			report := controller.Report()
			arrived := make(chan struct{}, 2)
			proceed := make(chan struct{})
			defer close(proceed)
			leases := make(chan *resourcecontroller.Lease, 2)
			results := make(chan error, 2)
			for i := range 2 {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				node := admissionNode("same-physical-host", report, time.Now())
				node.SetSandboxClient(&nodemanager.MockSandboxClientCustom{CreateFunc: func() error {
					arrived <- struct{}{}
					select {
					case <-proceed:
					case <-ctx.Done():
						return ctx.Err()
					}
					lease, err := controller.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: 100 * resourceadmission.MiB})
					if err != nil {
						cancel()

						return status.Error(codes.ResourceExhausted, "node resource capacity unavailable, please retry")
					}
					leases <- lease

					return nil
				}})
				go func() {
					req := testSbxRequest(fmt.Sprintf("sandbox-%d", i))
					req.Sandbox.Vcpu, req.Sandbox.TotalDiskSizeMb = 2, 100
					_, err := PlaceSandbox(ctx, NewBestOfK(DefaultBestOfKConfig()), []*nodemanager.Node{node}, nil, req, Requirements{ResourceAdmission: true}, false, nil)
					results <- err
				}()
			}
			for range 2 {
				select {
				case <-arrived:
				case <-t.Context().Done():
					t.Fatal("placement did not reach create")
				}
			}
			// Two permits release both creates without sharing API accounting.
			proceed <- struct{}{}
			proceed <- struct{}{}
			successes := 0
			for range 2 {
				err := <-results
				if err == nil {
					successes++
				} else {
					require.ErrorAs(t, err, &NoNodesAvailableError{})
				}
			}
			require.Equal(t, 1, successes)
			require.Equal(t, uint64(2), controller.Report().CPUCommitted)
			_, err = controller.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: 100 * resourceadmission.MiB})
			require.Error(t, err, "capacity cannot return before the unclaimed reservation is released")
			lease := <-leases
			lease.ReleaseUnclaimed()
			lease.ReleaseUnclaimed()
			require.Zero(t, controller.Report().CPUCommitted)
			replacement, err := controller.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: 100 * resourceadmission.MiB})
			require.NoError(t, err)
			replacement.ReleaseUnclaimed()
		})
	}
}
