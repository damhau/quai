package repositories

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/ssh"

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
	Connector     string         `json:"connector" yaml:"connector"`
	URL           string         `json:"url" yaml:"url"`
	DefaultBranch string         `json:"defaultBranch,omitempty" yaml:"defaultBranch"`
	Credentials   Credentials    `json:"credentials,omitzero" yaml:"credentials"`
	Definitions   Definitions    `json:"definitions,omitzero" yaml:"definitions"`
	Delivery      Delivery       `json:"delivery,omitzero" yaml:"delivery"`
	Enforcement   Enforcement    `json:"enforcement,omitzero" yaml:"enforcement"`
	Codeowners    CodeownersSpec `json:"codeowners,omitzero" yaml:"codeowners"`
	Runner        Runner         `json:"runner,omitzero" yaml:"runner"`
	Webhooks      Webhooks       `json:"webhooks,omitzero" yaml:"webhooks"`
	Sync          Sync           `json:"sync,omitzero" yaml:"sync"`
}

// Credentials name the secret Quai authenticates to the platform with. KnownHosts holds the
// SSH host keys of the remote (known_hosts lines), for an SSH deploy key: they are verified. AppID
// is the GitHub App whose private key Ref names: public, and what its JWTs say they come from.
type Credentials struct {
	Ref        string `json:"ref,omitempty" yaml:"ref"`
	KnownHosts string `json:"knownHosts,omitempty" yaml:"knownHosts"`
	AppID      int64  `json:"appId,omitempty" yaml:"appId"`
}

// SecretRef names a secret in a secret store (env:, k8s:, vault:, azkv:), never the secret itself.
type SecretRef struct {
	Ref string `json:"ref,omitempty" yaml:"ref"`
}

// Definitions says where the repository's types and blueprints live.
type Definitions struct {
	Path   string `json:"path,omitempty" yaml:"path"`
	Branch string `json:"branch,omitempty" yaml:"branch"`
}

// Delivery says how changes reach the repository.
type Delivery struct {
	Mode          string   `json:"mode,omitempty" yaml:"mode"` // pull-request (the default where the platform has them) or direct
	BranchPrefix  string   `json:"branchPrefix,omitempty" yaml:"branchPrefix"`
	Merge         string   `json:"merge,omitempty" yaml:"merge"`             // auto or manual
	MergeMethod   string   `json:"mergeMethod,omitempty" yaml:"mergeMethod"` // merge, squash or rebase
	WaitForChecks []string `json:"waitForChecks,omitempty" yaml:"waitForChecks"`
	DeleteBranch  *bool    `json:"deleteBranch,omitempty" yaml:"deleteBranch"`
}

// Enforcement says what Quai verifies on the target branch, and the rules every type inherits.
type Enforcement struct {
	RequiredCheck string      `json:"requiredCheck,omitempty" yaml:"requiredCheck"`
	FloorRules    []FloorRule `json:"floorRules,omitempty" yaml:"floorRules"`
	// SeparationOfDuties are the repository's separation-of-duties options, which no type can set
	// (docs/design/05-delivery-and-approvals.md#separation-of-duties).
	SeparationOfDuties SeparationOfDuties `json:"separationOfDuties,omitzero" yaml:"separationOfDuties"`
}

// SeparationOfDuties are the options of docs/design/05-delivery-and-approvals.md#separation-of-duties,
// on top of the rules that always hold (no self-approval, one person counts once).
type SeparationOfDuties struct {
	// DefinitionAuthors is a period: who changed the definitions a request is planned with, in
	// that period before it is planned, cannot review it. Zero for none.
	DefinitionAuthors Period `json:"definitionAuthors,omitzero" yaml:"definitionAuthors" doc:"A period such as 7d (days) or 36h, at most 90d: who changed a request's definitions in that period before it was planned cannot review it"`
	// Destroys is requireTwo: a request whose plan destroys something, or has no plan yet, needs
	// two different approvers.
	Destroys string `json:"destroys,omitempty" yaml:"destroys" enum:"requireTwo" doc:"requireTwo: a request whose plan destroys something, or that has no plan yet, needs two different approvers"`
}

// RequireTwo is the destroys option: two different approvers for a plan that destroys something.
const RequireTwo = "requireTwo"

// MaxDefinitionAuthors bounds the definitionAuthors period (open question 72).
const MaxDefinitionAuthors = Period(90 * 24 * time.Hour)

// Period is a positive time.Duration written in whole days ("7d") or as Go writes durations
// ("36h", "90m").
type Period time.Duration

func (p Period) String() string {
	d := time.Duration(p)
	if d > 0 && d%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
	return d.String()
}

func (p Period) MarshalJSON() ([]byte, error) { return json.Marshal(p.String()) }

func (p *Period) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	return p.parse(s)
}

func (p *Period) UnmarshalYAML(n *yaml.Node) error { return p.parse(n.Value) }

func (p *Period) parse(s string) error {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseInt(days, 10, 32)
		if err != nil {
			return fmt.Errorf("%q is not a period such as 7d or 36h", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("%q is not a period such as 7d or 36h", s)
		}
		d = v
	}
	if d <= 0 {
		return fmt.Errorf("%q is not a positive period", s)
	}
	*p = Period(d)
	return nil
}

// FloorRule is an approval rule every type of the repository inherits and cannot remove.
type FloorRule struct {
	Name      string   `json:"name" yaml:"name"`
	When      string   `json:"when,omitempty" yaml:"when"`
	Approvers []string `json:"approvers" yaml:"approvers"`
	Min       int      `json:"min,omitempty" yaml:"min"`
}

// CodeownersSpec says how the owners the platform names (CODEOWNERS entries, Azure DevOps' required
// reviewers) match Quai's users and groups, for the codeowners approver kind and
// ownership.codeowners (docs/design/08-identity-access-tenancy.md#directory).
type CodeownersSpec struct {
	// Mapping maps an owner as the platform names it (@org/team, @alice, [Project]\Group, an
	// account name) to group:<name> or user:<name>. A platform group matches no one without it.
	Mapping map[string]string `json:"mapping,omitempty" yaml:"mapping"`
}

// Runner says where plan and apply results come from (docs/design/06).
type Runner struct {
	Kind        string    `json:"kind,omitempty" yaml:"kind"`
	ReportToken SecretRef `json:"reportToken,omitzero" yaml:"reportToken"` // the token runners report with
	// PlanValues keeps the before and after values of the attributes a plan changes, except
	// sensitive ones; without it, Quai keeps only their names.
	PlanValues bool `json:"planValues,omitempty" yaml:"planValues"`
	// Pipelines say which of the platform's pipelines plan, apply or check drift for which
	// stacks (open question 8's default, docs/design/06-runs-status-and-drift.md#adapters).
	Pipelines []Pipeline `json:"pipelines,omitempty" yaml:"pipelines"`
	// DriftInterval is how often each stack's drift is checked, with the drift-check pipeline
	// (docs/design/06-runs-status-and-drift.md#drift); 24h by default.
	DriftInterval Duration `json:"driftInterval,omitzero" yaml:"driftInterval"`
}

// Pipeline is one of the platform's pipelines, as the runner adapter reads its runs.
type Pipeline struct {
	Name string `json:"name" yaml:"name"` // the pipeline's name on the platform
	Kind string `json:"kind" yaml:"kind" enum:"plan,apply,drift-check"`
	// Stacks are the stacks a run covers: one stack, or a pattern like a type's (envs/{env}),
	// each stack of the repository's resources that it matches.
	Stacks string `json:"stacks" yaml:"stacks"`
}

// Webhooks holds the secret the platform's webhooks carry.
type Webhooks struct {
	Secret SecretRef `json:"secret,omitzero" yaml:"secret"`
}

// Sync bounds how often Quai fetches the repository, and what.
type Sync struct {
	PollInterval Duration `json:"pollInterval,omitempty" yaml:"pollInterval"` // the safety net for lost webhooks
	MinInterval  Duration `json:"minInterval,omitempty" yaml:"minInterval"`   // at most one fetch per interval on reads
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
	nameRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	refRe    = regexp.MustCompile(`^(env|k8s|vault|azkv):\S+$`)
	mappedRe = regexp.MustCompile(`^(group|user):\S+$`)
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
		// Pull requests by default (ADR 0008); generic Git has none.
		dl.Mode = "pull-request"
		if s.Connector == "generic" {
			dl.Mode = "direct"
		}
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
	if err := checkKnownHosts(s.Credentials.KnownHosts); err != nil {
		add("spec.credentials.knownHosts: %v", err)
	}
	if s.Credentials.AppID != 0 && (s.Connector != "github" || s.Credentials.AppID < 0) {
		add("spec.credentials.appId is the ID of a GitHub App, for the github connector")
	}
	if s.Runner.DriftInterval != 0 && s.Runner.DriftInterval < Duration(time.Hour) {
		add("spec.runner.driftInterval must be at least 1h")
	}
	drift := 0
	for _, p := range s.Runner.Pipelines {
		if p.Kind == "drift-check" {
			drift++
		}
	}
	if drift > 1 {
		add("spec.runner.pipelines names more than one drift-check pipeline")
	}
	for i, p := range s.Runner.Pipelines {
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Stacks) == "" {
			add("spec.runner.pipelines[%d] needs a name and stacks", i)
		}
		if !slices.Contains([]string{"plan", "apply", "drift-check"}, p.Kind) {
			add("spec.runner.pipelines[%d].kind must be plan, apply or drift-check", i)
		}
	}
	switch s.Delivery.Mode {
	case "direct":
	case "pull-request":
		if s.Connector == "generic" {
			add("spec.delivery.mode: generic Git has no pull requests; use direct")
		}
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
	seen := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(s.Codeowners.Mapping)) {
		v := s.Codeowners.Mapping[k]
		key := ownerKey(k)
		switch {
		case key == "":
			add("spec.codeowners.mapping: an owner's name is empty")
		case !mappedRe.MatchString(v):
			add("spec.codeowners.mapping[%q] must be group:<name> or user:<name>", k)
		case seen[key] != "":
			add("spec.codeowners.mapping names one owner twice, %q and %q: owners match without regard to case or a leading @", seen[key], k)
		}
		seen[key] = k
	}
	env, err := expr.NewEnv(expr.WithPlan()) // floor rules are approval rules
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
	if sod := s.Enforcement.SeparationOfDuties; sod.DefinitionAuthors < 0 || sod.DefinitionAuthors > MaxDefinitionAuthors {
		add("spec.enforcement.separationOfDuties.definitionAuthors must be a positive period of at most %s", MaxDefinitionAuthors)
	}
	if d := s.Enforcement.SeparationOfDuties.Destroys; d != "" && d != RequireTwo {
		add("spec.enforcement.separationOfDuties.destroys must be %s", RequireTwo)
	}
	if len(errs) > 0 {
		return &InvalidError{Name: d.Metadata.Name, Problems: errs}
	}
	return nil
}

// InvalidError lists what is wrong with a Repository document.
type InvalidError struct {
	Name     string
	Problems []string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("repository %q: %s", e.Name, strings.Join(e.Problems, "; "))
}

// checkKnownHosts checks known_hosts lines.
func checkKnownHosts(lines string) error {
	rest := []byte(lines)
	for len(bytes.TrimSpace(rest)) > 0 {
		var err error
		if _, _, _, _, rest, err = ssh.ParseKnownHosts(rest); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
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
