package azuredevops

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/microsoft/azure-devops-go-api/azuredevops/v7"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/secrets"
)

// APIVersion is the version every call is pinned to: the one Azure DevOps Server 2022 serves,
// which Services serves too. Endpoints only in preview there add their -preview.N.
const APIVersion = "7.0"

// MinVersion is the oldest instance API Quai works with: Azure DevOps Server 2022.
var MinVersion = azuredevops.Version{Major: 7, Minor: 0}

// gitRepositories is the location of the Git repositories resource, whose newest version tells
// what the instance serves.
var gitRepositories = uuid.MustParse("225f7195-f9c7-4d14-ab28-a83f7ff77e1f")

// Client calls the REST API of one collection (Server) or organization (Services), with a
// personal access token. It is a thin layer over azure-devops-go-api: the SDK's Git client pins
// its calls to 7.1 previews, which Server 2022 refuses, so Client sets the version of every call
// itself, through the SDK's location service (Send) or straight over HTTP (Do).
type Client struct {
	collection string
	http       *http.Client
	sdk        *azuredevops.Client

	mu       sync.Mutex
	instance *Instance
}

// Instance is what Client detected about the instance.
type Instance struct {
	Services   bool   // hosted; otherwise on premises
	MaxVersion string // the newest Git API version it serves
	User       User   // whom the token authenticates
}

// User is an Azure DevOps identity.
type User struct {
	ID    string
	Name  string
	Email string // or UPN: the account name
}

// NewClient returns a client of the collection or organization at collection. Its calls go
// through client (the connectors' shared client: CA bundle, proxy, rate limits), with the PAT
// pat resolves added to each one, so a rotated PAT is picked up.
func NewClient(collection string, client *http.Client, pat func(context.Context) (secrets.Secret, bool, error)) *Client {
	host := ""
	if u, err := url.Parse(collection); err == nil {
		host = u.Host
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	authed := &http.Client{Transport: &patTransport{base: transport, host: host, pat: pat}, Timeout: client.Timeout}
	c := &Client{collection: strings.TrimSuffix(collection, "/"), http: authed}
	// The token is added by patTransport: the connection itself carries none.
	c.sdk = azuredevops.NewClientWithOptions(&azuredevops.Connection{BaseUrl: c.collection, SuppressFedAuthRedirect: true},
		c.collection, azuredevops.WithHTTPClient(authed))
	return c
}

// patTransport authenticates the calls to the instance's host with the PAT, as Basic
// authentication with an empty user name.
type patTransport struct {
	base http.RoundTripper
	host string
	pat  func(context.Context) (secrets.Secret, bool, error)
}

func (t *patTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == t.host {
		pat, ok, err := t.pat(r.Context())
		if err != nil {
			return nil, err
		}
		if ok {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", BasicAuth(pat.Reveal()))
		}
	}
	return t.base.RoundTrip(r)
}

// BasicAuth is the Authorization value of a PAT: Basic, with an empty user name.
func BasicAuth(pat string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+pat))
}

// Error is an error the API answered.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("azure devops: %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("azure devops: %d %s", e.Status, e.Message)
}

// Unwrap maps the statuses connectors share an error for.
func (e *Error) Unwrap() error {
	if e.Status == http.StatusNotFound {
		return connectors.ErrNotFound
	}
	return nil
}

// Do calls path (under the collection: /{project}/_apis/…) with the method, the API version
// and the query, sending in as JSON and decoding the answer into out (both may be nil).
func (c *Client) Do(ctx context.Context, method, path, version string, query url.Values, in, out any) error {
	if query == nil {
		query = url.Values{}
	}
	if version != "" {
		query.Set("api-version", version)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.collection+path+"?"+query.Encode(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-TFS-FedAuthRedirect", "Suppress") // a 401, not a sign-in page
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return decode(resp, out)
}

// Send calls the resource at location, as the SDK routes it, with the API version given and
// route values, sending in as JSON and decoding the answer into out.
func (c *Client) Send(ctx context.Context, method string, location uuid.UUID, version string, route map[string]string, query url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.sdk.Send(ctx, method, location, version, route, query, body, "application/json", "application/json", nil)
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	switch {
	case resp != nil && resp.StatusCode/100 != 2:
		return &Error{Status: resp.StatusCode, Message: sdkMessage(err)}
	case err != nil:
		return err
	}
	return decode(resp, out)
}

// sdkMessage returns the message of an error the SDK read from an answer.
func sdkMessage(err error) string {
	var value azuredevops.WrappedError
	var pointer *azuredevops.WrappedError
	switch {
	case errors.As(err, &value) && value.Message != nil:
		return *value.Message
	case errors.As(err, &pointer) && pointer.Message != nil:
		return *pointer.Message
	}
	return ""
}

// decode reads a successful answer into out, or the error an unsuccessful one carries.
func decode(resp *http.Response, out any) error {
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
		if e.Message == "" && resp.StatusCode == http.StatusUnauthorized {
			e.Message = "the personal access token was refused: missing, expired or revoked"
		}
		return &Error{Status: resp.StatusCode, Message: e.Message}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("azure devops: %s %s: %w", resp.Request.Method, resp.Request.URL.Path, err)
	}
	return nil
}

// Detect reads what the instance is, once: its kind (connectionData), the newest Git API version
// it serves (the location service), and whom the token authenticates. An instance older than
// Server 2022 is refused.
func (c *Client) Detect(ctx context.Context) (Instance, error) {
	c.mu.Lock()
	if c.instance != nil {
		defer c.mu.Unlock()
		return *c.instance, nil
	}
	c.mu.Unlock()
	var data struct {
		DeploymentType    string `json:"deploymentType"`
		AuthenticatedUser struct {
			ID                  string `json:"id"`
			ProviderDisplayName string `json:"providerDisplayName"`
			Properties          struct {
				Account struct {
					Value string `json:"$value"`
				} `json:"Account"`
			} `json:"properties"`
		} `json:"authenticatedUser"`
	}
	if err := c.Do(ctx, http.MethodGet, "/_apis/connectionData", "", nil, nil, &data); err != nil {
		return Instance{}, fmt.Errorf("azure devops %s: %w", c.collection, err)
	}
	var locations struct {
		Value []azuredevops.ApiResourceLocation `json:"value"`
	}
	if err := c.Do(ctx, http.MethodOptions, "/_apis", "", nil, nil, &locations); err != nil {
		return Instance{}, fmt.Errorf("azure devops %s: the location service: %w", c.collection, err)
	}
	in := Instance{Services: data.DeploymentType == "hosted", User: User{ID: data.AuthenticatedUser.ID, Name: data.AuthenticatedUser.ProviderDisplayName,
		Email: data.AuthenticatedUser.Properties.Account.Value}}
	for _, l := range locations.Value {
		if l.Id != nil && *l.Id == gitRepositories && l.MaxVersion != nil {
			in.MaxVersion = *l.MaxVersion
		}
	}
	if in.MaxVersion == "" {
		return Instance{}, fmt.Errorf("azure devops %s: the instance does not serve Git repositories", c.collection)
	}
	v, err := azuredevops.NewVersion(in.MaxVersion)
	if err != nil {
		return Instance{}, fmt.Errorf("azure devops %s: version %q: %w", c.collection, in.MaxVersion, err)
	}
	if v.CompareTo(MinVersion) < 0 {
		return Instance{}, fmt.Errorf("azure devops %s serves API %s: Quai needs %s (Azure DevOps Server 2022 or later)", c.collection, in.MaxVersion, MinVersion)
	}
	c.mu.Lock()
	c.instance = &in
	c.mu.Unlock()
	return in, nil
}
