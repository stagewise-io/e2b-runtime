//go:build linux

package server

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

// admitCheckpointReplacement runs while the old VM is still live and intact.
func (s *Server) admitCheckpointReplacement(ctx context.Context, sbx *sandbox.Sandbox) (context.Context, func(), error) {
	if s.sandboxFactory == nil || !s.sandboxFactory.Admission.Enabled() {
		return ctx, func() {}, nil
	}
	rootfs, err := sbx.Template.Rootfs()
	if err != nil {
		return ctx, nil, err
	}
	size, err := rootfs.Size(ctx)
	if err != nil {
		return ctx, nil, err
	}
	if size <= 0 || sbx.Config.Vcpu <= 0 {
		return ctx, nil, status.Error(codes.InvalidArgument, "invalid replacement resources")
	}
	lease, err := s.sandboxFactory.Admission.Acquire(resourceadmission.Resources{CPUs: uint64(sbx.Config.Vcpu), DiskBytes: uint64(size)})
	if err != nil {
		return ctx, nil, resourceAdmissionStatus(err)
	}

	return sandbox.WithResourceLease(ctx, lease), lease.ReleaseUnclaimed, nil
}

// resourceAdmissionStatus keeps host capacity details out of customer messages.
func resourceAdmissionStatus(err error) error {
	return resourceAdmissionStatusWithCode(err, codes.ResourceExhausted, "node resource capacity unavailable, please retry")
}

// A replacement refusal after destruction is fatal: ResourceExhausted would
// make the API restore Running even though the original VM no longer exists.
func resourceAdmissionStatusWithCode(err error, code codes.Code, message string) error {
	var refusal *resourceadmission.RefusalError
	if !errors.As(err, &refusal) {
		return err
	}
	s := status.New(code, message)
	detailed, detailErr := s.WithDetails(&errdetails.ErrorInfo{Domain: "e2b.orchestrator", Reason: "RESOURCE_ADMISSION_" + strings.ToUpper(string(refusal.Reason))})
	if detailErr != nil {
		return s.Err()
	}

	return detailed.Err()
}
