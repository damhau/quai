package secrets

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ExpiresAnnotation, on a Kubernetes Secret, says when the secret stops working (RFC 3339), for
// the warnings 30 days ahead: a PAT's expiry, for instance.
const ExpiresAnnotation = "quai.dev/expires-at"

// Kubernetes reads `k8s:namespace/name#key` from Kubernetes Secrets. The key may be left out when
// the Secret holds one key only. The client is made on first use, so that a Quai that references
// no Kubernetes Secret never needs a cluster.
type Kubernetes struct {
	config func() (*rest.Config, error)

	once   sync.Once
	client kubernetes.Interface
	err    error
}

// NewKubernetes returns a backend whose client comes from config.
func NewKubernetes(config func() (*rest.Config, error)) *Kubernetes {
	return &Kubernetes{config: config}
}

// KubeConfig is the cluster Quai runs in, or, outside a cluster, the kubeconfig file (the
// default loading rules: KUBECONFIG, then ~/.kube/config, when path is empty).
func KubeConfig(path string) func() (*rest.Config, error) {
	return func() (*rest.Config, error) {
		if path == "" && os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			return rest.InClusterConfig()
		}
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		rules.ExplicitPath = path
		return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	}
}

func (k *Kubernetes) Get(ctx context.Context, ref string) (Secret, error) {
	namespace, name, key, err := splitK8s(ref)
	if err != nil {
		return Secret{}, err
	}
	k.once.Do(func() {
		var cfg *rest.Config
		if cfg, k.err = k.config(); k.err == nil {
			k.client, k.err = kubernetes.NewForConfig(cfg)
		}
	})
	if k.err != nil {
		return Secret{}, fmt.Errorf("Kubernetes client: %w", k.err)
	}
	s, err := k.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Secret{}, ErrNotFound
	}
	if err != nil {
		return Secret{}, err
	}
	if key == "" {
		if len(s.Data) != 1 {
			keys := slices.Sorted(maps.Keys(s.Data))
			return Secret{}, fmt.Errorf("the Secret has the keys %s: name one with #key", strings.Join(keys, ", "))
		}
		for k := range s.Data {
			key = k
		}
	}
	v, ok := s.Data[key]
	if !ok {
		return Secret{}, fmt.Errorf("%w: the Secret has no key %s", ErrNotFound, key)
	}
	// Trimmed: a Secret made from a file often ends with a newline no token has.
	sec := Secret{value: strings.TrimSpace(string(v))}
	if at := s.Annotations[ExpiresAnnotation]; at != "" {
		if sec.Expires, err = time.Parse(time.RFC3339, at); err != nil {
			return Secret{}, fmt.Errorf("annotation %s: %q is not an RFC 3339 time", ExpiresAnnotation, at)
		}
	}
	return sec, nil
}
