//go:build linux

package metrics

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
)

func TestSandboxObserverWithoutCollector(t *testing.T) {
	t.Parallel()

	// Telemetry reads its endpoint at process initialization. A subprocess also
	// proves that standard OTLP defaults cannot re-enable network export.
	if os.Getenv("E2B_TEST_OBSERVER_WITHOUT_COLLECTOR") != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSandboxObserverWithoutCollector$", "-test.v")
		cmd.Env = append(os.Environ(),
			"E2B_TEST_OBSERVER_WITHOUT_COLLECTOR=1",
			"OTEL_COLLECTOR_GRPC_ENDPOINT=",
			"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:1",
		)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)

		return
	}

	observer, err := NewSandboxObserver(t.Context(), "test-node", "orchestrator", "test-commit", "test-version", "test-instance", sandbox.NewSandboxesMap())
	require.NoError(t, err)
	t.Cleanup(func() { _ = observer.Close(context.WithoutCancel(t.Context())) })
	require.NotNil(t, observer.registration, "local warning checks must remain registered")

	gauge, err := observer.meter.Int64ObservableGauge("observer.local")
	require.NoError(t, err)
	var callbacks atomic.Int64
	registration, err := observer.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		callbacks.Add(1)
		o.ObserveInt64(gauge, 1)

		return nil
	}, gauge)
	require.NoError(t, err)
	t.Cleanup(func() { _ = registration.Unregister() })

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, observer.meterProvider.ForceFlush(ctx), "an absent collector must not cause network retries")
	require.Equal(t, int64(1), callbacks.Load(), "local checks must run even without network export")
	require.NoError(t, observer.Close(ctx), "an absent collector must not make retirement fail")
}

type observerExporter struct {
	mu            sync.Mutex
	exported      []string
	shutdownCalls int
	shutdownError error
	stall         bool
}

func (*observerExporter) Temporality(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}

func (*observerExporter) Aggregation(kind sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(kind)
}

func (e *observerExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	if e.stall {
		<-ctx.Done()

		return ctx.Err()
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			e.exported = append(e.exported, m.Name)
		}
	}

	return nil
}

func (*observerExporter) ForceFlush(context.Context) error { return nil }

func (e *observerExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdownCalls++

	return e.shutdownError
}

func TestSandboxObserverCloseStopsReader(t *testing.T) {
	t.Parallel()

	exporter := &observerExporter{}
	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(time.Hour))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.WithoutCancel(t.Context())) })
	meter := provider.Meter("observer-test")
	counter, err := meter.Int64Counter("observer.pending")
	require.NoError(t, err)
	counter.Add(t.Context(), 1)
	gauge, err := meter.Int64ObservableGauge("observer.callback")
	require.NoError(t, err)
	var callbacks atomic.Int64
	registration, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		callbacks.Add(1)
		o.ObserveInt64(gauge, 1)

		return nil
	}, gauge)
	require.NoError(t, err)

	observer := &SandboxObserver{meterProvider: provider, registration: registration}
	require.NoError(t, observer.Close(t.Context()))
	assert.Zero(t, callbacks.Load(), "unregister callbacks before final collection")
	require.ErrorIs(t, reader.Collect(t.Context(), &metricdata.ResourceMetrics{}), sdkmetric.ErrReaderShutdown)
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	assert.Contains(t, exporter.exported, "observer.pending", "shutdown must flush through the provider")
	assert.Equal(t, 1, exporter.shutdownCalls)
}

func TestSandboxObserverClosePreservesErrors(t *testing.T) {
	t.Parallel()

	shutdownErr := errors.New("exporter shutdown failed")
	exporter := &observerExporter{shutdownError: shutdownErr}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
	observer := &SandboxObserver{meterProvider: provider}
	require.ErrorIs(t, observer.Close(t.Context()), shutdownErr, "real shutdown errors must remain visible to retirement")
}

func TestSandboxObserverCloseBoundsStalledExport(t *testing.T) {
	t.Parallel()

	exporter := &observerExporter{stall: true}
	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(time.Hour))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	counter, err := provider.Meter("observer-test").Int64Counter("observer.stalled")
	require.NoError(t, err)
	counter.Add(t.Context(), 1)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	observer := &SandboxObserver{meterProvider: provider}
	started := time.Now()
	require.ErrorIs(t, observer.Close(ctx), context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 3*time.Second)
	require.ErrorIs(t, reader.Collect(t.Context(), &metricdata.ResourceMetrics{}), sdkmetric.ErrReaderShutdown)
}
