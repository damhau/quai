package siem

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/secrets"
)

// otlpSender sends OpenTelemetry logs over OTLP/HTTP, in binary protobuf
// (https://opentelemetry.io/docs/specs/otlp/#otlphttp): a request per batch, to the collector's
// /v1/logs.
type otlpSender struct {
	url      string
	headers  map[string]string // name: secret reference
	client   *http.Client
	secrets  *secrets.Store
	resource *resource.Resource
}

// Scope is the instrumentation scope of the records.
const Scope = "quai.audit"

// Attributes of each log record besides its body, the record in JSON.
const (
	AttrSeq    = "quai.audit.seq"
	AttrHash   = "quai.audit.hash"
	AttrAction = "quai.audit.action"
)

func newOTLP(c config.OTLPExport, client *http.Client, sec *secrets.Store, hostname, service, version string) *otlpSender {
	endpoint := c.Endpoint
	// Like OTEL_EXPORTER_OTLP_ENDPOINT, the base URL: logs go to its /v1/logs unless it names a
	// path.
	if u, err := url.Parse(endpoint); err == nil && strings.Trim(u.Path, "/") == "" {
		u.Path = "/v1/logs"
		endpoint = u.String()
	}
	headers := map[string]string{}
	for name, h := range c.Headers {
		headers[name] = h.Ref
	}
	attrs := []*common.KeyValue{str("service.name", service), str("host.name", hostname)}
	if version != "" {
		attrs = append(attrs, str("service.version", version))
	}
	return &otlpSender{url: endpoint, headers: headers, client: client, secrets: sec, resource: &resource.Resource{Attributes: attrs}}
}

func str(k, v string) *common.KeyValue {
	return &common.KeyValue{Key: k, Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: v}}}
}

func (s *otlpSender) send(ctx context.Context, records []audit.Record) error {
	body, err := s.request(records, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	names := make([]string, 0, len(s.headers))
	for name := range s.headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v, err := s.secrets.Resolve(ctx, s.headers[name])
		if err != nil {
			return fmt.Errorf("the header %s: %w", name, err)
		}
		req.Header.Set(name, v.Reveal())
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the collector answered %s", resp.Status)
	}
	var out collogs.ExportLogsServiceResponse
	if err := proto.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("the collector's answer: %w", err)
	}
	if ps := out.GetPartialSuccess(); ps.GetRejectedLogRecords() > 0 {
		return &rejected{msg: fmt.Sprintf("the collector rejected %d of %d records: %q", ps.GetRejectedLogRecords(), len(records), ps.GetErrorMessage())}
	}
	return nil
}

// request is the OTLP request for records: one resource, Quai, one scope, a log record each.
func (s *otlpSender) request(records []audit.Record, now time.Time) ([]byte, error) {
	lr := make([]*logs.LogRecord, len(records))
	for i, r := range records {
		j, err := recordJSON(r)
		if err != nil {
			return nil, err
		}
		at, err := time.Parse(audit.AtFormat, r.At)
		if err != nil {
			return nil, err
		}
		sev, text := logs.SeverityNumber_SEVERITY_NUMBER_INFO2, "NOTICE"
		if severity(r.Action) == severityWarning {
			sev, text = logs.SeverityNumber_SEVERITY_NUMBER_WARN, "WARNING"
		}
		lr[i] = &logs.LogRecord{
			TimeUnixNano:         uint64(at.UnixNano()),  // #nosec G115 -- an event's time is after 1970
			ObservedTimeUnixNano: uint64(now.UnixNano()), // #nosec G115 -- now is after 1970
			SeverityNumber:       sev,
			SeverityText:         text,
			Body:                 &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: string(j)}},
			Attributes: []*common.KeyValue{
				{Key: AttrSeq, Value: &common.AnyValue{Value: &common.AnyValue_IntValue{IntValue: r.Seq}}},
				str(AttrHash, r.Hash),
				str(AttrAction, r.Action),
			},
		}
	}
	return proto.Marshal(&collogs.ExportLogsServiceRequest{ResourceLogs: []*logs.ResourceLogs{{
		Resource:  s.resource,
		ScopeLogs: []*logs.ScopeLogs{{Scope: &common.InstrumentationScope{Name: Scope}, LogRecords: lr}},
	}}})
}
