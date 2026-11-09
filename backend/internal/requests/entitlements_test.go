package requests

import (
	"reflect"
	"testing"

	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/engine/definitions"
)

func TestEntitlementOf(t *testing.T) {
	e := &definitions.Entitlements{Visibility: []string{"group:dev*"}, Requesters: []string{"group:developers"},
		Environments: map[string][]string{"prod": {"group:shop"}}}
	item := authz.Entitlement{Visibility: e.Visibility, Requesters: e.Requesters}
	prod := authz.Entitlement{Visibility: e.Visibility, Requesters: e.Requesters, Environment: "prod", EnvironmentRequesters: []string{"group:shop"}}
	for env, want := range map[string]authz.Entitlement{
		"":        item,
		"dev":     item,
		"prod":    prod,
		" prod\t": prod, // as checkParams trims it
		"PROD":    prod,
	} {
		if got := EntitlementOf(e, env); !reflect.DeepEqual(got, want) {
			t.Errorf("EntitlementOf(%q) = %+v, want %+v", env, got, want)
		}
	}
	if got := EntitlementOf(nil, "prod"); !reflect.DeepEqual(got, authz.Entitlement{}) {
		t.Errorf("EntitlementOf(nil) = %+v", got)
	}
	for v, want := range map[any]string{"prod": "prod", 3.0: "3", true: "true", nil: ""} {
		if got := EnvValue(v); got != want {
			t.Errorf("EnvValue(%#v) = %q, want %q", v, got, want)
		}
	}
	if got := EnvValue(map[string]any{"a": 1}); got != "" {
		t.Errorf("EnvValue(map) = %q", got)
	}
}
