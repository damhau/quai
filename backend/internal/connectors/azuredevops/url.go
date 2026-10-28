package azuredevops

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/damhau/quai/backend/internal/connectors"
)

// Location is where a repository lives in Azure DevOps.
type Location struct {
	Services   bool   // dev.azure.com or *.visualstudio.com; otherwise a Server
	Collection string // the collection's (Server) or organization's (Services) URL: API calls start there
	Project    string
	Repository string
}

// ParseURL reads a repository URL (docs/design/12-git-connectors.md#urls):
//
//	https://devops.acme.ch/tfs/DefaultCollection/Infrastructure/_git/live   Server, virtual directory tfs
//	https://devops.acme.ch/DefaultCollection/Infrastructure/_git/live       Server, no virtual directory
//	https://dev.azure.com/acme/Infrastructure/_git/live                     Services
//	https://acme.visualstudio.com/Infrastructure/_git/live                  Services, legacy
//
// A repository named after its project may leave the project out (…/acme/_git/Infrastructure).
// Names may hold spaces and non-ASCII characters: they are decoded here and encoded again in
// every API URL.
func ParseURL(raw string) (Location, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return Location{}, fmt.Errorf("azure devops: %q is not an HTTPS repository URL", redact(raw))
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	git := -1
	for i, s := range segs {
		if s == "_git" {
			git = i
		}
	}
	if git < 0 || git != len(segs)-2 || segs[git+1] == "" {
		return Location{}, fmt.Errorf("azure devops: %q is not a repository URL (…/{project}/_git/{repository})", redact(raw))
	}
	loc := Location{Repository: segs[git+1]}
	host := strings.ToLower(u.Hostname())
	before := segs[:git] // [virtual directory] collection [project], or organization [project] on Services
	base := url.URL{Scheme: u.Scheme, Host: u.Host}
	switch {
	case host == "dev.azure.com":
		loc.Services = true
		if len(before) < 1 || len(before) > 2 {
			return Location{}, fmt.Errorf("azure devops: %q is not dev.azure.com/{organization}/{project}/_git/{repository}", redact(raw))
		}
		base.Path = "/" + before[0]
		before = before[1:]
	case strings.HasSuffix(host, ".visualstudio.com"):
		loc.Services = true
		// The legacy form may name the DefaultCollection, which the organization URL leaves out.
		if len(before) > 0 && strings.EqualFold(before[0], "DefaultCollection") {
			before = before[1:]
		}
		if len(before) > 1 {
			return Location{}, fmt.Errorf("azure devops: %q is not {organization}.visualstudio.com/{project}/_git/{repository}", redact(raw))
		}
	default:
		// The last segment before _git is the project, unless only the collection is left (a
		// repository named after its project): the segments before are the collection's path.
		if len(before) == 0 {
			return Location{}, fmt.Errorf("azure devops: %q names no collection", redact(raw))
		}
		if len(before) >= 2 {
			loc.Project = before[len(before)-1]
			before = before[:len(before)-1]
		}
		base.Path = "/" + strings.Join(before, "/")
		before = nil
	}
	if len(before) == 1 {
		loc.Project = before[0]
	}
	if loc.Project == "" {
		loc.Project = loc.Repository
	}
	base.User = nil
	loc.Collection = strings.TrimSuffix(base.String(), "/")
	return loc, nil
}

func redact(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return connectors.Redact(u)
	}
	return "the URL"
}

// path joins segments into a URL path under the collection, each one encoded.
func path(segments ...string) string {
	var b strings.Builder
	for _, s := range segments {
		b.WriteString("/")
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}
