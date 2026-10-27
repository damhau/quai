package plan

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

const smallVM = `owner: bob
template: ubuntu-24.04
cpu: 2            # sized by the blueprint
memory_gb: 4
disks:
  - size_gb: 50
network:
  vlan: 110
`

func withInstances(t *testing.T) (fstest.MapFS, *Instance, *Instance) {
	fsys, _ := loadExamples(t)
	fsys["envs/dev/vms/shop-web01.yaml"] = &fstest.MapFile{Data: []byte(smallVM)}
	fsys["envs/dev/vms/web42.yaml"] = &fstest.MapFile{Data: []byte(smallVM)}
	fsys[".quai/instances/app-servers/shop-dev.yaml"] = &fstest.MapFile{Data: []byte(`apiVersion: quai.dev/v1beta1
kind: Instance
metadata: { name: shop-dev, blueprint: app-servers, blueprintVersion: 1.0.0, owner: bob }
spec:
  inputs: { environment: dev, app: shop, webCpu: 2, withDatabase: false, dbDiskGb: 200 }
  resources:
    web: infra-live/vm/dev/shop-web01
`)}
	fsys[".quai/instances/linux-vm/web42.yaml"] = &fstest.MapFile{Data: []byte(`apiVersion: quai.dev/v1beta1
kind: Instance
metadata: { name: web42, blueprint: linux-vm, blueprintVersion: 2.0.0, owner: team:platform }
spec:
  inputs: { environment: dev, name: web42, os: ubuntu-24.04, size: small, diskGb: 50, vlan: 110 }
  resources: { vm: infra-live/vm/dev/web42 }
`)}
	shop, err := ReadInstance(fsys, InstancePath("app-servers", "shop-dev"))
	if err != nil {
		t.Fatal(err)
	}
	web42, err := ReadInstance(fsys, InstancePath("linux-vm", "web42"))
	if err != nil {
		t.Fatal(err)
	}
	return fsys, shop, web42
}

func TestReadInstance(t *testing.T) {
	fsys, shop, web42 := withInstances(t)
	if shop.Owner != "bob" || shop.Inputs["app"] != "shop" || shop.Resources["web"] != "infra-live/vm/dev/shop-web01" || web42.Owner != "team:platform" {
		t.Errorf("instances = %+v %+v", shop, web42)
	}
	if _, err := ReadInstance(fsys, InstancePath("app-servers", "nope")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing record: %v", err)
	}
	fsys[".quai/instances/app-servers/moved.yaml"] = fsys[InstancePath("app-servers", "shop-dev")]
	if _, err := ReadInstance(fsys, InstancePath("app-servers", "moved")); err == nil {
		t.Error("a record whose name is not its file name was read")
	}
}

func TestBlueprintActions(t *testing.T) {
	fsys, shop, web42 := withInstances(t)
	_, repo := loadExamples(t)
	user := expr.User{Name: "bob"}

	// A blueprint action that sets values: the rest of the file, comments included, stays.
	req := BlueprintActionRequest{Repository: "infra-live", Instance: web42, Action: "resize", Inputs: map[string]any{"size": "medium"}, User: user}
	cs, inputs, err := BlueprintAction(fsys, repo, repo.Blueprints["linux-vm"], req)
	if err != nil {
		t.Fatal(err)
	}
	after := string(cs.Changes[0].After)
	if inputs["size"] != "medium" || cs.Changes[0].Path != "envs/dev/vms/web42.yaml" || !strings.Contains(after, "cpu: 4            # sized by the blueprint\nmemory_gb: 16\n") ||
		!strings.Contains(after, "disks:\n  - size_gb: 50\n") || cs.Resources[0].Values["cpu"] != int64(4) {
		t.Errorf("resize = %+v\n%s", cs.Resources, after)
	}

	// A blueprint action that runs one of the type's actions.
	req = BlueprintActionRequest{Repository: "infra-live", Instance: shop, Action: "scale-web", Inputs: map[string]any{"cpu": 8}, User: user}
	if cs, _, err = BlueprintAction(fsys, repo, repo.Blueprints["app-servers"], req); err != nil {
		t.Fatal(err)
	}
	if after := string(cs.Changes[0].After); cs.Changes[0].Path != "envs/dev/vms/shop-web01.yaml" || !strings.Contains(after, "cpu: 8            # sized by the blueprint\nmemory_gb: 16\n") {
		t.Errorf("scale-web = %s", after)
	}

	// The action's own form is checked first, and its errors are fields.
	req.Inputs = map[string]any{"cpu": 64}
	var ve *schema.ValidationError
	var re *ResourceError
	if _, _, err := BlueprintAction(fsys, repo, repo.Blueprints["app-servers"], req); !errors.As(err, &ve) || ve.Fields["cpu"] == "" || errors.As(err, &re) {
		t.Errorf("an input out of range: %v", err)
	}
	req.Action = "nope"
	if _, _, err := BlueprintAction(fsys, repo, repo.Blueprints["app-servers"], req); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown action: %v", err)
	}
	req = BlueprintActionRequest{Repository: "infra-live", Instance: web42, Action: "resize", Inputs: map[string]any{"size": "small"}, User: user}
	if _, _, err := BlueprintAction(fsys, repo, repo.Blueprints["linux-vm"], req); !errors.Is(err, ErrNoChange) {
		t.Errorf("a resize to the same size: %v", err)
	}

	// A resource the instance does not have, or that is gone, is the resource's error.
	gone := *shop
	gone.Resources = map[string]string{}
	req = BlueprintActionRequest{Repository: "infra-live", Instance: &gone, Action: "scale-web", Inputs: map[string]any{"cpu": 4}, User: user}
	if _, _, err := BlueprintAction(fsys, repo, repo.Blueprints["app-servers"], req); !errors.As(err, &re) || re.ID != "web" {
		t.Errorf("a resource the instance does not have: %v", err)
	}
	delete(fsys, "envs/dev/vms/shop-web01.yaml")
	req.Instance = shop
	if _, _, err := BlueprintAction(fsys, repo, repo.Blueprints["app-servers"], req); !errors.As(err, &re) || !errors.Is(err, ErrNotFound) {
		t.Errorf("a resource deleted by hand: %v", err)
	}
}
