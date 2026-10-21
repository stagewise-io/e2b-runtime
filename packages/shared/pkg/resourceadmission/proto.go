package resourceadmission

import (
	"slices"

	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
)

func (r Report) Proto() *orchestratorinfo.ResourceAdmissionReport {
	p := &orchestratorinfo.ResourceAdmissionReport{Version: r.Version, Enabled: r.Enabled, Valid: r.Valid, Uncertain: r.Uncertain, CpuLimit: r.CPULimit, CpuCommitted: r.CPUCommitted}
	for _, fs := range r.Filesystems {
		p.Filesystems = append(p.Filesystems, &orchestratorinfo.ResourceAdmissionFilesystem{Id: fs.ID, Roles: slices.Clone(fs.Roles), Valid: fs.Valid, TotalBytes: fs.TotalBytes, AvailableBytes: fs.AvailableBytes, LogicalLimitBytes: fs.LogicalLimitBytes, LogicalCommittedBytes: fs.LogicalCommittedBytes, WriteReservedBytes: fs.WriteReservedBytes, HeadroomBytes: fs.HeadroomBytes})
	}

	return p
}

// FromProto copies the report. Malformed policy is never interpreted as idle.
func FromProto(p *orchestratorinfo.ResourceAdmissionReport) *Report {
	if p == nil {
		return nil
	}
	r := &Report{Version: p.GetVersion(), Enabled: p.GetEnabled(), Valid: p.GetValid(), Uncertain: p.GetUncertain(), CPULimit: p.GetCpuLimit(), CPUCommitted: p.GetCpuCommitted()}
	seen := make(map[string]bool)
	roles := make(map[string]bool)
	for _, fs := range p.GetFilesystems() {
		if fs == nil {
			r.Valid = false

			continue
		}
		f := Filesystem{ID: fs.GetId(), Roles: slices.Clone(fs.GetRoles()), Valid: fs.GetValid(), TotalBytes: fs.GetTotalBytes(), AvailableBytes: fs.GetAvailableBytes(), LogicalLimitBytes: fs.GetLogicalLimitBytes(), LogicalCommittedBytes: fs.GetLogicalCommittedBytes(), WriteReservedBytes: fs.GetWriteReservedBytes(), HeadroomBytes: fs.GetHeadroomBytes()}
		if f.ID == "" || seen[f.ID] || f.TotalBytes == 0 || f.AvailableBytes > f.TotalBytes || f.LogicalLimitBytes > f.TotalBytes || f.HeadroomBytes == 0 || !f.Valid {
			r.Valid = false
		}
		for _, role := range f.Roles {
			if role == "" || roles[role] {
				r.Valid = false
			}
			roles[role] = true
		}
		seen[f.ID] = true
		r.Filesystems = append(r.Filesystems, f)
	}
	root, ok := r.Rootfs()
	if !ok || root.LogicalLimitBytes == 0 || r.CPULimit == 0 {
		r.Valid = false
	}

	return r
}
