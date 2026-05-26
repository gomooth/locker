package otel

import (
	"context"
	"fmt"
	"time"

	"github.com/gomooth/locker"
	"go.opentelemetry.io/otel/metric"
)

// MetricsProvider adapts locker.Metrics to OpenTelemetry
type MetricsProvider struct {
	counters  map[string]metric.Int64Counter
	durations map[string]metric.Float64Histogram
	strict    bool
}

// MetricsOption 配置 MetricsProvider 的选项
type MetricsOption func(*MetricsProvider)

// WithStrictMetrics 启用严格校验模式，遇到未知 metric 名时 panic。
// 仅建议在测试环境启用，用于捕获 locker 包新增 metric 未同步到 otel 适配器的情况。
func WithStrictMetrics() MetricsOption {
	return func(p *MetricsProvider) {
		p.strict = true
	}
}

// NewMetricsProvider creates a locker.Metrics backed by an OpenTelemetry MeterProvider
func NewMetricsProvider(mp metric.MeterProvider, opts ...MetricsOption) (*MetricsProvider, error) {
	if mp == nil {
		return nil, fmt.Errorf("meter provider is nil")
	}
	m := mp.Meter("github.com/gomooth/locker")
	p := &MetricsProvider{
		counters:  make(map[string]metric.Int64Counter),
		durations: make(map[string]metric.Float64Histogram),
	}

	for _, opt := range opts {
		opt(p)
	}

	counterNames := []string{
		locker.MetricAcquire, locker.MetricAcquireFail,
		locker.MetricFencingTokenFail,
		locker.MetricRelease,
		locker.MetricRenew, locker.MetricRenewFail,
		locker.MetricLost,
	}
	for _, name := range counterNames {
		c, err := m.Int64Counter(name, metric.WithDescription(fmt.Sprintf("locker counter: %s", name)))
		if err != nil {
			return nil, fmt.Errorf("create counter %q: %w", name, err)
		}
		p.counters[name] = c
	}

	durationNames := []string{locker.MetricAcquireDur, locker.MetricRenewDur}
	for _, name := range durationNames {
		h, err := m.Float64Histogram(name,
			metric.WithDescription(fmt.Sprintf("locker duration: %s", name)),
			metric.WithUnit("s"))
		if err != nil {
			return nil, fmt.Errorf("create histogram %q: %w", name, err)
		}
		p.durations[name] = h
	}

	return p, nil
}

func (p *MetricsProvider) IncrementCounter(name string, attrs ...locker.Attr) {
	if c, ok := p.counters[name]; ok {
		c.Add(context.Background(), 1, metric.WithAttributes(attrsToOtel(attrs...)...))
		return
	}
	if p.strict {
		panic(fmt.Sprintf("locker: unknown counter metric %q", name))
	}
}

func (p *MetricsProvider) RecordDuration(name string, d time.Duration, attrs ...locker.Attr) {
	if h, ok := p.durations[name]; ok {
		h.Record(context.Background(), d.Seconds(), metric.WithAttributes(attrsToOtel(attrs...)...))
		return
	}
	if p.strict {
		panic(fmt.Sprintf("locker: unknown duration metric %q", name))
	}
}
