// Package expr compiles and evaluates the CEL expressions in definitions: the conditions
// (`when`, `visibleIf`, `availableIf`) and the `${{ … }}` bindings in templates.
//
// It implements docs/adr/0009-cel-expressions.md and the expression rules in
// docs/design/02-resource-types-and-generators.md. Expressions are compiled once, when a
// definition is loaded, against an Env that declares the variables they may use; a compiled
// Program is then evaluated with Vars as often as needed, concurrently if need be.
//
// CEL is stricter than the prototype's language: reading a missing key is an error, not
// undefined, so optional fields are tested with `has(previous.cpu)` or read with `inputs.?cpu`;
// `+` does not mix strings and numbers, so numbers are converted with `string(x)`; and
// conditions must evaluate to a bool.
//
// Values use the JSON data model in both directions: nil, bool, string, numbers, []any and
// map[string]any. Until variables are typed by their schemas, a whole number is an int and any
// other number a double, so `inputs.cpu * 2` works whether cpu came from YAML (4) or JSON (4.0).
package expr

import (
	"errors"
	"fmt"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/ext"
)

// The variables every expression can use.
const (
	VarInputs   = "inputs"   // the validated form values
	VarParams   = "params"   // the locator params (env, name)
	VarDoc      = "doc"      // the current region content
	VarPrevious = "previous" // the values before a day-2 change; empty on create
	VarAction   = "action"   // the action's own inputs
	VarRequest  = "request"  // kind, action, reason
	VarUser     = "user"     // name, groups
)

// Limits on what an expression may cost. They protect the server from expensive definitions;
// expressions in real definitions stay far below them.
const (
	// MaxExpressionSize is the longest expression accepted, in code points.
	MaxExpressionSize = 4096
	// CostLimit bounds the work of one evaluation, in CEL cost units: roughly one per operation,
	// plus the length of the strings and lists it touches. It is the per-call limit Kubernetes
	// applies to CRD validation rules.
	CostLimit = 1_000_000
)

// StringsVersion is the version of the CEL strings extension enabled in every Env. It is pinned
// so that a cel-go upgrade cannot change which functions definitions may call; version 5 is the
// first whose functions count toward CostLimit.
const StringsVersion = 5

var (
	anyMap    = cel.MapType(cel.StringType, cel.DynType)
	stringMap = cel.MapType(cel.StringType, cel.StringType)
)

// defaultTypes declares each variable until a schema gives it a more precise type.
func defaultTypes() map[string]*cel.Type {
	return map[string]*cel.Type{
		VarInputs:   anyMap,
		VarParams:   anyMap,
		VarDoc:      cel.DynType,
		VarPrevious: anyMap,
		VarAction:   anyMap,
		VarRequest:  stringMap,
		VarUser:     anyMap,
	}
}

// Env declares the variables expressions may use and their types. It is safe for concurrent use.
type Env struct {
	cel *cel.Env
}

// Option configures an Env.
type Option func(types map[string]*cel.Type) error

// WithType declares the type of one of the variables, so that expressions using it are
// type-checked when they are compiled. Without it, inputs, params, previous, action and user are
// maps of string to any value, request is a map of string to string, and doc is any value. It is
// the hook through which definitions give variables the types of their schemas.
func WithType(name string, t *cel.Type) Option {
	return func(types map[string]*cel.Type) error {
		if _, ok := types[name]; !ok {
			return fmt.Errorf("unknown variable %q", name)
		}
		if t == nil {
			return fmt.Errorf("variable %q: nil type", name)
		}
		types[name] = t
		return nil
	}
}

// NewEnv returns an environment declaring the variables of docs/design/02 with the CEL standard
// library, optional values (`inputs.?x`, `optional.none()`) and the strings extension.
func NewEnv(opts ...Option) (*Env, error) {
	vars := defaultTypes()
	for _, opt := range opts {
		if err := opt(vars); err != nil {
			return nil, err
		}
	}
	envOpts := []cel.EnvOption{
		cel.OptionalTypes(),
		ext.Strings(ext.StringsVersion(StringsVersion)),
		cel.CrossTypeNumericComparisons(true),
		cel.ParserExpressionSizeLimit(MaxExpressionSize),
	}
	for _, name := range []string{VarInputs, VarParams, VarDoc, VarPrevious, VarAction, VarRequest, VarUser} {
		envOpts = append(envOpts, cel.Variable(name, vars[name]))
	}
	env, err := cel.NewEnv(envOpts...)
	if err != nil {
		return nil, fmt.Errorf("CEL environment: %w", err)
	}
	return &Env{cel: env}, nil
}

// Error reports an expression that does not compile or fails to evaluate.
type Error struct {
	Expr string // the expression's source
	Err  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("expression %q: %v", e.Expr, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Program is a compiled expression. It is safe for concurrent use.
type Program struct {
	src       string
	prg       cel.Program // nil for an empty condition, which is true
	condition bool
}

// Compile type-checks an expression whose result may be of any type.
func (e *Env) Compile(src string) (*Program, error) {
	return e.compile(src, false)
}

// CompileCondition type-checks a `when`, `visibleIf` or `availableIf` expression, whose result
// must be a bool. An empty condition is true.
func (e *Env) CompileCondition(src string) (*Program, error) {
	if strings.TrimSpace(src) == "" {
		return &Program{src: src, condition: true}, nil
	}
	return e.compile(src, true)
}

func (e *Env) compile(src string, condition bool) (*Program, error) {
	if strings.TrimSpace(src) == "" {
		return nil, &Error{Expr: src, Err: errors.New("empty expression")}
	}
	ast, iss := e.cel.Compile(src)
	if err := iss.Err(); err != nil {
		return nil, &Error{Expr: src, Err: err}
	}
	if condition {
		out := ast.OutputType()
		if !out.IsExactType(cel.BoolType) && out.Kind() != types.DynKind {
			return nil, &Error{Expr: src, Err: fmt.Errorf("a condition must be a bool, not %s", out)}
		}
	}
	prg, err := e.cel.Program(ast, cel.CostLimit(CostLimit))
	if err != nil {
		return nil, &Error{Expr: src, Err: err}
	}
	return &Program{src: src, prg: prg, condition: condition}, nil
}

// String returns the expression's source.
func (p *Program) String() string { return p.src }

// Eval evaluates the expression. The result is in the JSON data model, or Absent when the
// expression evaluates to `optional.none()`.
func (p *Program) Eval(v Vars) (any, error) {
	act, err := v.activation()
	if err != nil {
		return nil, err
	}
	return p.eval(act)
}

// Test evaluates a condition.
func (p *Program) Test(v Vars) (bool, error) {
	act, err := v.activation()
	if err != nil {
		return false, err
	}
	return p.test(act)
}

func (p *Program) eval(act map[string]any) (any, error) {
	out, err := p.evalCEL(act)
	if err != nil {
		return nil, err
	}
	v, err := fromCEL(out, true)
	if err != nil {
		return nil, &Error{Expr: p.src, Err: err}
	}
	return v, nil
}

func (p *Program) test(act map[string]any) (bool, error) {
	out, err := p.evalCEL(act)
	if err != nil {
		return false, err
	}
	b, ok := out.(types.Bool)
	if !ok {
		return false, &Error{Expr: p.src, Err: fmt.Errorf("a condition must be a bool, not %s", out.Type().TypeName())}
	}
	return bool(b), nil
}

func (p *Program) evalCEL(act map[string]any) (ref.Val, error) {
	if p.prg == nil {
		return types.True, nil
	}
	out, _, err := p.prg.Eval(act)
	if err != nil {
		return nil, &Error{Expr: p.src, Err: err}
	}
	return out, nil
}

// Vars are the values of the variables for one evaluation. A nil map is an empty map, so on
// create `has(previous.cpu)` is false.
type Vars struct {
	Inputs   map[string]any
	Params   map[string]any
	Doc      any // nil is an empty map
	Previous map[string]any
	Action   map[string]any
	Request  Request
	User     User
}

// Request describes the request an expression is evaluated for.
type Request struct {
	Kind   string // create, update, delete, action
	Action string // the action's name, for kind action
	Reason string
}

// User is the person who makes the request.
type User struct {
	Name   string
	Groups []string
}

// activation converts the variables to the values CEL evaluates against.
func (v Vars) activation() (map[string]any, error) {
	act := make(map[string]any, 7)
	maps := []struct {
		name string
		m    map[string]any
	}{
		{VarInputs, v.Inputs}, {VarParams, v.Params}, {VarPrevious, v.Previous}, {VarAction, v.Action},
	}
	for _, m := range maps {
		n, err := normalize(m.m) // a nil map becomes an empty one
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.name, err)
		}
		act[m.name] = n
	}
	doc, err := normalize(v.Doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", VarDoc, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	act[VarDoc] = doc
	act[VarRequest] = map[string]string{"kind": v.Request.Kind, "action": v.Request.Action, "reason": v.Request.Reason}
	groups := make([]any, len(v.User.Groups))
	for i, g := range v.User.Groups {
		groups[i] = g
	}
	act[VarUser] = map[string]any{"name": v.User.Name, "groups": groups}
	return act, nil
}
