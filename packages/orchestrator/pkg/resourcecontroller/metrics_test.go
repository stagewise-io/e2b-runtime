package resourcecontroller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

func TestAdmissionTelemetry(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t, false)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	registration, err := c.RegisterMetrics(provider.Meter("test"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, registration.Unregister()) })
	lease, err := c.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: 100})
	require.NoError(t, err)
	_, err = c.Acquire(resourceadmission.Resources{CPUs: 21, DiskBytes: 1})
	reason(t, err, resourceadmission.CPU)
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	require.Len(t, data.ScopeMetrics, 1)
	for _, m := range data.ScopeMetrics[0].Metrics {
		switch m.Name {
		case "orchestrator.resource_admission.committed":
			points := m.Data.(metricdata.Gauge[float64]).DataPoints
			require.Len(t, points, 3)
			for _, point := range points {
				for _, attr := range point.Attributes.ToSlice() {
					require.Equal(t, "resource", string(attr.Key))
				}
			}
		case "orchestrator.resource_admission.rejections":
			points := m.Data.(metricdata.Sum[float64]).DataPoints
			require.Len(t, points, len(rejectionReasons))
			var total float64
			for _, point := range points {
				total += point.Value
				require.Equal(t, 1, point.Attributes.Len())
			}
			require.Equal(t, float64(1), total)
		}
	}
	lease.Release(nil)
}
