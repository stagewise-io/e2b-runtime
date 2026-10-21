package resourceadmission

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
)

func TestReportPresence(t *testing.T) {
	t.Parallel()
	require.Nil(t, FromProto(nil))
	for _, enabled := range []bool{false, true} {
		p := &orchestratorinfo.ServiceInfoResponse{ResourceAdmission: &orchestratorinfo.ResourceAdmissionReport{Version: Version, Enabled: enabled, Valid: true, CpuLimit: 20, Filesystems: []*orchestratorinfo.ResourceAdmissionFilesystem{{Id: "dev", Roles: []string{"rootfs"}, Valid: true, TotalBytes: 100, AvailableBytes: 90, LogicalLimitBytes: 80, HeadroomBytes: 10}}}}
		b, err := proto.Marshal(p)
		require.NoError(t, err)
		var decoded orchestratorinfo.ServiceInfoResponse
		require.NoError(t, proto.Unmarshal(b, &decoded))
		r := FromProto(decoded.GetResourceAdmission())
		require.NotNil(t, r)
		require.True(t, r.Valid)
		require.Equal(t, enabled, r.Enabled)
		require.Zero(t, r.CPUCommitted)
		decoded.GetResourceAdmission().GetFilesystems()[0].GetRoles()[0] = "changed"
		require.Equal(t, "rootfs", r.Filesystems[0].Roles[0])
	}
	require.False(t, FromProto(&orchestratorinfo.ResourceAdmissionReport{Version: Version, Enabled: true, Valid: true}).Valid)
	require.False(t, FromProto(&orchestratorinfo.ResourceAdmissionReport{Version: Version, Enabled: true, Valid: false}).Valid)
}

func TestMalformedReports(t *testing.T) {
	t.Parallel()
	mutations := []func(*orchestratorinfo.ResourceAdmissionReport){
		func(p *orchestratorinfo.ResourceAdmissionReport) { p.CpuLimit = 0 },
		func(p *orchestratorinfo.ResourceAdmissionReport) { p.Valid = false },
		func(p *orchestratorinfo.ResourceAdmissionReport) { p.Filesystems = append(p.GetFilesystems(), nil) },
		func(p *orchestratorinfo.ResourceAdmissionReport) { p.GetFilesystems()[0].AvailableBytes = 101 },
		func(p *orchestratorinfo.ResourceAdmissionReport) { p.GetFilesystems()[0].LogicalLimitBytes = 101 },
		func(p *orchestratorinfo.ResourceAdmissionReport) { p.GetFilesystems()[0].HeadroomBytes = 0 },
		func(p *orchestratorinfo.ResourceAdmissionReport) { p.GetFilesystems()[0].Id = "" },
		func(p *orchestratorinfo.ResourceAdmissionReport) { p.GetFilesystems()[0].Valid = false },
		func(p *orchestratorinfo.ResourceAdmissionReport) {
			p.Filesystems = append(p.GetFilesystems(), &orchestratorinfo.ResourceAdmissionFilesystem{Id: "other", Roles: []string{"rootfs"}, Valid: true, TotalBytes: 100, AvailableBytes: 90, LogicalLimitBytes: 80, HeadroomBytes: 10})
		},
	}
	for _, mutate := range mutations {
		p := &orchestratorinfo.ResourceAdmissionReport{Version: Version, Enabled: true, Valid: true, CpuLimit: 20, Filesystems: []*orchestratorinfo.ResourceAdmissionFilesystem{{Id: "dev", Roles: []string{"rootfs"}, Valid: true, TotalBytes: 100, AvailableBytes: 90, LogicalLimitBytes: 80, HeadroomBytes: 10}}}
		mutate(p)
		r := FromProto(p)
		require.False(t, r.Valid)
		require.Error(t, r.Fit(Resources{}, Resources{}))
	}
}
