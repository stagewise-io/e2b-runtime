//go:build linux

package service

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/resourcecontroller"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

func TestServiceInfoResourceAdmissionPresence(t *testing.T) {
	t.Parallel()

	for _, injected := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "disabled"}[injected], func(t *testing.T) {
			t.Parallel()

			var controller *resourcecontroller.Controller
			if injected {
				var err error
				controller, err = resourcecontroller.New(resourcecontroller.Options{})
				require.NoError(t, err)
			}
			server := NewInfoService(&ServiceInfo{}, sandbox.NewSandboxesMap(), metrics.NewHostMetrics(), controller)
			response, err := server.ServiceInfo(t.Context(), &emptypb.Empty{})
			require.NoError(t, err)
			if !injected {
				require.Nil(t, response.GetResourceAdmission())

				return
			}
			require.NotNil(t, response.GetResourceAdmission())
			require.EqualValues(t, resourceadmission.Version, response.GetResourceAdmission().GetVersion())
			require.False(t, response.GetResourceAdmission().GetEnabled())
		})
	}
}

func TestServiceInfoResourceAdmissionTracksPhysicalLifecycles(t *testing.T) {
	t.Parallel()

	measurementFails := false
	controller, err := resourcecontroller.New(resourcecontroller.Options{
		Policy:   resourceadmission.Policy{Enabled: true, CPUPercent: 250, DiskPercent: 80, HeadroomBytes: 10},
		CPUCount: 8,
		Paths:    map[string]string{resourcecontroller.Rootfs: "rootfs", resourcecontroller.Build: "build", resourcecontroller.Template: "template"},
		Sample: func(string) (resourcecontroller.Sample, error) {
			if measurementFails {
				return resourcecontroller.Sample{}, errors.New("measurement failed")
			}

			return resourcecontroller.Sample{ID: "shared-device", TotalBytes: 1000, AvailableBytes: 900}, nil
		},
	})
	require.NoError(t, err)
	server := NewInfoService(&ServiceInfo{}, sandbox.NewSandboxesMap(), metrics.NewHostMetrics(), controller)
	fetch := func() *orchestratorinfo.ResourceAdmissionReport {
		t.Helper()
		response, infoErr := server.ServiceInfo(t.Context(), &emptypb.Empty{})
		require.NoError(t, infoErr)
		// Physical commitments must not change the legacy live-only fields.
		require.Zero(t, response.GetMetricCpuAllocated())
		require.Zero(t, response.GetMetricDiskAllocatedBytes())
		require.Zero(t, response.GetMetricSandboxesRunning())
		require.NotNil(t, response.GetResourceAdmission())

		return response.GetResourceAdmission()
	}

	idle := fetch()
	require.True(t, idle.GetEnabled())
	require.True(t, idle.GetValid())
	require.EqualValues(t, 20, idle.GetCpuLimit())
	require.Zero(t, idle.GetCpuCommitted())
	require.Len(t, idle.GetFilesystems(), 1)

	lease, err := controller.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: 100})
	require.NoError(t, err)
	require.NoError(t, lease.Claim("physical-lifecycle"))
	starting := fetch()
	require.EqualValues(t, 2, starting.GetCpuCommitted())
	require.EqualValues(t, 100, starting.GetFilesystems()[0].GetLogicalCommittedBytes())
	require.EqualValues(t, 100, starting.GetFilesystems()[0].GetWriteReservedBytes())

	lease.Started()
	stopping := fetch()
	require.EqualValues(t, 2, stopping.GetCpuCommitted())
	require.EqualValues(t, 100, stopping.GetFilesystems()[0].GetLogicalCommittedBytes())
	require.Zero(t, stopping.GetFilesystems()[0].GetWriteReservedBytes())
	// Responses own their slices, including the role lists.
	stopping.Filesystems[0].Roles[0] = "mutated-response"
	require.NotContains(t, fetch().GetFilesystems()[0].GetRoles(), "mutated-response")

	measurementFails = true
	invalid := fetch()
	require.False(t, invalid.GetValid())
	require.EqualValues(t, 2, invalid.GetCpuCommitted())
	require.EqualValues(t, 100, invalid.GetFilesystems()[0].GetLogicalCommittedBytes())
	measurementFails = false

	lease.Release(nil)
	released := fetch()
	require.True(t, released.GetValid())
	require.Zero(t, released.GetCpuCommitted())
	require.Zero(t, released.GetFilesystems()[0].GetLogicalCommittedBytes())
	// Earlier reports are immutable snapshots, not views into the ledger.
	require.EqualValues(t, 2, starting.GetCpuCommitted())
	require.EqualValues(t, 100, starting.GetFilesystems()[0].GetWriteReservedBytes())

	failed, err := controller.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: 100})
	require.NoError(t, err)
	failed.Release(errors.New("cleanup failed"))
	uncertain := fetch()
	require.True(t, uncertain.GetUncertain())
	require.EqualValues(t, 2, uncertain.GetCpuCommitted())
	require.EqualValues(t, 100, uncertain.GetFilesystems()[0].GetLogicalCommittedBytes())
}
