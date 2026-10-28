// Package k8stest gives tests a real Kubernetes API server (docs/engineering/testing.md): the
// kube-apiserver and etcd of the envtest release of controller-tools, downloaded once into the
// user cache directory and checked against their SHA-512, then started by controller-runtime's
// envtest. It runs inside the agents' sandbox: no Docker, no cluster.
//
// A package that uses it calls Main from TestMain.
package k8stest

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// Version is the envtest release: Kubernetes 1.37.
const Version = "v1.37.0"

// sha512s are the archives' checksums, from the release's .sha512 files.
var sha512s = map[string]string{
	"linux-amd64":  "1d1c453633b72c161a5d5a886cde7ac850be1a2ac796a9e1d4ffacacc64868295bdd2d57aa66cd0c158f5ce510f5dfe3fbc61ac21bd3dcfb875bd70658aa663a",
	"linux-arm64":  "ae6a670502988200b0131c943758cfd3d3a58cf4e6247ef7f0e6a6467f1cd9a333c802e86101f0446b1b92a0e327387224e1e44fe1a4693da0929c6e529cfe9a",
	"darwin-amd64": "19a2a5376a8aa57a7b25ec5198834db29bf2ba0d6ff572d7f45ba683b13fb14c3ebec8069e92db66c39f1c7a9cff1bc2be879e2aacdc9fa8b8a7e861959bdf7b",
	"darwin-arm64": "fb38cfacdd71b5e97a4d4cceac861af5f55069cf783f0e49cf181bfc32eb3e557c2091a534dc5d38f1b92c5ba142bc1979f215a5385b3630ea6a661be6fa161b",
}

var cfg *rest.Config

// Main starts the API server, runs the tests and stops it. Use it as
// `func TestMain(m *testing.M) { os.Exit(k8stest.Main(m)) }`.
func Main(m *testing.M) int {
	ctrllog.SetLogger(logr.Discard())
	dir, err := binaries()
	if err != nil {
		fmt.Fprintln(os.Stderr, "k8stest:", err)
		return 1
	}
	env := &envtest.Environment{BinaryAssetsDirectory: dir}
	// Without a default route (a sandbox's network namespace), the API server cannot pick the
	// address to advertise by itself; a loopback address needs the endpoint reconciler off, which
	// tests do not use.
	env.ControlPlane.GetAPIServer().Configure().
		Set("advertise-address", "127.0.0.1").
		Set("endpoint-reconciler-type", "none")
	if cfg, err = env.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "k8stest: start the API server:", err)
		return 1
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "k8stest: stop:", err)
	}
	return code
}

// Config returns the client configuration of the API server, with full access.
func Config(t testing.TB) *rest.Config {
	t.Helper()
	if cfg == nil {
		t.Fatal("k8stest: call k8stest.Main from TestMain")
	}
	return rest.CopyConfig(cfg)
}

// binaries returns the directory holding etcd and kube-apiserver, downloading them first if the
// cache does not have them.
func binaries() (string, error) {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	want, ok := sha512s[platform]
	if !ok {
		return "", fmt.Errorf("no envtest binaries for %s", platform)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "quai", "envtest", Version+"-"+platform)
	if _, err := os.Stat(filepath.Join(dir, "kube-apiserver")); err == nil {
		return dir, nil
	}
	url := fmt.Sprintf("https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-%s/envtest-%s-%s.tar.gz", Version, Version, platform)
	// A download can break off (a reset connection on CI): it is tried three times.
	var archive string
	for attempt := 1; ; attempt++ {
		if archive, err = download(url, want); err == nil {
			break
		}
		if attempt == 3 {
			return "", err
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	defer func() { _ = os.Remove(archive) }()
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "partial-")
	if err != nil {
		return "", err
	}
	if err := extract(archive, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil && !errors.Is(err, os.ErrExist) {
		_ = os.RemoveAll(tmp)
		if _, statErr := os.Stat(filepath.Join(dir, "kube-apiserver")); statErr == nil {
			return dir, nil // another test binary got there first
		}
		return "", err
	}
	return dir, nil
}

// download saves url to a temporary file and checks its SHA-512.
func download(url, want string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.CreateTemp("", "envtest-*.tar.gz")
	if err != nil {
		return "", err
	}
	h := sha512.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		err = fmt.Errorf("download %s: the SHA-512 does not match", url)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// extract writes the archive's regular files, by base name, into dir.
func extract(archive, dir string) error {
	f, err := os.Open(archive) // #nosec G304 -- the file download just wrote
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// #nosec G302 G304 -- executables, for their owner only; a base name in dir
		out, err := os.OpenFile(filepath.Join(dir, path.Base(hdr.Name)), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, io.LimitReader(tr, 1<<30)) // #nosec G110 -- checksummed archive, bounded
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
}
