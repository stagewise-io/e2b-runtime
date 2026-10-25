//go:build linux

package service

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
)

func TestAdmissionRequiresHealthyStatus(t *testing.T) {
	t.Parallel()

	for value, name := range orchestratorinfo.ServiceInfoStatus_name {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			info := &ServiceInfo{}
			info.SetStatus(t.Context(), orchestratorinfo.ServiceInfoStatus(value))
			done, err := info.AdmitNewWork()
			if value != int32(orchestratorinfo.ServiceInfoStatus_Healthy) {
				require.Equal(t, codes.ResourceExhausted, status.Code(err))
				require.Nil(t, done)
				require.Zero(t, info.OutstandingWork())

				return
			}
			require.NoError(t, err)
			require.Equal(t, int64(1), info.OutstandingWork())
			info.SetStatus(t.Context(), orchestratorinfo.ServiceInfoStatus_Draining)
			childDone := info.TrackWork()
			done()
			require.Equal(t, int64(1), info.OutstandingWork(), "drain must allow children of admitted work")
			childDone()
			require.Zero(t, info.OutstandingWork())
			require.True(t, info.OverrideStatus(t.Context(), orchestratorinfo.ServiceInfoStatus_Healthy))
			done, err = info.AdmitNewWork()
			require.NoError(t, err)
			done()
		})
	}
}

func TestDrainRacesWithAdmission(t *testing.T) {
	t.Parallel()

	for range 50 {
		info := &ServiceInfo{}
		start, release := make(chan struct{}), make(chan struct{})
		var attempted, finished sync.WaitGroup
		var accepted atomic.Int64
		for range 32 {
			attempted.Add(1)
			finished.Go(func() {
				<-start
				done, err := info.AdmitNewWork()
				if err == nil {
					accepted.Add(1)
				}
				attempted.Done()
				<-release
				if err == nil {
					done()
				}
			})
		}
		close(start)
		info.SetStatus(t.Context(), orchestratorinfo.ServiceInfoStatus_Draining)
		attempted.Wait()
		info.statusMu.RLock()
		report := info.retirementReportLocked()
		info.statusMu.RUnlock()
		// Release before assertions so a failed test cannot leak blocked workers.
		close(release)
		finished.Wait()
		require.True(t, report.GetAdmissionClosed())
		require.EqualValues(t, accepted.Load(), report.GetOutstandingWork())
		done, err := info.AdmitNewWork()
		require.Equal(t, codes.ResourceExhausted, status.Code(err))
		require.Nil(t, done)
		require.Zero(t, info.OutstandingWork())
	}
}

func TestRetirementFailureSurvivesHealthyOverride(t *testing.T) {
	t.Parallel()

	info := &ServiceInfo{}
	server := NewInfoService(info, sandbox.NewSandboxesMap(), metrics.NewHostMetrics())
	fetch := func() *orchestratorinfo.RetirementReport {
		t.Helper()
		response, err := server.ServiceInfo(t.Context(), &emptypb.Empty{})
		require.NoError(t, err)
		require.NotNil(t, response.GetRetirement())
		require.Equal(t, response.GetOutstandingWork(), response.GetRetirement().GetOutstandingWork())
		require.Equal(t, response.GetServiceStatus() != orchestratorinfo.ServiceInfoStatus_Healthy, response.GetRetirement().GetAdmissionClosed())

		return response.GetRetirement()
	}
	idle := fetch()
	require.EqualValues(t, 1, idle.GetVersion())
	require.False(t, idle.GetAdmissionClosed())
	require.False(t, idle.GetPersistenceFailed())
	done, err := info.AdmitNewWork()
	require.NoError(t, err)
	info.SetStatus(t.Context(), orchestratorinfo.ServiceInfoStatus_Draining)
	require.EqualValues(t, 1, fetch().GetOutstandingWork())
	info.RecordPersistenceFailure()
	done()
	failed := fetch()
	require.Zero(t, failed.GetOutstandingWork())
	require.True(t, failed.GetPersistenceFailed())
	require.True(t, info.OverrideStatus(t.Context(), orchestratorinfo.ServiceInfoStatus_Healthy))
	require.True(t, fetch().GetPersistenceFailed())
	info.SetStatus(t.Context(), orchestratorinfo.ServiceInfoStatus_Draining)
	require.True(t, fetch().GetPersistenceFailed())
	require.False(t, idle.GetPersistenceFailed(), "earlier responses must be immutable")
}
