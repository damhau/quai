// Package adotest gives live tests the Azure DevOps sandbox (docs/plan/phase-1-backlog.md#test-infrastructure):
// QUAI_TEST_AZURE_DEVOPS_URL names the project, QUAI_TEST_AZURE_DEVOPS_PAT_FILE the file holding
// the PAT; without them, the tests are skipped. The PAT is read from the file only and never
// shown. Tests create what they need with a quai-test- prefix and delete it, failed or not.
//
// Azure DevOps cannot reach a test machine: Deliveries reads back, from a service hook
// subscription's history, what it tried to deliver, to replay it to Quai.
package adotest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
)

// APIVersion is the version of the calls the helpers make.
const APIVersion = "7.0"

// Sandbox is the Azure DevOps project of the live tests.
type Sandbox struct {
	Collection string // the organization's URL
	Project    string
	Network    *netx.Network // the environment's proxy, as Quai uses it
	HTTP       *http.Client  // the connectors' shared client over Network
	pat        secrets.Secret
}

// New returns the sandbox, or skips the test.
func New(t *testing.T) *Sandbox {
	t.Helper()
	file, project := os.Getenv("QUAI_TEST_AZURE_DEVOPS_PAT_FILE"), os.Getenv("QUAI_TEST_AZURE_DEVOPS_URL")
	if file == "" || project == "" {
		t.Skip("QUAI_TEST_AZURE_DEVOPS_PAT_FILE and QUAI_TEST_AZURE_DEVOPS_URL are not set: no live Azure DevOps tests")
	}
	b, err := os.ReadFile(file) // #nosec G304 -- the file the developer named
	if err != nil {
		t.Fatalf("the PAT file: %v", err)
	}
	network, err := netx.New("")
	if err != nil {
		t.Fatal(err)
	}
	project = strings.TrimSuffix(project, "/")
	i := strings.LastIndex(project, "/")
	name, err := url.PathUnescape(project[i+1:])
	if err != nil {
		t.Fatal(err)
	}
	return &Sandbox{Collection: project[:i], Project: name, Network: network, HTTP: connectors.NewHTTPClient(network.Transport(), nil),
		pat: secrets.FromValue(strings.TrimSpace(string(b)))}
}

// PAT resolves the PAT, as a connector's Credentials does.
func (s *Sandbox) PAT(context.Context) (secrets.Secret, bool, error) { return s.pat, true, nil }

// Get is the PAT as a secret backend: a reference of any name resolves to it.
func (s *Sandbox) Get(context.Context, string) (secrets.Secret, error) { return s.pat, nil }

// Path joins segments into a path under the organization, each one encoded.
func Path(segments ...string) string {
	var b strings.Builder
	for _, seg := range segments {
		b.WriteString("/" + url.PathEscape(seg))
	}
	return b.String()
}

// Do calls path (under the organization) with the PAT, sending in as JSON and decoding the answer
// into out. Errors carry the status and the API's message, never the request's headers.
func (s *Sandbox) Do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("api-version", APIVersion)
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.Collection+path+"?"+query.Encode(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+s.pat.Reveal())))
	req.Header.Set("X-TFS-FedAuthRedirect", "Suppress")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, e.Message)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Repository creates a repository with a first commit on main, and deletes it (and purges it
// from the recycle bin) when the test ends. Its name starts with quai-test-.
func (s *Sandbox) Repository(t *testing.T) (name, id string) {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name = "quai-test-" + hex.EncodeToString(b)
	var repo struct {
		ID string `json:"id"`
	}
	if err := s.Do(ctx, http.MethodPost, Path(s.Project, "_apis", "git", "repositories"), nil, map[string]any{"name": name}, &repo); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.Do(ctx, http.MethodDelete, Path(s.Project, "_apis", "git", "repositories", repo.ID), nil, nil, nil); err != nil {
			t.Errorf("delete %s: %v", name, err)
			return
		}
		if err := s.Do(ctx, http.MethodDelete, Path(s.Project, "_apis", "git", "recycleBin", "repositories", repo.ID), nil, nil, nil); err != nil {
			t.Errorf("purge %s: %v", name, err)
		}
	})
	push := map[string]any{
		"refUpdates": []map[string]any{{"name": "refs/heads/main", "oldObjectId": strings.Repeat("0", 40)}},
		"commits": []map[string]any{{"comment": "Initial commit", "changes": []map[string]any{{"changeType": "add",
			"item": map[string]any{"path": "/README.md"}, "newContent": map[string]any{"content": "# Quai test\n", "contentType": "rawtext"}}}}},
	}
	if err := s.Do(ctx, http.MethodPost, Path(s.Project, "_apis", "git", "repositories", repo.ID, "pushes"), nil, push, nil); err != nil {
		t.Fatalf("initialize %s: %v", name, err)
	}
	return name, repo.ID
}

// URL returns a repository's Git URL.
func (s *Sandbox) URL(name string) string {
	return s.Collection + Path(s.Project, "_git", name)
}

// Subscriptions returns the service hook subscriptions delivering to hook, by event type.
func (s *Sandbox) Subscriptions(ctx context.Context, hook string) (map[string]string, error) {
	var list struct {
		Value []struct {
			ID             string            `json:"id"`
			EventType      string            `json:"eventType"`
			ConsumerInputs map[string]string `json:"consumerInputs"`
		} `json:"value"`
	}
	if err := s.Do(ctx, http.MethodGet, "/_apis/hooks/subscriptions", url.Values{"consumerId": {"webHooks"}}, nil, &list); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, sub := range list.Value {
		if sub.ConsumerInputs["url"] == hook {
			out[sub.EventType] = sub.ID
		}
	}
	return out, nil
}

// Unsubscribe deletes the subscriptions delivering to hook when the test ends.
func (s *Sandbox) Unsubscribe(t *testing.T, hook string) {
	t.Cleanup(func() {
		ctx := context.Background()
		subs, err := s.Subscriptions(ctx, hook)
		if err != nil {
			t.Errorf("subscriptions: %v", err)
			return
		}
		for _, id := range subs {
			if err := s.Do(ctx, http.MethodDelete, "/_apis/hooks/subscriptions/"+id, nil, nil, nil); err != nil {
				t.Errorf("unsubscribe %s: %v", id, err)
			}
		}
	})
}

// Delivery is a request Azure DevOps tried to send, as it sent it.
type Delivery struct {
	EventType string
	Header    http.Header
	Body      []byte
}

// Deliveries waits until Azure DevOps recorded deliveries to hook that enough accepts, and
// returns them: the body as sent, and the headers with secret, which the history masks, put back
// in secretHeader.
func (s *Sandbox) Deliveries(t *testing.T, hook, secretHeader, secret string, enough func([]Delivery) bool) []Delivery {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		subs, err := s.Subscriptions(ctx, hook)
		if err != nil {
			t.Fatal(err)
		}
		var out []Delivery
		for event, id := range subs {
			var list struct {
				Value []struct {
					ID int `json:"id"`
				} `json:"value"`
			}
			at := "/_apis/hooks/subscriptions/" + id + "/notifications"
			if err := s.Do(ctx, http.MethodGet, at, url.Values{"maxResults": {"50"}}, nil, &list); err != nil {
				t.Fatal(err)
			}
			for _, n := range list.Value {
				var one struct {
					Details struct {
						Request string `json:"request"`
					} `json:"details"`
				}
				if err := s.Do(ctx, http.MethodGet, at+"/"+strconv.Itoa(n.ID), nil, nil, &one); err != nil {
					t.Fatal(err)
				}
				if header, body, ok := Replay(one.Details.Request, secretHeader, secret); ok {
					out = append(out, Delivery{EventType: event, Header: header, Body: body})
				}
			}
		}
		if enough(out) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("Azure DevOps did not record the deliveries in time: %d so far", len(out))
		}
		time.Sleep(5 * time.Second)
	}
}

// Replay reads a request as a subscription's history shows it: Method, URI, Headers { … },
// Content: the body.
func Replay(request, secretHeader, secret string) (http.Header, []byte, bool) {
	request = strings.ReplaceAll(request, "\r\n", "\n")
	headers, body, ok := strings.Cut(request, "\nContent:\n")
	if !ok {
		return nil, nil, false
	}
	h := http.Header{}
	if _, block, ok := strings.Cut(headers, "Headers:\n{"); ok {
		for _, line := range strings.Split(strings.TrimSuffix(strings.TrimSpace(block), "}"), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
				h.Set(k, strings.TrimSpace(v))
			}
		}
	}
	h.Set(secretHeader, secret)
	return h, []byte(body), true
}

// HasEvent reports whether deliveries hold one of an event type with prefix.
func HasEvent(deliveries []Delivery, prefix string) bool {
	for _, d := range deliveries {
		if strings.HasPrefix(d.EventType, prefix) {
			return true
		}
	}
	return false
}
