package definitions

import (
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

// The Go form of the quai.dev/v1beta1 definitions in docs/design/02-resource-types-and-generators.md
// and docs/design/04-blueprints-and-change-sets.md. Fields with a json tag are read from the
// definition; fields tagged "-" hold what Load compiles from them.

// Metadata names and describes a definition.
type Metadata struct {
	Name        string   `json:"name"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Icon        string   `json:"icon,omitempty"`
	Category    string   `json:"category,omitempty"`
	Version     string   `json:"version,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Owners      []string `json:"owners,omitempty"`
}

// ResourceType describes one kind of thing in one repository.
type ResourceType struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       TypeSpec `json:"spec"`

	// Path is the definition's file in the repository.
	Path string `json:"-"`
	// Files are the repository files the type is made of: its own and its schemas'. Digest
	// fingerprints them.
	Files []string `json:"-"`
	// FileSchema validates the content of the type's files; InputSchema validates form values.
	// They are the same schema when the type has no separate input schema.
	FileSchema  *schema.Schema `json:"-"`
	InputSchema *schema.Schema `json:"-"`
}

// TypeSpec is the body of a resource type.
type TypeSpec struct {
	Locate      Locate            `json:"locate"`
	Params      []Param           `json:"params,omitempty"`
	Schema      string            `json:"schema"`
	Inputs      string            `json:"inputs,omitempty"`
	Generate    Generate          `json:"generate"`
	Ownership   *Ownership        `json:"ownership,omitempty"`
	ParamFields map[string]string `json:"paramFields,omitempty"` // param name → field that mirrors it
	Labels      map[string]string `json:"labels,omitempty"`
	Stack       string            `json:"stack,omitempty"`
	Address     string            `json:"address,omitempty"`
	Deletion    string            `json:"deletion,omitempty"`
	Catalog     string            `json:"catalog,omitempty"`
	UI          UI                `json:"ui"`
	Approval    Approval          `json:"approval"`
	Actions     []Action          `json:"actions,omitempty"`
}

// Locator strategies.
const (
	LocateFile  = "file"
	LocateDir   = "dir"
	LocateEntry = "entry"
	LocateField = "field"
)

// Locate says where instances of a type live.
type Locate struct {
	Strategy   string   `json:"strategy"`
	Path       string   `json:"path"`
	Collection string   `json:"collection,omitempty"`
	Key        string   `json:"key,omitempty"`
	KeyParam   string   `json:"keyParam,omitempty"`
	Fields     []string `json:"fields,omitempty"`
	Order      string   `json:"order,omitempty"`
}

// Locator returns the locator the spec describes.
func (lo Locate) Locator() (*locate.Locator, error) {
	switch lo.Strategy {
	case LocateEntry:
		return locate.NewEntry(lo.Path, lo.Collection, lo.Key, lo.KeyParam)
	case LocateField:
		return locate.NewField(lo.Path, lo.Collection, lo.Key, lo.KeyParam, lo.Fields)
	}
	return locate.New(lo.Strategy, lo.Path)
}

// Param is a locator parameter, such as the environment and the name in envs/{env}/vms/{name}.yaml.
type Param struct {
	Name        string   `json:"name"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type,omitempty"` // string (the default) or enum
	Options     []string `json:"options,omitempty"`
	Discover    bool     `json:"discover,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
}

// Generator kinds.
const (
	GenerateIdentity  = "identity"
	GenerateMapping   = "mapping"
	GenerateComponent = "component"
	GeneratePlugin    = "plugin"
)

// Generate says how form values become file content and back.
type Generate struct {
	Kind      string            `json:"kind"`
	Template  map[string]any    `json:"template,omitempty"`
	Read      map[string]string `json:"read,omitempty"`
	Component string            `json:"component,omitempty"`
	With      map[string]any    `json:"with,omitempty"`
	Module    string            `json:"module,omitempty"`
	Digest    string            `json:"digest,omitempty"`
	Config    map[string]any    `json:"config,omitempty"`

	TemplateValue *expr.Value               `json:"-"`
	ReadTemplates map[string]*expr.Template `json:"-"`
}

// Ownership says who owns each resource: a field of the file, CODEOWNERS, or a fixed team.
type Ownership struct {
	Field      string `json:"field,omitempty"`
	Codeowners bool   `json:"codeowners,omitempty"`
	Team       string `json:"team,omitempty"`
}

// UI holds form hints.
type UI struct {
	Order  []string           `json:"order,omitempty"`
	Fields map[string]UIField `json:"fields,omitempty"`
}

// UIField holds the hints for one field, by dotted path.
type UIField struct {
	Immutable bool   `json:"immutable,omitempty"`
	VisibleIf string `json:"visibleIf,omitempty"`

	Visible *expr.Program `json:"-"`
}

// Approval holds approval rules (docs/design/05-delivery-and-approvals.md#approval-policy).
type Approval struct {
	Rules  []Rule `json:"rules,omitempty"`
	Expiry string `json:"expiry,omitempty"` // such as 7d
	StepUp string `json:"stepUp,omitempty"` // a condition: approvers must have re-authenticated

	StepUpCondition *expr.Program `json:"-"`
}

// Rule requires approvals from approvers, or from an external system, when its condition holds.
type Rule struct {
	Name      string   `json:"name"`
	When      string   `json:"when,omitempty"`
	Approvers []string `json:"approvers,omitempty"` // group:<name>, user:<name>, owners, codeowners, manager
	Min       int      `json:"min,omitempty"`
	External  string   `json:"external,omitempty"`
	Distinct  *bool    `json:"distinct,omitempty"`

	Condition *expr.Program `json:"-"`
}

// Action kinds.
const (
	ActionDeclarative = "declarative"
	ActionImperative  = "imperative"
)

// Action is a day-2 action of a type.
type Action struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Icon        string         `json:"icon,omitempty"`
	Description string         `json:"description,omitempty"`
	Kind        string         `json:"kind,omitempty"` // declarative (the default) or imperative
	AvailableIf string         `json:"availableIf,omitempty"`
	Inputs      []Input        `json:"inputs,omitempty"`
	Set         map[string]any `json:"set,omitempty"` // dotted field path → value
	Executor    string         `json:"executor,omitempty"`
	Operation   string         `json:"operation,omitempty"`
	Target      map[string]any `json:"target,omitempty"`
	Approval    *Approval      `json:"approval,omitempty"` // nil: the type's rules apply

	Available   *expr.Program `json:"-"`
	SetValue    *expr.Value   `json:"-"`
	TargetValue *expr.Value   `json:"-"`
}

// Input types of action inputs.
const (
	InputString  = "string"
	InputInteger = "integer"
	InputNumber  = "number"
	InputBoolean = "boolean"
	InputEnum    = "enum"
)

// Input is one input of an action's form.
type Input struct {
	Name        string   `json:"name"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`
	Options     []string `json:"options,omitempty"`
	Default     any      `json:"default,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	VisibleIf   string   `json:"visibleIf,omitempty"`

	Visible *expr.Program `json:"-"`
}

// Blueprint is a catalog item made of resources of existing types.
type Blueprint struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   Metadata      `json:"metadata"`
	Spec       BlueprintSpec `json:"spec"`

	Path        string         `json:"-"`
	Files       []string       `json:"-"` // its own, its input schema's and its types' files
	InputSchema *schema.Schema `json:"-"`
}

// BlueprintSpec is the body of a blueprint.
type BlueprintSpec struct {
	Inputs    map[string]any      `json:"inputs"` // a JSON Schema; its $refs resolve from the blueprint's file
	UI        UI                  `json:"ui"`
	Resources []BlueprintResource `json:"resources"`
	Approval  Approval            `json:"approval"`
	Actions   []BlueprintAction   `json:"actions,omitempty"`
}

// BlueprintResource is one resource a blueprint creates.
type BlueprintResource struct {
	ID        string         `json:"id"`
	Type      TypeRef        `json:"type"`
	Params    map[string]any `json:"params,omitempty"`
	Values    map[string]any `json:"values,omitempty"`
	When      string         `json:"when,omitempty"`
	DependsOn []string       `json:"dependsOn,omitempty"`

	Condition   *expr.Program `json:"-"`
	ParamsValue *expr.Value   `json:"-"`
	ValuesValue *expr.Value   `json:"-"`
}

// TypeRef names a resource type. An empty Repository is the blueprint's own repository.
type TypeRef struct {
	Repository string `json:"repository,omitempty"`
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
}

// BlueprintAction is a day-2 action of a blueprint, fanned out to its resources.
type BlueprintAction struct {
	Name        string                    `json:"name"`
	Title       string                    `json:"title,omitempty"`
	Icon        string                    `json:"icon,omitempty"`
	Description string                    `json:"description,omitempty"`
	Inputs      []Input                   `json:"inputs,omitempty"`
	Apply       map[string]ResourceChange `json:"apply"` // resource id → change
	Approval    *Approval                 `json:"approval,omitempty"`
}

// ResourceChange is what a blueprint action does to one resource: set values, or run one of the
// type's actions.
type ResourceChange struct {
	Values map[string]any `json:"values,omitempty"`
	Action string         `json:"action,omitempty"`
	Inputs map[string]any `json:"inputs,omitempty"`

	ValuesValue *expr.Value `json:"-"`
	InputsValue *expr.Value `json:"-"`
}
