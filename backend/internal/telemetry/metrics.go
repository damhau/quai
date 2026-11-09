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
	auditCps   metric.Int64Counter
	auditVer   metric.Int64Counter
	auditExp   metric.Int64Counter
	notified   metric.Int64Counter
}

var current atomic.Pointer[instruments]

// AuditExportLag is how far a destination of the audit log's export is behind.
type AuditExportLag struct {
	Destination string
	// Pending is how many events the destination has not taken yet.
	Pending int64
	// Lag is how long ago the database recorded the oldest of them; zero when there is none.
	Lag time.Duration
}

// exportLags reads the export's lag, at each collection (ObserveAuditExport).
var exportLags atomic.Pointer[func(context.Context) ([]AuditExportLag, error)]

// ObserveAuditExport makes lags the source of the audit export's lag and pending events, which
// the metrics read at each collection: from the database, so that every worker reports the same
// values, whichever exported last.
func ObserveAuditExport(lags func(context.Context) ([]AuditExportLag, error)) {
	exportLags.Store(&lags)
}

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
	if in.auditCps, err = m.Int64Counter("quai.audit.checkpoints",
		metric.WithDescription("Audit checkpoint runs: signing the latest event and committing it to the audit repository, by outcome (ok, error)")); err != nil {
		return err
	}
	if in.auditVer, err = m.Int64Counter("quai.audit.verifications",
		metric.WithDescription("Daily verifications of the audit log, by outcome (ok, broken, error)")); err != nil {
		return err
	}
	if in.auditExp, err = m.Int64Counter("quai.audit.exports",
		metric.WithDescription("Runs of the audit log's export to a SIEM, by destination and outcome (ok, error)")); err != nil {
		return err
	}
	if in.notified, err = m.Int64Counter("quai.notifications",
		metric.WithDescription("Notifications, by channel and outcome: sent, retried (an attempt failed), failed (no attempt left), skipped (nothing to send)")); err != nil {
		return err
	}
	lag, err := m.Float64ObservableGauge("quai.audit.export.lag", metric.WithUnit("s"),
		metric.WithDescription("How long ago the database recorded the oldest audit event a SIEM has not taken yet, by destination; 0 when it has them all"))
	if err != nil {
		return err
	}
	pending, err := m.Int64ObservableGauge("quai.audit.export.pending",
		metric.WithDescription("Audit events a SIEM has not taken yet, by destination"))
	if err != nil {
		return err
	}
	if _, err := m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		read := exportLags.Load()
		if read == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		lags, err := (*read)(ctx)
		if err != nil {
			return err
		}
		for _, l := range lags {
			dest := metric.WithAttributes(attribute.String("destination", l.Destination))
			o.ObserveFloat64(lag, l.Lag.Seconds(), dest)
			o.ObserveInt64(pending, l.Pending, dest)
		}
		return nil
	}, lag, pending); err != nil {
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

// AuditCheckpointed records a checkpoint run of the audit log.
func AuditCheckpointed(ctx context.Context, err error) {
	if in := current.Load(); in != nil {
		in.auditCps.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome(err))))
	}
}

// AuditVerified records a verification of the audit log: ok, broken or error (it could not run).
func AuditVerified(ctx context.Context, result string) {
	if in := current.Load(); in != nil {
		in.auditVer.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", result)))
	}
}

// AuditExported records a run of the audit log's export to a destination.
func AuditExported(ctx context.Context, destination string, err error) {
	if in := current.Load(); in != nil {
		in.auditExp.Add(ctx, 1, metric.WithAttributes(attribute.String("destination", destination), attribute.String("outcome", outcome(err))))
	}
}

// Notified records what became of a notification on a channel: sent, retried (an attempt failed),
// failed (no attempt left) or skipped (nothing to send: no address, the request moved on).
func Notified(ctx context.Context, channel, result string) {
	if in := current.Load(); in != nil {
		in.notified.Add(ctx, 1, metric.WithAttributes(attribute.String("channel", channel), attribute.String("outcome", result)))
	}
}
