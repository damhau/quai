package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/damhau/quai/backend/internal/config"
)

func TestWithoutEndpoint(t *testing.T) {
	shutdown, err := Setup(context.Background(), config.Telemetry{ServiceName: "quai"}, "dev")
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
	shutdown, err := Setup(context.Background(), config.Telemetry{OTLPEndpoint: collector.URL, ServiceName: "quai"}, "dev")
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
