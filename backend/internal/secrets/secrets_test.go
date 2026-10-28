package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/damhau/quai/backend/internal/k8stest"
)

func TestMain(m *testing.M) { os.Exit(k8stest.Main(m)) }

const value = "s3cr3t-value"

func TestRedaction(t *testing.T) {
	s := Secret{value: value}
	var out bytes.Buffer
	for _, h := range []slog.Handler{slog.NewTextHandler(&out, nil), slog.NewJSONHandler(&out, nil)} {
		slog.New(h).Info("resolved", slog.Any("secret", s), "again", s)
	}
	b, _ := json.Marshal(struct{ Token Secret }{s})
	text := strings.Join([]string{fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q", s, s, s, s, s), fmt.Errorf("with %v", s).Error(), out.String(), string(b)}, "\n")
	if strings.Contains(text, value) || !strings.Contains(text, redacted) {
		t.Errorf("the secret shows:\n%s", text)
	}
	if s.Reveal() != value {
		t.Error("Reveal")
	}
	now := time.Now()
	if (Secret{Expires: now.Add(20 * 24 * time.Hour)}).ExpiresWithin(30*24*time.Hour, now) != true || (Secret{}).ExpiresWithin(time.Hour, now) {
		t.Error("ExpiresWithin")
	}
}

type counting struct{ gets int }

func (c *counting) Get(_ context.Context, name string) (Secret, error) {
	c.gets++
	return Secret{value: fmt.Sprintf("%s-%d", name, c.gets)}, nil
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	c := &counting{}
	env := Env{Lookup: func(n string) (string, bool) { return map[string]string{"QUAI_PAT": value}[n], n == "QUAI_PAT" }}
	s := New(map[string]Backend{"env": env, "count": c})
	now := time.Now()
	s.now = func() time.Time { return now }

	if sec, err := s.Resolve(ctx, "env:QUAI_PAT"); err != nil || sec.Reveal() != value {
		t.Errorf("env = %v, %v", sec, err)
	}
	if _, err := s.Resolve(ctx, "env:QUAI_NOPE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing variable: %v", err)
	}
	for _, ref := range []string{"QUAI_PAT", "env:", "vault:kv/quai#pat", "env:with space"} {
		if _, err := s.Resolve(ctx, ref); err == nil {
			t.Errorf("%q was resolved", ref)
		}
	}

	// Kept for the TTL, read again after it.
	a, _ := s.Resolve(ctx, "count:x")
	b, _ := s.Resolve(ctx, "count:x")
	now = now.Add(TTL)
	c2, _ := s.Resolve(ctx, "count:x")
	if a.Reveal() != "x-1" || b.Reveal() != "x-1" || c2.Reveal() != "x-2" {
		t.Errorf("cache: %s %s %s", a.Reveal(), b.Reveal(), c2.Reveal())
	}
}

func TestKubernetes(t *testing.T) {
	ctx := context.Background()
	cfg := k8stest.Config(t)
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "quai"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, sec := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "infra-live-pat", Annotations: map[string]string{ExpiresAnnotation: "2027-01-31T00:00:00Z"}},
			Data: map[string][]byte{"token": []byte(value + "\n")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "two"}, Data: map[string][]byte{"a": []byte("A"), "b": []byte("B")}},
	} {
		if _, err := client.CoreV1().Secrets("quai").Create(ctx, sec, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	// Outside a cluster, Quai reads a kubeconfig file, as a deployment beside the cluster would.
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	kc := clientcmdapi.NewConfig()
	kc.Clusters["envtest"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	kc.AuthInfos["admin"] = &clientcmdapi.AuthInfo{ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData, Token: cfg.BearerToken}
	kc.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "admin"}
	kc.CurrentContext = "envtest"
	if err := clientcmd.WriteToFile(*kc, kubeconfig); err != nil {
		t.Fatal(err)
	}
	s := New(map[string]Backend{"k8s": NewKubernetes(KubeConfig(kubeconfig))})

	sec, err := s.Resolve(ctx, "k8s:quai/infra-live-pat")
	if err != nil || sec.Reveal() != value || !sec.Expires.Equal(time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the only key = %q, %v, %v", sec.Reveal(), sec.Expires, err)
	}
	if sec, err := s.Resolve(ctx, "k8s:quai/two#b"); err != nil || sec.Reveal() != "B" {
		t.Errorf("a named key = %v, %v", sec, err)
	}
	if _, err := s.Resolve(ctx, "k8s:quai/two"); err == nil || !strings.Contains(err.Error(), "the keys a, b") {
		t.Errorf("no key among several: %v", err)
	}
	for _, ref := range []string{"k8s:quai/nope", "k8s:quai/two#c", "k8s:other/two#a"} {
		if _, err := s.Resolve(ctx, ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", ref, err)
		}
	}
	if _, err := s.Resolve(ctx, "k8s:two#a"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("a reference without a namespace: %v", err)
	}
}
