// Package telemetry wires OpenTelemetry tracing and metrics (docs/operations/deployment-and-operations.md
// #observability). Spans and metrics are exported over OTLP/HTTP when an endpoint is configured;
// otherwise the global providers stay the no-op ones and nothing leaves the process. Trace
// context is propagated either way, so Quai joins traces started by a gateway.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
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
	// its /v1/traces unless the URL names another path, metrics to its /v1/metrics.
	endpoint, metrics := cfg.OTLPEndpoint, cfg.OTLPEndpoint
	if u, err := url.Parse(endpoint); err == nil {
		if strings.Trim(u.Path, "/") == "" {
			u.Path = "/v1/traces"
			endpoint = u.String()
		}
		u.Path = "/v1/metrics"
		metrics = u.String()
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}
	if network != nil {
		opts = append(opts, otlptracehttp.WithTLSClientConfig(network.TLSConfig()), otlptracehttp.WithProxy(network.ProxyRequest))
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}
	res := resource.NewSchemaless(attribute.String("service.name", cfg.ServiceName), attribute.String("service.version", version))
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)

	mopts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(metrics)}
	if network != nil {
		mopts = append(mopts, otlpmetrichttp.WithTLSClientConfig(network.TLSConfig()), otlpmetrichttp.WithProxy(network.ProxyRequest))
	}
	mexp, err := otlpmetrichttp.New(ctx, mopts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp)), sdkmetric.WithResource(res))
	otel.SetMeterProvider(mp)
	if err := use(mp); err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}
	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}
