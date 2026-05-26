package otel_test

import (
	"context"
	"testing"
	"time"

	"github.com/gomooth/locker"
	lockerotel "github.com/gomooth/locker/otel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetricsProviderIncrementCounter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer mp.Shutdown(context.Background())

	metrics, err := lockerotel.NewMetricsProvider(mp)
	require.Nil(t, err)

	metrics.IncrementCounter(locker.MetricAcquire, locker.AttrKey("test-key"))
	metrics.IncrementCounter(locker.MetricAcquire, locker.AttrKey("test-key"))
	metrics.IncrementCounter(locker.MetricAcquireFail, locker.AttrKey("test-key"))

	var rm metricdata.ResourceMetrics
	err = reader.Collect(context.Background(), &rm)
	require.Nil(t, err)

	t.Logf("Number of scopes: %d", len(rm.ScopeMetrics))
	for i, sm := range rm.ScopeMetrics {
		t.Logf("Scope %d metrics: %d", i, len(sm.Metrics))
		for j, m := range sm.Metrics {
			t.Logf("Metric %d: %s", j, m.Name)
		}
	}

	assert.Equal(t, 1, len(rm.ScopeMetrics), "should have 1 metric scope")

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == locker.MetricAcquire {
				sum := m.Data.(metricdata.Sum[int64])
				assert.Equal(t, int64(2), sum.DataPoints[0].Value, "lock.acquire should be incremented twice")
			}
		}
	}
}

func TestMetricsProviderRecordDuration(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer mp.Shutdown(context.Background())

	metrics, err := lockerotel.NewMetricsProvider(mp)
	require.Nil(t, err)

	metrics.RecordDuration(locker.MetricAcquireDur, 100*time.Millisecond, locker.AttrKey("test-key"))

	var rm metricdata.ResourceMetrics
	err = reader.Collect(context.Background(), &rm)
	require.Nil(t, err)

	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == locker.MetricAcquireDur {
				found = true
				hist := m.Data.(metricdata.Histogram[float64])
				assert.Equal(t, uint64(1), hist.DataPoints[0].Count, "should record one duration sample")
				assert.Greater(t, hist.DataPoints[0].Sum, 0.0, "duration sum should be positive")
			}
		}
	}
	assert.True(t, found, "should find lock.acquire.duration metric")
}

func TestMetricsProviderError(t *testing.T) {
	// Passing nil as MeterProvider should return an error
	_, err := lockerotel.NewMetricsProvider(nil)
	assert.NotNil(t, err, "should return error for nil MeterProvider")
}
