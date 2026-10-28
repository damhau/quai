package repositories

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/damhau/quai/backend/internal/engine/expr"
)

// The Repository document of docs/design/01-repository-connections.md.
const (
	APIVersion = "quai.dev/v1beta1"
	Kind       = "Repository"
)

// Document is a repository connection as declared: in a configuration file or through the API.
type Document struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion"`
	Kind       string   `json:"kind" yaml:"kind"`
	Metadata   Metadata `json:"metadata" yaml:"metadata"`
	Spec       Spec     `json:"spec" yaml:"spec"`
}

// Metadata names a repository connection.
type Metadata struct {
	// Name is part of every resource ID and request record, so it never changes.
	Name   string   `json:"name" yaml:"name"`
	Title  string   `json:"title,omitempty" yaml:"title"`
	Owners []string `json:"owners,omitempty" yaml:"owners"` // who administers the connection
}

// Spec is how Quai works with the repository.
type Spec struct {
	Connector     string      `json:"connector" yaml:"connector"`
	URL           string      `json:"url" yaml:"url"`
	DefaultBranch string      `json:"defaultBranch" yaml:"defaultBranch"`
	Credentials   SecretRef   `json:"credentials,omitzero" yaml:"credentials"`
	Definitions   Definitions `json:"definitions" yaml:"definitions"`
	Delivery      Delivery    `json:"delivery" yaml:"delivery"`
	Enforcement   Enforcement `json:"enforcement" yaml:"enforcement"`
	Runner        Runner      `json:"runner,omitzero" yaml:"runner"`
	Webhooks      Webhooks    `json:"webhooks,omitzero" yaml:"webhooks"`
	Sync          Sync        `json:"sync" yaml:"sync"`
}

// SecretRef names a secret in a secret store (env:, k8s:, vault:, azkv:), never the secret itself.
type SecretRef struct {
	Ref string `json:"ref,omitempty" yaml:"ref"`
}

// Definitions says where the repository's types and blueprints live.
type Definitions struct {
	Path   string `json:"path" yaml:"path"`
	Branch string `json:"branch" yaml:"branch"`
}

// Delivery says how changes reach the repository.
type Delivery struct {
	Mode          string   `json:"mode" yaml:"mode"` // direct, or pull-request (P1-13)
	BranchPrefix  string   `json:"branchPrefix" yaml:"branchPrefix"`
	Merge         string   `json:"merge" yaml:"merge"`             // auto or manual
	MergeMethod   string   `json:"mergeMethod" yaml:"mergeMethod"` // merge, squash or rebase
	WaitForChecks []string `json:"waitForChecks,omitempty" yaml:"waitForChecks"`
	DeleteBranch  *bool    `json:"deleteBranch" yaml:"deleteBranch"`
}

// Enforcement says what Quai verifies on the target branch, and the rules every type inherits.
type Enforcement struct {
	RequiredCheck string      `json:"requiredCheck" yaml:"requiredCheck"`
	FloorRules    []FloorRule `json:"floorRules,omitempty" yaml:"floorRules"`
}

// FloorRule is an approval rule every type of the repository inherits and cannot remove.
type FloorRule struct {
	Name      string   `json:"name" yaml:"name"`
	When      string   `json:"when,omitempty" yaml:"when"`
	Approvers []string `json:"approvers" yaml:"approvers"`
	Min       int      `json:"min,omitempty" yaml:"min"`
}

// Runner says where plan and apply results come from (docs/design/06).
type Runner struct {
	Kind        string    `json:"kind,omitempty" yaml:"kind"`
	ReportToken SecretRef `json:"reportToken,omitzero" yaml:"reportToken"` // the token runners report with
}

// Webhooks holds the secret the platform's webhooks carry.
type Webhooks struct {
	Secret SecretRef `json:"secret,omitzero" yaml:"secret"`
}

// Sync bounds how often Quai fetches the repository, and what.
type Sync struct {
	PollInterval Duration `json:"pollInterval" yaml:"pollInterval"` // the safety net for lost webhooks
	MinInterval  Duration `json:"minInterval" yaml:"minInterval"`   // at most one fetch per interval on reads
	// PartialClone mirrors the history without file contents, fetched when needed: for large
	// repositories. It applies when the mirror is first cloned.
	PartialClone bool `json:"partialClone,omitempty" yaml:"partialClone"`
}

// Duration is a time.Duration written as text: "5m", "10s".
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	return d.parse(s)
}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error { return d.parse(n.Value) }

func (d *Duration) parse(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration such as 30s or 5m", s)
	}
	*d = Duration(v)
	return nil
}

// Connectors are the Git platforms a repository can be on (docs/design/12-git-connectors.md).
var Connectors = []string{"azure-devops", "github", "gitlab", "bitbucket", "gitea", "generic"}

var (
	nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	refRe  = regexp.MustCompile(`^(env|k8s|vault|azkv):\S+$`)
)

// ApplyDefaults fills what the document leaves out.
func (d *Document) ApplyDefaults() {
	m, s := &d.Metadata, &d.Spec
	if m.Title == "" {
		m.Title = m.Name
	}
	if s.DefaultBranch == "" {
		s.DefaultBranch = "main"
	}
	if s.Definitions.Path == "" {
		s.Definitions.Path = ".quai"
	}
	if s.Definitions.Branch == "" {
		s.Definitions.Branch = s.DefaultBranch
	}
	dl := &s.Delivery
	if dl.Mode == "" {
		dl.Mode = "direct" // pull-request becomes the default with P1-13 (ADR 0008)
	}
	if dl.BranchPrefix == "" {
		dl.BranchPrefix = "quai/"
	}
	if dl.Merge == "" {
		dl.Merge = "auto"
	}
	if dl.MergeMethod == "" {
		dl.MergeMethod = "squash"
	}
	if dl.DeleteBranch == nil {
		yes := true
		dl.DeleteBranch = &yes
	}
	if s.Enforcement.RequiredCheck == "" {
		s.Enforcement.RequiredCheck = "quai/approval"
	}
	if s.Sync.PollInterval == 0 {
		s.Sync.PollInterval = Duration(5 * time.Minute)
	}
	if s.Sync.MinInterval == 0 {
		s.Sync.MinInterval = Duration(10 * time.Second)
	}
}

// Validate checks a document with its defaults applied. It reports every problem, by field.
func (d *Document) Validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if d.APIVersion != APIVersion {
		add("apiVersion must be %s", APIVersion)
	}
	if d.Kind != Kind {
		add("kind must be %s", Kind)
	}
	s := d.Spec
	if !nameRe.MatchString(d.Metadata.Name) {
		add("metadata.name must be a lowercase DNS label")
	}
	if !slices.Contains(Connectors, s.Connector) {
		add("spec.connector must be one of %s", strings.Join(Connectors, ", "))
	}
	if s.URL == "" {
		add("spec.url is required")
	}
	for at, ref := range map[string]string{"spec.credentials.ref": s.Credentials.Ref, "spec.runner.reportToken.ref": s.Runner.ReportToken.Ref,
		"spec.webhooks.secret.ref": s.Webhooks.Secret.Ref} {
		if ref != "" && !refRe.MatchString(ref) {
			add("%s must be a secret reference such as k8s:namespace/name#key or env:NAME", at)
		}
	}
	switch s.Delivery.Mode {
	case "direct":
	case "pull-request":
		add("spec.delivery.mode must be direct until pull-request delivery exists (P1-13)")
	default:
		add("spec.delivery.mode must be direct or pull-request")
	}
	if !strings.HasSuffix(s.Delivery.BranchPrefix, "/") {
		add("spec.delivery.branchPrefix must end with /")
	}
	if !slices.Contains([]string{"auto", "manual"}, s.Delivery.Merge) {
		add("spec.delivery.merge must be auto or manual")
	}
	if !slices.Contains([]string{"merge", "squash", "rebase"}, s.Delivery.MergeMethod) {
		add("spec.delivery.mergeMethod must be merge, squash or rebase")
	}
	if s.Sync.PollInterval < Duration(time.Second) || s.Sync.MinInterval < 0 {
		add("spec.sync.pollInterval must be at least 1s and spec.sync.minInterval not negative")
	}
	env, err := expr.NewEnv()
	if err != nil {
		return err
	}
	for i, f := range s.Enforcement.FloorRules {
		if f.Name == "" || len(f.Approvers) == 0 {
			add("spec.enforcement.floorRules[%d] needs a name and approvers", i)
		}
		if f.When != "" {
			if _, err := env.CompileCondition(f.When); err != nil {
				add("spec.enforcement.floorRules[%d].when: %v", i, err)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("repository %q: %s", d.Metadata.Name, strings.Join(errs, "; "))
	}
	return nil
}

// Parse reads the Repository documents of a YAML stream, strictly: an unknown field is an error.
// It applies the defaults and validates each document.
func Parse(src []byte) ([]Document, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	var docs []Document
	for {
		var d Document
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		d.ApplyDefaults()
		if err := d.Validate(); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
}

// LoadFiles reads the Repository documents at path: a YAML file, or a directory whose .yaml and
// .yml files are read in name order. An empty path declares none. A name declared twice is an
// error.
func LoadFiles(path string) ([]Document, error) {
	if path == "" {
		return nil, nil
	}
	files := []string{path}
	if info, err := os.Stat(path); err != nil {
		return nil, err
	} else if info.IsDir() {
		files = nil
		for _, pattern := range []string{"*.yaml", "*.yml"} {
			m, _ := filepath.Glob(filepath.Join(path, pattern))
			files = append(files, m...)
		}
		slices.Sort(files)
	}
	var docs []Document
	seen := map[string]string{}
	for _, f := range files {
		src, err := os.ReadFile(f) // #nosec G304 -- files the operator configured
		if err != nil {
			return nil, err
		}
		ds, err := Parse(src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, d := range ds {
			if prev, ok := seen[d.Metadata.Name]; ok {
				return nil, fmt.Errorf("%s: repository %q is declared in %s already", f, d.Metadata.Name, prev)
			}
			seen[d.Metadata.Name] = f
			docs = append(docs, d)
		}
	}
	return docs, nil
}
