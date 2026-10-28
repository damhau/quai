package netx

import (
	"bytes"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/http/httpproxy"
)

// server is an HTTPS server whose certificate no system trusts; its certificate in PEM.
func server(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(srv.Close)
	return srv, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func TestCABundle(t *testing.T) {
	srv, ca := server(t)
	none, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	if none.GitBundle() != nil {
		t.Error("a bundle for git without extra certificates")
	}
	if _, err := none.Client().Get(srv.URL); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("an unknown CA was trusted: %v", err)
	}

	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, append([]byte("# Internal CA\n"), ca...), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := New(file)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := n.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// Git's bundle is the system's certificates, then the extra ones.
	system := filepath.Join(t.TempDir(), "system.pem")
	if err := os.WriteFile(system, []byte("SYSTEM\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := build(ca, []string{filepath.Join(t.TempDir(), "missing.pem"), system}, &httpproxy.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b.GitBundle(), []byte("SYSTEM\n")) || !bytes.HasSuffix(b.GitBundle(), ca) {
		t.Errorf("git's bundle:\n%s", b.GitBundle())
	}

	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")})
	for name, content := range map[string][]byte{"empty": nil, "not PEM": []byte("hello"), "a key": append(ca, key...),
		"a broken certificate": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")})} {
		if err := os.WriteFile(file, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(file); err == nil || !strings.Contains(err.Error(), "CA bundle") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := New(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Error("a missing file")
	}
}

func TestProxy(t *testing.T) {
	n, err := build(nil, nil, &httpproxy.Config{HTTPSProxy: "http://proxy.example.com:3128", NoProxy: "devops.acme.ch,.internal"})
	if err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]string{
		"https://dev.azure.com/org":         "http://proxy.example.com:3128",
		"https://devops.acme.ch/tfs":        "",
		"https://git.corp.internal/x":       "",
		"http://plain.example.com/":         "", // only HTTPS_PROXY is set
		"https://127.0.0.1:8443/loopback/x": "",
	} {
		str := func(u *url.URL) string {
			if u == nil {
				return ""
			}
			return u.String()
		}
		u, _ := url.Parse(target)
		got, err := n.Proxy(u)
		if err != nil {
			t.Fatal(err)
		}
		if str(got) != want {
			t.Errorf("%s: proxy %q, want %q", target, str(got), want)
		}
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if viaTransport, _ := n.Transport().Proxy(req); str(viaTransport) != want {
			t.Errorf("%s: the transport uses %q", target, str(viaTransport))
		}
	}
	if n.TLSConfig().InsecureSkipVerify {
		t.Error("verification is off")
	}
}
