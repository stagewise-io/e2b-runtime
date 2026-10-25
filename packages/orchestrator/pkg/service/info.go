//go:build linux

package service

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/service/machineinfo"
	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// ServiceStatus bundles the service status with the time of its last change.
type ServiceStatus struct {
	Status    orchestratorinfo.ServiceInfoStatus
	ChangedAt time.Time
}

type ServiceInfo struct {
	ClientId  string
	ServiceId string

	SourceVersion string
	SourceCommit  string

	Startup      time.Time
	Roles        []orchestratorinfo.ServiceInfoRole
	Labels       []string
	MachineInfo  machineinfo.MachineInfo
	MaxSandboxes atomic.Int64

	status            ServiceStatus
	statusMu          sync.RWMutex
	outstandingWork   int64
	persistenceFailed bool
}

var serviceRolesMapper = map[cfg.ServiceType]orchestratorinfo.ServiceInfoRole{
	cfg.Orchestrator:    orchestratorinfo.ServiceInfoRole_Orchestrator,
	cfg.TemplateManager: orchestratorinfo.ServiceInfoRole_TemplateBuilder,
}

func (s *ServiceInfo) GetStatus() ServiceStatus {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()

	return s.status
}

// AdmitNewWork closes the stale-placement race by registering a start under the
// same lock as status changes. Only external starts use this gate; children of
// admitted work and operations on existing sandboxes must keep using TrackWork.
func (s *ServiceInfo) AdmitNewWork() (func(), error) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	if s.status.Status != orchestratorinfo.ServiceInfoStatus_Healthy {
		return nil, status.Error(codes.ResourceExhausted, "node is not accepting new work, please retry")
	}
	s.outstandingWork++

	return s.finishWork, nil
}

// RecordPersistenceFailure blocks retirement for this process incarnation. A
// finished upload is not necessarily durable. Healthy overrides cannot clear it.
func (s *ServiceInfo) RecordPersistenceFailure() {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	s.persistenceFailed = true
}

// retirementReportLocked requires statusMu. All fields describe the same cut.
// It is evidence for a supervised retirement, never permission to delete a host.
func (s *ServiceInfo) retirementReportLocked() *orchestratorinfo.RetirementReport {
	return &orchestratorinfo.RetirementReport{
		Version:           1,
		AdmissionClosed:   s.status.Status != orchestratorinfo.ServiceInfoStatus_Healthy,
		OutstandingWork:   uint64(s.outstandingWork),
		PersistenceFailed: s.persistenceFailed,
	}
}

// Child work must be registered before its parent releases ownership.
func (s *ServiceInfo) TrackWork() func() {
	s.statusMu.Lock()
	s.outstandingWork++
	s.statusMu.Unlock()

	return s.finishWork
}

func (s *ServiceInfo) finishWork() {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	s.outstandingWork--
}

func (s *ServiceInfo) OutstandingWork() int64 {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()

	return s.outstandingWork
}

func (s *ServiceInfo) SetStatus(ctx context.Context, status orchestratorinfo.ServiceInfoStatus) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	s.setStatus(ctx, status)
}

func (s *ServiceInfo) OverrideStatus(ctx context.Context, status orchestratorinfo.ServiceInfoStatus) bool {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	// Only process shutdown may enter ShuttingDown.
	if status == orchestratorinfo.ServiceInfoStatus_ShuttingDown {
		return false
	}

	if s.status.Status == orchestratorinfo.ServiceInfoStatus_Draining && status == orchestratorinfo.ServiceInfoStatus_Standby {
		return false
	}

	return s.setStatus(ctx, status)
}

func (s *ServiceInfo) setStatus(ctx context.Context, status orchestratorinfo.ServiceInfoStatus) bool {
	if s.status.Status == orchestratorinfo.ServiceInfoStatus_ShuttingDown && status != s.status.Status {
		return false
	}

	if s.status.Status != status {
		logger.L().Info(ctx, "Service status changed", zap.String("status", status.String()))
		s.status = ServiceStatus{Status: status, ChangedAt: time.Now()}
	}

	return true
}

func NewInfoContainer(clientId string, version string, commit string, instanceID string, machineInfo machineinfo.MachineInfo, config cfg.Config) *ServiceInfo {
	services := cfg.GetServices(config)
	serviceRoles := make([]orchestratorinfo.ServiceInfoRole, 0)

	for _, service := range services {
		if role, ok := serviceRolesMapper[service]; ok {
			serviceRoles = append(serviceRoles, role)
		}
	}

	startup := time.Now()
	serviceInfo := &ServiceInfo{
		ClientId:  clientId,
		ServiceId: instanceID,

		status: ServiceStatus{Status: orchestratorinfo.ServiceInfoStatus_Healthy, ChangedAt: startup},

		Startup:     startup,
		Roles:       serviceRoles,
		Labels:      config.NodeLabels,
		MachineInfo: machineInfo,

		SourceVersion: version,
		SourceCommit:  commit,
	}

	return serviceInfo
}
