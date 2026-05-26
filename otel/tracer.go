package otel

import (
	"context"

	"github.com/gomooth/locker"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// TracerProvider adapts locker.Tracer to OpenTelemetry
type TracerProvider struct {
	tp trace.TracerProvider
}

// NewTracerProvider creates a locker.Tracer backed by an OpenTelemetry TracerProvider
func NewTracerProvider(tp trace.TracerProvider) *TracerProvider {
	return &TracerProvider{tp: tp}
}

func (p *TracerProvider) StartSpan(ctx context.Context, name string, attrs ...locker.Attr) (context.Context, locker.Span) {
	tr := p.tp.Tracer("github.com/gomooth/locker")
	opts := []trace.SpanStartOption{
		trace.WithAttributes(attrsToOtel(attrs...)...),
	}
	ctx, otelSpan := tr.Start(ctx, name, opts...)
	return ctx, &otelSpanAdapter{span: otelSpan}
}

type otelSpanAdapter struct {
	span trace.Span
}

func (s *otelSpanAdapter) End()                               { s.span.End() }
func (s *otelSpanAdapter) SetAttributes(attrs ...locker.Attr) { s.span.SetAttributes(attrsToOtel(attrs...)...) }
func (s *otelSpanAdapter) RecordError(err error)              { s.span.RecordError(err) }

func attrsToOtel(attrs ...locker.Attr) []attribute.KeyValue {
	result := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		result = append(result, attribute.String(a.Key, a.Value))
	}
	return result
}
