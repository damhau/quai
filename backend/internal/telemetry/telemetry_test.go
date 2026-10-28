package telemetry

import (
	"context"
	"encoding/pem"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
)

func TestWithoutEndpoint(t *testing.T) {
	shutdown, err := Setup(context.Background(), config.Telemetry{ServiceName: "quai"}, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Error(err)
	}
	if otel.GetTextMapPropagator().Fields() == nil {
		t.Error("no propagator was installed")
	}
}

func TestExportsToTheEndpoint(t *testing.T) {
	var got atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/traces" {
			got.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	shutdown, err := Setup(context.Background(), config.Telemetry{OTLPEndpoint: collector.URL, ServiceName: "quai"}, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "work")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Load() == 0 {
		t.Error("no spans reached the collector")
	}
}

// TestExportsOverTLS: the exporter trusts the collector's certificate through network.caFile.
func TestExportsOverTLS(t *testing.T) {
	var got atomic.Int32
	collector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		got.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: collector.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	network, err := netx.New(ca)
	if err != nil {
		t.Fatal(err)
	}
	shutdown, err := Setup(context.Background(), config.Telemetry{OTLPEndpoint: collector.URL, ServiceName: "quai"}, "dev", network)
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "work")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Load() == 0 {
		t.Error("no spans reached the collector")
	}
}

// TestMetrics: the fetch, webhook and delivery metrics are recorded with their attributes, and
// exported to the collector's /v1/metrics.
func TestMetrics(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	if err := use(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { current.Store(nil) })
	Fetched(ctx, "infra-live", 1500*time.Millisecond, nil)
	Fetched(ctx, "infra-live", time.Second, errors.New("unreachable"))
	WebhookProcessed(ctx, "infra-live", 200*time.Millisecond)
	Delivered(ctx, "infra-live", "pull-request", "applied")
	Delivered(ctx, "infra-live", "pull-request", "bypassed")
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					got[m.Name] += int(p.Count)
				}
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					got[m.Name] += int(p.Value)
				}
			}
		}
	}
	want := map[string]int{"quai.git.fetch.duration": 2, "quai.webhook.lag": 1, "quai.deliveries": 2}
	if !maps.Equal(got, want) {
		t.Errorf("metrics = %v, want %v", got, want)
	}

	var posted atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/metrics" {
			posted.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	shutdown, err := Setup(ctx, config.Telemetry{OTLPEndpoint: collector.URL, ServiceName: "quai"}, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	Delivered(ctx, "infra-live", "direct", "applied")
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if posted.Load() == 0 {
		t.Error("no metrics reached the collector")
	}
}
