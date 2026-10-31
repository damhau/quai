package generate

import (
	"fmt"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/generate/component"
)

// Component returns, for a type with the component generator, the record of the values after
// the request, made as the identity generator makes a file (managed fields, locked fields, the
// schema), and the manifests its component renders: `with` is evaluated as data, seeing the
// record as inputs and the locator params as params, and gives the component's properties. A
// manifest the component refuses is the type's mistake, not the form's. It returns nil for a
// delete.
func Component(t *definitions.ResourceType, r Request) (map[string]any, []map[string]any, error) {
	g := t.Spec.Generate
	if g.Kind != definitions.GenerateComponent {
		return nil, nil, fmt.Errorf("type %s does not use the component generator", t.Metadata.Name)
	}
	comp := component.Get(g.Component)
	if comp == nil {
		return nil, nil, fmt.Errorf("type %s: the %s component is not available yet", t.Metadata.Name, g.Component)
	}
	record, err := Identity(t, r)
	if err != nil || record == nil {
		return nil, nil, err
	}
	props := map[string]any{}
	if g.WithValue != nil {
		v, err := g.WithValue.Eval(expr.Vars{Inputs: record, Params: anyMap(r.Params), Request: expr.Request{Kind: r.Kind}})
		if err != nil {
			return nil, nil, fmt.Errorf("type %s: spec.generate.with: %w", t.Metadata.Name, err)
		}
		props, _ = v.(map[string]any)
	}
	manifests, err := comp.Render(props)
	if err != nil {
		return nil, nil, fmt.Errorf("type %s: %w", t.Metadata.Name, err)
	}
	return record, manifests, nil
}
