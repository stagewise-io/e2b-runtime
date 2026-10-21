package resourcecontroller

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

var rejectionReasons = [...]resourceadmission.Reason{resourceadmission.CPU, resourceadmission.LogicalDisk, resourceadmission.PhysicalDisk, resourceadmission.Measurement, resourceadmission.Uncertain}

func (c *Controller) recordRefusalLocked(err error) {
	var refusal *resourceadmission.RefusalError
	if !errors.As(err, &refusal) {
		return
	}
	for _, reason := range rejectionReasons {
		if refusal.Reason == reason {
			c.rejections[reason]++
			return
		}
	}
}

// RegisterMetrics exports only fixed resource/reason labels. Filesystem paths,
// device IDs, sandbox IDs and customer identities never become metric labels.
// Devices are summed here for observation only, never for admission.
func (c *Controller) RegisterMetrics(meter metric.Meter) (metric.Registration, error) {
	committed, err := meter.Float64ObservableGauge("orchestrator.resource_admission.committed")
	if err != nil {
		return nil, err
	}
	rejected, err := meter.Float64ObservableCounter("orchestrator.resource_admission.rejections")
	if err != nil {
		return nil, err
	}
	valid, err := meter.Int64ObservableGauge("orchestrator.resource_admission.valid")
	if err != nil {
		return nil, err
	}
	return meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		c.mu.Lock()
		r := c.reportLocked()
		counts := make(map[resourceadmission.Reason]uint64, len(c.rejections))
		for reason, count := range c.rejections {
			counts[reason] = count
		}
		c.mu.Unlock()
		var logical, writes float64
		for _, fs := range r.Filesystems {
			logical += float64(fs.LogicalCommittedBytes)
			writes += float64(fs.WriteReservedBytes)
		}
		for resource, value := range map[string]float64{"cpu": float64(r.CPUCommitted), "logical_disk_bytes": logical, "write_bytes": writes} {
			obs.ObserveFloat64(committed, value, metric.WithAttributes(attribute.String("resource", resource)))
		}
		for _, reason := range rejectionReasons {
			obs.ObserveFloat64(rejected, float64(counts[reason]), metric.WithAttributes(attribute.String("reason", string(reason))))
		}
		var healthy int64
		if r.Enabled && r.Valid && !r.Uncertain {
			healthy = 1
		}
		obs.ObserveInt64(valid, healthy)
		return nil
	}, committed, rejected, valid)
}
