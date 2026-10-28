// Package netx is the network setup every outgoing connection of Quai shares
// (docs/design/12-git-connectors.md#network-server): the CA certificates trusted in addition to
// the system's (an internal CA, typically) and the HTTP proxy. Git gets the same setup through
// gitx, the platform APIs, the identity provider and the trace exporter through Client and
// TLSConfig. Certificate verification is never turned off.
package netx

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// Network is the trusted certificates and the proxy of outgoing connections.
type Network struct {
	roots  *x509.CertPool
	bundle []byte // the system's certificates and the extra ones, in PEM; nil without extra ones
	proxy  func(*url.URL) (*url.URL, error)
}

// systemBundles are where Linux distributions keep their trusted certificates, as Go looks for
// them (crypto/x509/root_linux.go).
var systemBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian, Ubuntu, Gentoo
	"/etc/pki/tls/certs/ca-bundle.crt",                  // Fedora, RHEL 6
	"/etc/ssl/ca-bundle.pem",                            // OpenSUSE
	"/etc/pki/tls/cacert.pem",                           // OpenELEC
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // CentOS, RHEL 7
	"/etc/ssl/cert.pem",                                 // Alpine
}

// New returns the network setup: the certificates of caFile (PEM; none when empty) trusted in
// addition to the system's, and the proxy from the environment (HTTPS_PROXY, HTTP_PROXY,
// NO_PROXY).
func New(caFile string) (*Network, error) {
	var extra []byte
	if caFile != "" {
		var err error
		if extra, err = os.ReadFile(caFile); err != nil { // #nosec G304 -- the file the operator configured
			return nil, fmt.Errorf("CA bundle: %w", err)
		}
		if err := checkPEM(extra); err != nil {
			return nil, fmt.Errorf("CA bundle %s: %w", caFile, err)
		}
	}
	return build(extra, systemBundles, httpproxy.FromEnvironment())
}

func build(extra []byte, system []string, proxy *httpproxy.Config) (*Network, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	n := &Network{roots: roots, proxy: proxy.ProxyFunc()}
	if len(extra) == 0 {
		return n, nil
	}
	if err := checkPEM(extra); err != nil {
		return nil, fmt.Errorf("CA bundle: %w", err)
	}
	roots.AppendCertsFromPEM(extra)
	// Git takes one bundle file (http.sslCAInfo), which replaces its default: give it the
	// system's certificates as well, so that public hosts still verify.
	var b bytes.Buffer
	if path := os.Getenv("SSL_CERT_FILE"); path != "" {
		system = append([]string{path}, system...)
	}
	for _, path := range system {
		if sys, err := os.ReadFile(path); err == nil { // #nosec G304 -- the system's bundle
			b.Write(sys)
			b.WriteString("\n")
			break
		}
	}
	b.Write(extra)
	n.bundle = b.Bytes()
	return n, nil
}

// checkPEM checks that b holds certificates, and only certificates (text between them, which
// bundles often have, is ignored).
func checkPEM(b []byte) error {
	n := 0
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("a %s block where certificates are expected", block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("not a PEM file of certificates")
	}
	return nil
}

// GitBundle returns the certificates git must trust, in PEM, or nil when git's default suffices.
func (n *Network) GitBundle() []byte { return n.bundle }

// Proxy returns the proxy to reach u through, or nil to connect directly.
func (n *Network) Proxy(u *url.URL) (*url.URL, error) { return n.proxy(u) }

// TLSConfig verifies servers against the system's certificates and the extra ones.
func (n *Network) TLSConfig() *tls.Config {
	return &tls.Config{RootCAs: n.roots, MinVersion: tls.VersionTLS12}
}

// Transport is an HTTP transport with the trusted certificates and the proxy.
func (n *Network) Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = n.TLSConfig()
	t.Proxy = n.ProxyRequest
	return t
}

// ProxyRequest is Proxy for an HTTP request, as http.Transport takes it.
func (n *Network) ProxyRequest(r *http.Request) (*url.URL, error) { return n.proxy(r.URL) }

// Client is an HTTP client with the trusted certificates and the proxy, and a timeout.
func (n *Network) Client() *http.Client {
	return &http.Client{Transport: n.Transport(), Timeout: time.Minute}
}
