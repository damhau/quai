package main

import (
	"slices"
	"testing"
)

func TestStacksOf(t *testing.T) {
	got := stacksOf([]string{"envs/prod/vms/web01.yaml", "envs/dev/vms/a.yaml", "envs/prod/main.tf", ".quai/types/vm.yaml", "envs/README.md"})
	if want := []string{"envs/dev", "envs/prod"}; !slices.Equal(got, want) {
		t.Errorf("stacksOf = %v, want %v", got, want)
	}
}
