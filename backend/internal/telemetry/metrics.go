package telemetry

import (
	"context"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics (docs/operations/deployment-and-operations.md#observability). They are recorded with
// the meter provider Setup installs; before, or without an endpoint, recording does nothing.

type instruments struct {
	fetch      metric.Float64Histogram
	webhookLag metric.Float64Histogram
	deliveries metric.Int64Counter
}

var current atomic.Pointer[instruments]

// use makes the instruments of mp the ones recorded with.
func use(mp metric.MeterProvider) error {
	m := mp.Meter("github.com/damhau/quai/backend")
	var in instruments
	var err error
	if in.fetch, err = m.Float64Histogram("quai.git.fetch.duration", metric.WithUnit("s"),
		metric.WithDescription("How long fetching a repository's mirror takes, by repository and outcome")); err != nil {
		return err
	}
	if in.webhookLag, err = m.Float64Histogram("quai.webhook.lag", metric.WithUnit("s"),
		metric.WithDescription("From a webhook's receipt to the end of its processing, by repository")); err != nil {
		return err
	}
	if in.deliveries, err = m.Int64Counter("quai.deliveries",
		metric.WithDescription("Deliveries that ended, by repository, delivery mode and outcome (applied, failed, bypassed)")); err != nil {
		return err
	}
	current.Store(&in)
	return nil
}

func outcome(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// Fetched records a fetch of a repository's mirror.
func Fetched(ctx context.Context, repository string, d time.Duration, err error) {
	if in := current.Load(); in != nil {
		in.fetch.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("repository", repository), attribute.String("outcome", outcome(err))))
	}
}

// WebhookProcessed records a webhook delivery processed lag after its receipt.
func WebhookProcessed(ctx context.Context, repository string, lag time.Duration) {
	if in := current.Load(); in != nil {
		in.webhookLag.Record(ctx, lag.Seconds(), metric.WithAttributes(attribute.String("repository", repository)))
	}
}

// Delivered records a delivery that ended: applied, failed or bypassed (merged without Quai's
// approval).
func Delivered(ctx context.Context, repository, mode, result string) {
	if in := current.Load(); in != nil {
		in.deliveries.Add(ctx, 1, metric.WithAttributes(attribute.String("repository", repository), attribute.String("mode", mode),
			attribute.String("outcome", result)))
	}
}
