package otel_test

import (
	"context"
	"testing"

	"github.com/gomooth/locker"
	lockerotel "github.com/gomooth/locker/otel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTracerProviderStartSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)))
	defer tp.Shutdown(context.Background())

	tracer := lockerotel.NewTracerProvider(tp)

	ctx, span := tracer.StartSpan(context.Background(), locker.SpanLock, locker.AttrKey("test-key"), locker.AttrOwner("test-owner"))
	require.NotNil(t, span)
	require.NotNil(t, ctx)

	span.SetAttributes(locker.AttrReason("test"))
	span.End()

	spans := exporter.GetSpans()
	assert.Equal(t, 1, len(spans), "should export one span")
	assert.Equal(t, locker.SpanLock, spans[0].Name)

	attrMap := make(map[string]string)
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value.AsString()
	}
	assert.Equal(t, "test-key", attrMap["lock.key"])
	assert.Equal(t, "test-owner", attrMap["lock.owner"])
	assert.Equal(t, "test", attrMap["lock.reason"])
}

func TestTracerProviderSpanParentChild(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)))
	defer tp.Shutdown(context.Background())

	tracer := lockerotel.NewTracerProvider(tp)

	// Create parent span
	ctx, parentSpan := tracer.StartSpan(context.Background(), locker.SpanLock, locker.AttrKey("test-key"))
	require.NotNil(t, parentSpan)

	// Create child span from parent context
	_, childSpan := tracer.StartSpan(ctx, locker.SpanRenew, locker.AttrKey("test-key"))
	require.NotNil(t, childSpan)

	childSpan.End()
	parentSpan.End()

	spans := exporter.GetSpans()
	assert.Equal(t, 2, len(spans), "should export two spans")

	var parentSpanID, childParentID string
	for _, s := range spans {
		if s.Name == locker.SpanRenew {
			childParentID = s.Parent.SpanID().String()
		}
		if s.Name == locker.SpanLock {
			parentSpanID = s.SpanContext.SpanID().String()
		}
	}
	assert.Equal(t, parentSpanID, childParentID, "Renew span should be child of Lock span")
}
