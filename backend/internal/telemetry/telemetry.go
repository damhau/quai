// Package telemetry wires OpenTelemetry tracing (docs/operations/deployment-and-operations.md
// #observability). Spans are exported over OTLP/HTTP when an endpoint is configured; otherwise
// the global tracer provider stays the no-op one and nothing leaves the process. Trace context
// is propagated either way, so Quai joins traces started by a gateway.
package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
)

// Setup installs the propagator and, with an endpoint, an exporting tracer provider, which
// reaches the collector through network (nil: the defaults). The returned function flushes and
// stops it.
func Setup(ctx context.Context, cfg config.Telemetry, version string, network *netx.Network) (shutdown func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if cfg.OTLPEndpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	// Like OTEL_EXPORTER_OTLP_ENDPOINT, the setting is the collector's base URL: traces go to
	// its /v1/traces unless the URL names another path.
	endpoint := cfg.OTLPEndpoint
	if u, err := url.Parse(endpoint); err == nil && strings.Trim(u.Path, "/") == "" {
		u.Path = "/v1/traces"
		endpoint = u.String()
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}
	if network != nil {
		opts = append(opts, otlptracehttp.WithTLSClientConfig(network.TLSConfig()), otlptracehttp.WithProxy(network.ProxyRequest))
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", cfg.ServiceName),
			attribute.String("service.version", version),
		)),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}
