package telemetry

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"

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
