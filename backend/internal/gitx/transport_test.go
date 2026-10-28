package gitx

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/damhau/quai/backend/internal/engine/plan"
)

// token is the secret the test server requires, as an HTTP Basic password.
const token = "s3cr3t-pat-value"

var header = "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("quai:"+token))

// testCA is a certificate authority of the tests' own, as an internal CA would be.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	file string // the CA certificate, in PEM
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Quai test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, file: file}
}

// serverCert issues a certificate for 127.0.0.1 and git.quai.test.
func (ca *testCA) serverCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "git.quai.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"git.quai.test"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// gitServer is a smart-HTTP Git server over HTTPS (git http-backend), serving the bare
// repositories of root to requests that carry the header.
type gitServer struct {
	*httptest.Server
	root     string
	requests atomic.Int32
}

func newGitServer(t *testing.T, ca *testCA, bare string) *gitServer {
	t.Helper()
	root := filepath.Dir(bare)
	// Pushes over HTTP, partial clones and the objects they ask for later, as the platforms allow.
	for _, kv := range [][2]string{{"http.receivepack", "true"}, {"uploadpack.allowFilter", "true"}, {"uploadpack.allowAnySHA1InWant", "true"}} {
		mustRun(t, "", "--git-dir", bare, "config", kv[0], kv[1])
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{
		"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + t.TempDir(),
	}, InheritEnv: []string{"PATH"}}
	s := &gitServer{root: root}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if "Authorization: "+r.Header.Get("Authorization") != header {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{ca.serverCert(t)}, MinVersion: tls.VersionTLS12}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// url is the HTTPS URL of a repository of the server, at host (127.0.0.1 when empty).
func (s *gitServer) url(host, name string) string {
	u, _ := url.Parse(s.URL)
	if host != "" {
		u.Host = host + ":" + u.Port()
	}
	return u.String() + "/" + name
}

func withHeader(h string) func(context.Context) (Credentials, error) {
	return func(context.Context) (Credentials, error) { return Credentials{Header: h}, nil }
}

// TestHTTPS clones, reads and pushes over HTTPS with the header, trusting the server through the
// CA bundle only.
func TestHTTPS(t *testing.T) {
	bare, push := remote(t)
	ca := newCA(t)
	srv := newGitServer(t, ca, bare)
	remoteURL := srv.url("", "remote.git")
	dir := func() string { return filepath.Join(t.TempDir(), "m.git") }

	// Certificates are verified: without the CA, git does not trust the server.
	if _, err := OpenMirror(ctx, remoteURL, dir(), Options{Committer: quai, Auth: withHeader(header)}); err == nil ||
		!strings.Contains(err.Error(), "certificate") {
		t.Errorf("an unknown CA: %v", err)
	}
	// Without the header, or with another, the server refuses.
	for _, h := range []string{"", "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("quai:wrong"))} {
		opts := Options{Committer: quai, CAFile: ca.file}
		if h != "" {
			opts.Auth = withHeader(h)
		}
		if _, err := OpenMirror(ctx, remoteURL, dir(), opts); err == nil {
			t.Errorf("header %q was accepted", h)
		}
	}
	for _, h := range []string{"no colon", "Authorization: a\r\nX-Injected: b", ": empty name"} {
		if _, err := OpenMirror(ctx, remoteURL, dir(), Options{Committer: quai, CAFile: ca.file, Auth: withHeader(h)}); err == nil ||
			!strings.Contains(err.Error(), "an HTTP header") {
			t.Errorf("header %q: %v", h, err)
		}
	}

	m, err := OpenMirror(ctx, remoteURL, dir(), Options{Committer: quai, CAFile: ca.file, Auth: withHeader(header)})
	if err != nil {
		t.Fatal(err)
	}
	push("envs/dev/vms/new.yaml", "cpu: 2\n")
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	head, _ := m.Head(ctx, "main")
	if b, err := m.ReadFile(ctx, head, "envs/dev/vms/new.yaml"); err != nil || string(b) != "cpu: 2\n" {
		t.Errorf("after a fetch: %q, %v", b, err)
	}
	if _, err := m.CommitAndPush(ctx, Commit{Branch: "main", Message: "Resize web01\n", Author: bob,
		Changes: []plan.Change{{Op: plan.OpUpdate, Path: "envs/prod/vms/web01.yaml", After: []byte("cpu: 8\n")}}}); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, "", "--git-dir", bare, "show", "main:envs/prod/vms/web01.yaml"); got != "cpu: 8\n" {
		t.Errorf("pushed %q", got)
	}
	// Nothing of the credentials stays in the mirror's configuration.
	if cfg, _ := os.ReadFile(filepath.Join(m.dir, "config")); strings.Contains(string(cfg), token) || strings.Contains(string(cfg), "extraHeader") {
		t.Errorf("the mirror's configuration holds the credentials:\n%s", cfg)
	}
}

// TestProxy reaches the server through an HTTP proxy, by a name only the proxy resolves.
func TestProxy(t *testing.T) {
	bare, _ := remote(t)
	ca := newCA(t)
	srv := newGitServer(t, ca, bare)
	target := srv.Listener.Addr().String()
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Like the agents' sandbox proxy: credentials required, and the connection closed after
		// a 407 without saying so, so a client must send them with its first request.
		if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("quai:proxy-password")) {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_, _ = conn.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"proxy\"\r\nContent-Length: 0\r\n\r\n"))
				_ = conn.Close()
			}
			return
		}
		if r.Method != http.MethodConnect || !strings.HasPrefix(r.Host, "git.quai.test:") {
			http.Error(w, "CONNECT to git.quai.test only", http.StatusForbidden)
			return
		}
		connects.Add(1)
		upstream, err := net.Dial("tcp", target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		go func() { _, _ = io.Copy(upstream, buf); _ = upstream.Close() }()
		_, _ = io.Copy(conn, upstream)
		_ = conn.Close()
	}))
	t.Cleanup(proxy.Close)
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("quai", "proxy-password")
	var asked []string
	opts := Options{Committer: quai, CAFile: ca.file, Auth: withHeader(header), Proxy: func(u *url.URL) (*url.URL, error) {
		asked = append(asked, u.Host)
		return proxyURL, nil
	}}
	m, err := OpenMirror(ctx, srv.url("git.quai.test", "remote.git"), filepath.Join(t.TempDir(), "m.git"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if connects.Load() == 0 || len(asked) == 0 || !strings.HasPrefix(asked[0], "git.quai.test:") {
		t.Errorf("the proxy was not used: %d connections, asked for %v", connects.Load(), asked)
	}
	if _, err := m.Head(ctx, "main"); err != nil {
		t.Error(err)
	}
	opts.Proxy = func(*url.URL) (*url.URL, error) { return nil, errors.New("no proxy decision") }
	if _, err := OpenMirror(ctx, srv.url("git.quai.test", "remote.git"), filepath.Join(t.TempDir(), "m.git"), opts); err == nil {
		t.Error("a proxy error was ignored")
	}
}

// TestPartialClone mirrors without file contents; reading a file, or checking out a sparse
// worktree, fetches only what it needs, with the credentials.
func TestPartialClone(t *testing.T) {
	bare, _ := remote(t)
	ca := newCA(t)
	srv := newGitServer(t, ca, bare)
	m, err := OpenMirror(ctx, srv.url("", "remote.git"), filepath.Join(t.TempDir(), "m.git"),
		Options{Committer: quai, CAFile: ca.file, Auth: withHeader(header), PartialClone: true})
	if err != nil {
		t.Fatal(err)
	}
	blobs := func() int {
		out := mustRun(t, "", "--git-dir", m.dir, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype)")
		return strings.Count(out, "blob")
	}
	if n := blobs(); n != 0 {
		t.Errorf("a partial clone holds %d blobs", n)
	}
	head, _ := m.Head(ctx, "main")
	wt, err := m.Worktree(ctx, head, "envs/prod/vms/web01.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wt.Close() }()
	var checkedOut []string
	_ = filepath.WalkDir(wt.Dir(), func(p string, d os.DirEntry, _ error) error {
		if d.Name() == ".git" { // a file, in a worktree
			return nil
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(wt.Dir(), p)
			checkedOut = append(checkedOut, filepath.ToSlash(rel))
		}
		return nil
	})
	if strings.Join(checkedOut, ",") != "envs/prod/vms/web01.yaml" || blobs() != 1 {
		t.Errorf("a sparse worktree checked out %v and fetched %d blobs", checkedOut, blobs())
	}
	if b, err := m.ReadFile(ctx, head, "README.md"); err != nil || string(b) != "# infra\n" || blobs() != 2 {
		t.Errorf("README.md = %q, %v; %d blobs", b, err, blobs())
	}
}

// TestSparseWorktree checks out the paths given, files and directories, and nothing else.
func TestSparseWorktree(t *testing.T) {
	bare, push := remote(t)
	push("odd [name]*?.yaml", "x: 1\n")
	m := mirror(t, bare)
	head, _ := m.Head(ctx, "main")
	wt, err := m.Worktree(ctx, head, "envs/prod/vms", "odd [name]*?.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wt.Close() }()
	for p, want := range map[string]bool{"envs/prod/vms/web01.yaml": true, "envs/prod/vms/db01.yaml": true, "odd [name]*?.yaml": true, "README.md": false} {
		if _, err := os.Stat(filepath.Join(wt.Dir(), p)); (err == nil) != want {
			t.Errorf("%s checked out: %v, want %v", p, err == nil, want)
		}
	}
	if _, err := m.Worktree(ctx, head, "../outside"); err == nil {
		t.Error("a path outside the repository")
	}
}

// TestRedaction: what git reports never carries the credentials of the remote or the proxy.
func TestRedaction(t *testing.T) {
	bare, _ := remote(t)
	ca := newCA(t)
	srv := newGitServer(t, ca, bare)
	m, err := OpenMirror(ctx, srv.url("", "remote.git"), filepath.Join(t.TempDir(), "m.git"),
		Options{Committer: quai, CAFile: ca.file, Auth: withHeader(header)})
	if err != nil {
		t.Fatal(err)
	}
	// A server that repeats the secrets back, in a hook refusing the push.
	encoded := strings.TrimPrefix(header, "Authorization: Basic ")
	hook := filepath.Join(bare, "hooks", "pre-receive")
	write(t, hook, fmt.Sprintf("#!/bin/sh\necho 'seen: %s %s https://quai:%s@example.com/x' >&2\nexit 1\n", encoded, token, token))
	if err := os.Chmod(hook, 0o700); err != nil { // #nosec G302 -- a test hook, executable by its owner
		t.Fatal(err)
	}
	_, err = m.CommitAndPush(ctx, Commit{Branch: "main", Message: "m\n", Author: bob,
		Changes: []plan.Change{{Op: plan.OpUpdate, Path: "README.md", After: []byte("changed\n")}}})
	if err == nil || !strings.Contains(err.Error(), "seen: ***") {
		t.Fatalf("the hook's refusal: %v", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), encoded) {
		t.Errorf("the error shows the credentials: %v", err)
	}

	// A remote URL with credentials, unreachable.
	_, err = OpenMirror(ctx, "https://quai:"+token+"@127.0.0.1:1/remote.git", filepath.Join(t.TempDir(), "m.git"), Options{Committer: quai})
	if err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("a URL with credentials: %v", err)
	}
	if got := scrub("fatal: https://u:p@h/x and Basic abc= at http://v@w", []string{"abc=", ""}); got != "fatal: https://***@h/x and Basic *** at http://***@w" {
		t.Errorf("scrub = %q", got)
	}
}

// sshServer serves the bare repositories of root over SSH to the holder of key: git
// upload-pack and receive-pack, run as commands.
func sshServer(t *testing.T, root string, key ssh.PublicKey) (addr string, hostKey ssh.PublicKey) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if string(k.Marshal()) != string(key.Marshal()) {
			return nil, errors.New("unknown key")
		}
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				serveSSH(conn, cfg, root)
			}()
		}
	}()
	return ln.Addr().String(), signer.PublicKey()
}

func serveSSH(conn net.Conn, cfg *ssh.ServerConfig, root string) {
	defer func() { _ = conn.Close() }()
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, requests, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = ch.Close() }()
			for req := range requests {
				if req.Type == "env" {
					_ = req.Reply(true, nil)
					continue
				}
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				_ = ssh.Unmarshal(req.Payload, &payload)
				prog, repo, _ := strings.Cut(payload.Command, " ")
				repo = strings.Trim(repo, "'/")
				if (prog != "git-upload-pack" && prog != "git-receive-pack") || strings.Contains(repo, "..") {
					_ = req.Reply(false, nil)
					return
				}
				_ = req.Reply(true, nil)
				cmd := exec.Command("git", strings.TrimPrefix(prog, "git-"), filepath.Join(root, repo)) // #nosec G204 -- the test's own server
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
				cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
				status := 0
				if err := cmd.Run(); err != nil {
					status = 1
				}
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)})) // #nosec G115 -- 0 or 1
				return
			}
		}()
	}
}

// TestSSH clones and pushes with an SSH key, checking the server's host key.
func TestSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh client")
	}
	bare, _ := remote(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(block)
	sshPub, _ := ssh.NewPublicKey(pub)
	addr, hostKey := sshServer(t, filepath.Dir(bare), sshPub)
	host, port, _ := net.SplitHostPort(addr)
	remoteURL := "ssh://git@" + addr + "/remote.git"
	knownHosts := []byte(fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(hostKey)))
	creds := func(c Credentials) func(context.Context) (Credentials, error) {
		return func(context.Context) (Credentials, error) { return c, nil }
	}

	m, err := OpenMirror(ctx, remoteURL, filepath.Join(t.TempDir(), "m.git"), Options{Committer: quai, Auth: creds(Credentials{SSHKey: key, KnownHosts: knownHosts})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CommitAndPush(ctx, Commit{Branch: "main", Message: "Resize web01\n", Author: bob,
		Changes: []plan.Change{{Op: plan.OpUpdate, Path: "envs/prod/vms/web01.yaml", After: []byte("cpu: 8\n")}}}); err != nil {
		t.Fatal(err)
	}

	// The host key is always checked: another one, or none, is refused.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(otherPriv)
	wrongHosts := []byte(fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(other.PublicKey())))
	for name, c := range map[string]Credentials{
		"another host key": {SSHKey: key, KnownHosts: wrongHosts},
		"no host keys":     {SSHKey: key},
	} {
		if _, err := OpenMirror(ctx, remoteURL, filepath.Join(t.TempDir(), "m.git"), Options{Committer: quai, Auth: creds(c)}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "quai-ssh-*")); len(matches) > 0 {
		for _, dir := range matches {
			if b, _ := os.ReadFile(filepath.Join(dir, "key")); strings.Contains(string(b), string(key)) {
				t.Errorf("the key was left in %s", dir)
			}
		}
	}
}

// TestNewBranch commits on a new branch started from another one, as a pull request's source.
func TestNewBranch(t *testing.T) {
	bare, _ := remote(t)
	m := mirror(t, bare)
	main, _ := m.Head(ctx, "main")
	sha, err := m.CommitAndPush(ctx, Commit{Branch: "quai/r-1-web01", Base: "main", Message: "Resize web01\n", Author: bob,
		Changes: []plan.Change{{Op: plan.OpUpdate, Path: "envs/prod/vms/web01.yaml", After: []byte("cpu: 8\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	if parents, err := m.Parents(ctx, sha); err != nil || len(parents) != 1 || parents[0] != main {
		t.Errorf("parents = %v, %v; main is %s", parents, err, main)
	}
	if head, _ := m.Head(ctx, "main"); head != main {
		t.Error("main moved")
	}
	if _, err := m.CommitAndPush(ctx, Commit{Branch: "quai/r-2", Base: "nope", Message: "m\n"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing base: %v", err)
	}
	if _, err := m.CommitAndPush(ctx, Commit{Branch: "quai/r-2", Base: "../main", Message: "m\n"}); err == nil {
		t.Error("an invalid base")
	}
	if _, err := m.Parents(ctx, "zzz"); err == nil {
		t.Error("Parents of an invalid commit")
	}
	if err := m.DeleteBranch(ctx, "quai/r-1-web01"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Head(ctx, "quai/r-1-web01"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the branch is still there: %v", err)
	}
	if err := m.DeleteBranch(ctx, "quai/r-1-web01"); err != nil {
		t.Errorf("deleting it again: %v", err)
	}
}

// TestFiles reads a commit's files without a checkout, as fs.FS.
func TestFiles(t *testing.T) {
	bare, push := remote(t)
	m := mirror(t, bare)
	old, _ := m.Head(ctx, "main")
	push("envs/prod/vms/web01.yaml", "cpu: 16\n")
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	fsys, err := m.Files(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := fs.ReadFile(fsys, "envs/prod/vms/web01.yaml"); err != nil || string(b) != "cpu: 4\n" {
		t.Errorf("the old version = %q, %v", b, err)
	}
	entries, err := fs.ReadDir(fsys, "envs/prod/vms")
	if err != nil || len(entries) != 2 || entries[0].Name() != "db01.yaml" || entries[0].IsDir() {
		t.Errorf("ReadDir = %v, %v", entries, err)
	}
	if root, err := fs.ReadDir(fsys, "."); err != nil || len(root) != 2 || !root[1].IsDir() {
		t.Errorf("the root = %v, %v", root, err)
	}
	if _, err := fs.ReadFile(fsys, "nope.yaml"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file: %v", err)
	}
	if _, err := fs.ReadFile(fsys, "../etc/passwd"); err == nil {
		t.Error("an invalid path")
	}
	var walked []string
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			walked = append(walked, p)
		}
		return err
	})
	if strings.Join(walked, ",") != "README.md,envs/prod/vms/db01.yaml,envs/prod/vms/web01.yaml" {
		t.Errorf("walked %v", walked)
	}
	if _, err := m.Files(ctx, "main"); err == nil {
		t.Error("a branch name instead of a commit")
	}
}
