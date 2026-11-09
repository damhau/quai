package siem_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	syslog "github.com/leodido/go-syslog/v4"
	"github.com/leodido/go-syslog/v4/octetcounting"
	"github.com/leodido/go-syslog/v4/rfc5424"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

// The receivers the export is checked with (docs/plan/phase-4-backlog.md#test-infrastructure):
// a syslog listener over TCP and TLS whose messages an RFC 5424 parser reads, and an OTLP/HTTP
// logs receiver decoding OpenTelemetry's protobufs. Splunk's collector is internal/splunktest.

// authority is a CA the test makes, and the certificate of 127.0.0.1 and localhost it signs.
type authority struct {
	pem    []byte // the CA's certificate
	server *tls.Config
}

func newAuthority(t *testing.T) authority {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Quai test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "siem.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return authority{
		pem:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		server: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12},
	}
}

// syslogReceiver is a syslog listener over TCP, or TLS with a configuration: it reads messages
// framed by octet counting (RFC 6587) and parses them as RFC 5424 says, MSG included, then closes
// the connection when the sender closed its side.
type syslogReceiver struct {
	t    *testing.T
	tls  *tls.Config
	addr string

	mu       sync.Mutex
	ln       net.Listener
	messages []*rfc5424.SyslogMessage
	errs     []error
	reset    int // resets the next connection after reading this many messages, when positive
	conns    sync.WaitGroup
}

func newSyslogReceiver(t *testing.T, cfg *tls.Config) *syslogReceiver {
	t.Helper()
	r := &syslogReceiver{t: t, tls: cfg, addr: "127.0.0.1:0"}
	r.start()
	t.Cleanup(r.stop)
	return r
}

// start listens, on the address it listened on before if any.
func (r *syslogReceiver) start() {
	r.t.Helper()
	ln, err := net.Listen("tcp", r.addr)
	if err != nil {
		r.t.Fatal(err)
	}
	r.mu.Lock()
	r.ln, r.addr = ln, ln.Addr().String()
	r.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r.conns.Add(1)
			go r.serve(c.(*net.TCPConn))
		}
	}()
}

// stop stops listening: connections are refused.
func (r *syslogReceiver) stop() {
	r.mu.Lock()
	ln := r.ln
	r.ln = nil
	r.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	r.conns.Wait()
}

// resetAfter makes the receiver reset the next connection after reading n messages, as one
// that dies would.
func (r *syslogReceiver) resetAfter(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reset = n
}

func (r *syslogReceiver) serve(raw *net.TCPConn) {
	defer r.conns.Done()
	var conn net.Conn = raw
	if r.tls != nil {
		conn = tls.Server(raw, r.tls)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	r.mu.Lock()
	reset := r.reset
	r.reset = 0
	r.mu.Unlock()
	read, gone := 0, false
	p := octetcounting.NewParser(syslog.WithMachineOptions(rfc5424.WithCompliantMsg()), syslog.WithListener(func(res *syslog.Result) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if gone {
			return // what the parser makes of the connection it lost
		}
		if res.Error != nil {
			r.errs = append(r.errs, res.Error)
			return
		}
		r.messages = append(r.messages, res.Message.(*rfc5424.SyslogMessage))
		read++
		if read == reset {
			_ = raw.SetLinger(0)
			_ = raw.Close()
			gone = true
		}
	}))
	p.Parse(conn)
}

// received returns the messages parsed so far, and the parse errors.
func (r *syslogReceiver) received() ([]*rfc5424.SyslogMessage, []error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*rfc5424.SyslogMessage(nil), r.messages...), append([]error(nil), r.errs...)
}

// otlpReceiver is an OTLP/HTTP logs receiver over TLS: it decodes OpenTelemetry's protobufs
// and keeps the requests.
type otlpReceiver struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*collogs.ExportLogsServiceRequest
	headers  []http.Header
	down     bool  // answers 503
	reject   int64 // answers a partial success rejecting this many records, once
	errs     []error
}

func newOTLPReceiver(t *testing.T, cfg *tls.Config) *otlpReceiver {
	t.Helper()
	r := &otlpReceiver{}
	r.Server = httptest.NewUnstartedServer(http.HandlerFunc(r.serve))
	r.TLS = cfg
	r.StartTLS()
	t.Cleanup(r.Close)
	return r
}

func (r *otlpReceiver) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.Method != http.MethodPost || req.URL.Path != "/v1/logs" || req.Header.Get("Content-Type") != "application/x-protobuf" {
		r.errs = append(r.errs, errors.New(req.Method+" "+req.URL.Path+" "+req.Header.Get("Content-Type")))
		http.Error(w, "unsupported", http.StatusBadRequest)
		return
	}
	if r.down {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	b, err := io.ReadAll(req.Body)
	if err != nil {
		r.errs = append(r.errs, err)
		return
	}
	var in collogs.ExportLogsServiceRequest
	if err := proto.Unmarshal(b, &in); err != nil {
		r.errs = append(r.errs, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.requests = append(r.requests, &in)
	r.headers = append(r.headers, req.Header.Clone())
	var out collogs.ExportLogsServiceResponse
	if r.reject > 0 {
		out.PartialSuccess = &collogs.ExportLogsPartialSuccess{RejectedLogRecords: r.reject, ErrorMessage: "too large"}
		r.reject = 0
	}
	body, _ := proto.Marshal(&out)
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(body)
}

func (r *otlpReceiver) setDown(down bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.down = down
}

// records returns the log records received, in order, with the request each came in.
func (r *otlpReceiver) records() ([]*logs.LogRecord, []*collogs.ExportLogsServiceRequest, []error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*logs.LogRecord
	for _, req := range r.requests {
		for _, rl := range req.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				out = append(out, sl.GetLogRecords()...)
			}
		}
	}
	return out, append([]*collogs.ExportLogsServiceRequest(nil), r.requests...), append([]error(nil), r.errs...)
}

// pemOf returns a certificate in PEM.
func pemOf(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
