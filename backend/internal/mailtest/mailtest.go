// Package mailtest gives tests a real SMTP server, Mailpit (docs/engineering/testing.md): its
// release, pinned and checked against its SHA-256, downloaded once into the user cache directory,
// started on free local ports for one test, with a certificate of a CA made for that test. Its
// API reads what was sent. It needs no Docker, so it runs inside the agents' sandbox and on CI.
package mailtest

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Version is the Mailpit release, the one the compose stack runs.
const Version = "1.32.0"

// archives are the release archives' checksums, from the release's checksums.txt.
var archives = map[string]string{
	"linux-amd64":  "4b5993c978e8604c70d43927e2ad103b311bba3b1822fa0cf1d4ee4ec890a93c",
	"linux-arm64":  "e1e45d1dc72bd497fb262a1e4100178eee057c42c5fc36d143eb8621769e1137",
	"darwin-amd64": "d7f3d0964cd2ea4c2a7d10660a12e02876718e67d88ced390d9cd90350a453da",
	"darwin-arm64": "23709bd727f20da2290de6a2a40c4685c48a3127586d77370efd1a3a595f9ef6",
}

// binaries are the checksums of the binaries the archives hold, for the cached copy.
var binaries = map[string]string{
	"linux-amd64":  "262cfa28fae4554be198d1fea5ef83c0f8299317876d658623711c629a8edec8",
	"linux-arm64":  "0e39f7ce4de3c2a4e939dc5fcfad355221c0bc412c62f13888e20fde1de7b3a7",
	"darwin-amd64": "a83b4758c788e14c632de00e85f4ef0cde4ff1b3e44d2354fa5d45bab8f6065f",
	"darwin-arm64": "a2f3371333d7e5e463040b1c6b766c2905af5833ac3c5bd0aee6adce40305f83",
}

// What a server encrypts SMTP with.
const (
	STARTTLS = "starttls" // STARTTLS, which it requires before a message
	TLS      = "tls"      // TLS from the first byte
	Plain    = "plain"    // nothing: it does not offer STARTTLS
)

// Options configures a server.
type Options struct {
	// Transport is STARTTLS (the default), TLS or Plain.
	Transport string
	// Username and Password, when set, are the only credentials it accepts, and it requires them.
	Username, Password string
}

// Server is a running Mailpit.
type Server struct {
	Host   string // 127.0.0.1
	Port   int    // SMTP's
	API    string // http://127.0.0.1:port, its web UI and API
	CAFile string // the PEM of the CA that signed its certificate, for network.caFile
}

// Start starts a Mailpit for a test, stopped when it ends.
//
// Its ports are free when chosen, not when Mailpit binds them: a server another test starts at the
// same moment can take one in between, and Mailpit then exits. Until it does, the other server can
// answer in its place: on 2026-11-09 another test's Mailpit answered the API, and the SMTP client
// read EOF from this one as it exited. So it is ready only once the API reports its own database
// and its SMTP port greets, and a start that lost a port is retried on others.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	bin, err := binary()
	if err != nil {
		t.Fatalf("mailtest: %v", err)
	}
	switch opts.Transport {
	case "", STARTTLS, TLS, Plain:
	default:
		t.Fatalf("mailtest: unknown transport %q", opts.Transport)
	}
	for attempt := 1; ; attempt++ {
		s, err := start(t, bin, opts)
		if err == nil {
			return s
		}
		if !errors.Is(err, errPortTaken) || attempt == 5 {
			t.Fatalf("mailtest: %v", err)
		}
	}
}

var errPortTaken = errors.New("a port was taken before mailpit bound it")

func start(t testing.TB, bin string, opts Options) (*Server, error) {
	dir := t.TempDir()
	s := &Server{Host: "127.0.0.1", Port: freePort(t), CAFile: filepath.Join(dir, "ca.pem")}
	apiPort := freePort(t)
	s.API = "http://127.0.0.1:" + strconv.Itoa(apiPort)
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := certificates(s.CAFile, cert, key); err != nil {
		return nil, err
	}
	database := filepath.Join(dir, "mailpit.db")
	args := []string{"--disable-version-check", "--quiet", "--enable-chaos", "--smtp-disable-rdns", "--smtp-strict-rfc-headers",
		"--database", database,
		"--listen", "127.0.0.1:" + strconv.Itoa(apiPort), "--smtp", "127.0.0.1:" + strconv.Itoa(s.Port)}
	switch opts.Transport {
	case "", STARTTLS:
		args = append(args, "--smtp-tls-cert", cert, "--smtp-tls-key", key, "--smtp-require-starttls")
	case TLS:
		args = append(args, "--smtp-tls-cert", cert, "--smtp-tls-key", key, "--smtp-require-tls")
	}
	if opts.Username != "" {
		auth := filepath.Join(dir, "auth")
		if err := os.WriteFile(auth, []byte(opts.Username+":"+opts.Password+"\n"), 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--smtp-auth-file", auth)
	}
	var log bytes.Buffer
	cmd := exec.Command(bin, args...) // #nosec G204 -- the checksummed binary
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, &log, &log
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-done: // the log is complete once it exited
			if strings.Contains(log.String(), "address already in use") {
				return nil, fmt.Errorf("%w: %s", errPortTaken, log.String())
			}
			return nil, fmt.Errorf("mailpit stopped: %s", log.String())
		default:
		}
		if s.ours(database) && s.greets(opts.Transport) {
			select {
			case <-done: // it lost a port after all: the loop says which
			default:
				return s, nil
			}
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			<-done
			return nil, fmt.Errorf("mailpit did not start: %s", log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ours reports whether the Mailpit answering on the API port is this one, by its database.
func (s *Server) ours(database string) bool {
	resp, err := http.Get(s.API + "/api/v1/info")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	var info struct{ Database string }
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&info) == nil && info.Database == database
}

// greets reports whether the SMTP port answers with a greeting, over TLS for transport TLS.
func (s *Server) greets(transport string) bool {
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if transport == TLS {
		ca, err := os.ReadFile(s.CAFile)
		if err != nil {
			return false
		}
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(ca)
		tlsConn := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: s.Host, MinVersion: tls.VersionTLS12})
		if tlsConn.Handshake() != nil {
			return false
		}
		conn = tlsConn
	}
	greeting, err := bufio.NewReader(conn).ReadString('\n')
	return err == nil && strings.HasPrefix(greeting, "220 ")
}

// Address is a mailbox of a message.
type Address struct{ Name, Address string }

// Summary is a message as Mailpit lists it.
type Summary struct {
	ID      string
	From    Address
	To      []Address
	Subject string
}

// Message is a message, its parts decoded.
type Message struct {
	Summary
	Text, HTML string
}

// Messages returns the messages received, newest first.
func (s *Server) Messages(t testing.TB) []Summary {
	t.Helper()
	var list struct{ Messages []Summary }
	s.get(t, "/api/v1/messages?limit=1000", &list)
	return list.Messages
}

// To returns the messages to an address, newest first.
func (s *Server) To(t testing.TB, address string) []Summary {
	t.Helper()
	var out []Summary
	for _, m := range s.Messages(t) {
		for _, a := range m.To {
			if strings.EqualFold(a.Address, address) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// Message reads a message.
func (s *Server) Message(t testing.TB, id string) Message {
	t.Helper()
	var m Message
	s.get(t, "/api/v1/message/"+url.PathEscape(id), &m)
	return m
}

// Raw returns a message as it was received, headers included.
func (s *Server) Raw(t testing.TB, id string) string {
	t.Helper()
	resp, err := http.Get(s.API + "/api/v1/message/" + url.PathEscape(id) + "/raw")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("mailtest: raw message %s: %s %v", id, resp.Status, err)
	}
	return string(b)
}

// Await waits until at least n messages were received, and returns them, newest first.
func (s *Server) Await(t testing.TB, n int, timeout time.Duration) []Summary {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		list := s.Messages(t)
		if len(list) >= n {
			return list
		}
		if time.Now().After(deadline) {
			t.Fatalf("mailtest: %d messages after %s, want %d", len(list), timeout, n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Fail makes the server refuse every message with a temporary error (451) until Recover.
func (s *Server) Fail(t testing.TB) { s.chaos(t, 451, 100) }

// Recover makes the server accept messages again.
func (s *Server) Recover(t testing.TB) { s.chaos(t, 451, 0) }

func (s *Server) chaos(t testing.TB, code, probability int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Sender": map[string]int{"ErrorCode": code, "Probability": probability}})
	req, err := http.NewRequest(http.MethodPut, s.API+"/api/v1/chaos", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mailtest: chaos: %s", resp.Status)
	}
}

func (s *Server) get(t testing.TB, path string, v any) {
	t.Helper()
	resp, err := http.Get(s.API + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("mailtest: GET %s: %s %s", path, resp.Status, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// certificates writes a CA, and a certificate it signed for 127.0.0.1 and localhost with its key.
func certificates(caFile, certFile, keyFile string) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mailtest CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	if ca, err = x509.ParseCertificate(caDER); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	for _, f := range []struct {
		path, kind string
		der        []byte
	}{{caFile, "CERTIFICATE", caDER}, {certFile, "CERTIFICATE", der}, {keyFile, "EC PRIVATE KEY", keyDER}} {
		if err := os.WriteFile(f.path, pem.EncodeToMemory(&pem.Block{Type: f.kind, Bytes: f.der}), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// binary returns the path of Mailpit's binary, downloaded once.
func binary() (string, error) {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	want, ok := binaries[platform]
	if !ok {
		return "", fmt.Errorf("no Mailpit binary for %s", platform)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "quai", "mailpit")
	path := filepath.Join(dir, "mailpit-"+Version+"-"+platform)
	if sum, err := fileSHA256(path); err == nil && sum == want {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	u := fmt.Sprintf("https://github.com/axllent/mailpit/releases/download/v%s/mailpit-%s.tar.gz", Version, platform)
	// A download can break off (a reset connection on CI): it is tried three times.
	for attempt := 1; ; attempt++ {
		err = download(u, archives[platform], want, dir, path)
		if err == nil || attempt == 3 {
			return path, err
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
}

// download saves the binary of the archive at url as path, through a temporary file in dir, once
// the archive's SHA-256 is archive and the binary's is want.
func download(url, archive, want, dir, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	var b bytes.Buffer
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(&b, h), io.LimitReader(resp.Body, 200<<20)); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != archive {
		return fmt.Errorf("download %s: the SHA-256 does not match", url)
	}
	z, err := gzip.NewReader(&b)
	if err != nil {
		return err
	}
	tr := tar.NewReader(z)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("download %s: no mailpit binary in the archive", url)
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Name == "mailpit" {
			break
		}
	}
	f, err := os.CreateTemp(dir, "partial-")
	if err != nil {
		return err
	}
	h = sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(tr, 200<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		err = fmt.Errorf("download %s: the binary's SHA-256 does not match", url)
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o700) // #nosec G302 -- an executable, for its owner only
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- the cached binary
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
