package main

import (
	"slices"
	"testing"
)

func TestStacksOf(t *testing.T) {
	got := stacksOf([]string{"envs/prod/vms/web01.yaml", "envs/dev/vms/a.yaml", "envs/prod/main.tf", ".quai/types/vm.yaml", "envs/README.md",
		"clusters/dev-1/namespaces.yaml", "dns/shop.internal/records.auto.tfvars", "databases/prod/databases.auto.tfvars.json", "modules/namespace/main.tf"})
	if want := []string{"clusters/dev-1", "databases/prod", "dns/shop.internal", "envs/dev", "envs/prod"}; !slices.Equal(got, want) {
		t.Errorf("stacksOf = %v, want %v", got, want)
	}
	if !recordedPlan("envs/prod") || recordedPlan("clusters/dev-1") {
		t.Error("only the stacks of envs/ carry the recorded plan")
	}
}
