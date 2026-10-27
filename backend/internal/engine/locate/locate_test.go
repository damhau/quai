package locate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// Ported from the prototype's "path patterns match, format and refuse traversal".
func TestPattern(t *testing.T) {
	p, err := Compile("envs/{env}/vms/{name}.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if p.Prefix() != "envs/" || p.String() != "envs/{env}/vms/{name}.yaml" || !reflect.DeepEqual(p.Params(), []string{"env", "name"}) {
		t.Errorf("pattern = %q, prefix %q, params %v", p, p.Prefix(), p.Params())
	}
	if got, ok := p.Match("envs/prod/vms/web01.yaml"); !ok || !reflect.DeepEqual(got, map[string]string{"env": "prod", "name": "web01"}) {
		t.Errorf("Match = %v, %v", got, ok)
	}
	for _, path := range []string{"envs/prod/vms/sub/web01.yaml", "envs/prod/vms/.gitkeep", "envs/prod/vms/web01.yml", "envs/../vms/x.yaml", "envs/a..b/vms/x.yaml"} {
		if got, ok := p.Match(path); ok {
			t.Errorf("Match(%q) = %v", path, got)
		}
	}
	params := map[string]string{"env": "prod", "name": "web01"}
	if key := p.Key(params); key != "prod/web01" {
		t.Errorf("Key = %q", key)
	}
	if got, err := p.ParseKey("prod/web01"); err != nil || !reflect.DeepEqual(got, params) {
		t.Errorf("ParseKey = %v, %v", got, err)
	}
	for _, key := range []string{"prod", "prod/web01/x", "prod/.."} {
		if _, err := p.ParseKey(key); err == nil {
			t.Errorf("ParseKey(%q) succeeded", key)
		}
	}
	if got, err := p.Format(map[string]string{"env": "dev", "name": "x1"}); err != nil || got != "envs/dev/vms/x1.yaml" {
		t.Errorf("Format = %q, %v", got, err)
	}
	for _, bad := range []map[string]string{
		{"env": "..", "name": "x"},
		{"env": "dev/../../etc", "name": "x"},
		{"env": "dev", "name": ""},
		{"env": "dev"},
		{"env": "dev", "name": ".hidden"},
		{"env": "dev", "name": strings.Repeat("a", 64)},
	} {
		if got, err := p.Format(bad); err == nil {
			t.Errorf("Format(%v) = %q, want an error", bad, got)
		}
	}
}

func TestCompileRefuses(t *testing.T) {
	for source, want := range map[string]string{
		"":                 "clean relative path",
		"/abs/{a}.yaml":    "clean relative path",
		"a/../{b}.yaml":    "clean relative path",
		"a//{b}.yaml":      "clean relative path",
		`a\{b}.yaml`:       "clean relative path",
		"a/{a}{b}.yaml":    "separated by literal text",
		"a/{A}.yaml":       "not a valid param name",
		"a/{a}/{a}.yaml":   "appears twice",
		"a/{a}.yaml}":      "unbalanced braces",
		"a/{a.yaml":        "unbalanced braces",
		"static/file.yaml": "needs at least one {param}",
	} {
		if _, err := Compile(source); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Compile(%q) error = %v, want %q", source, err, want)
		}
	}
	p, err := Compile("{env}/x-{name}.json")
	if err != nil || p.Prefix() != "" {
		t.Fatalf("Compile = %v, %v", p, err)
	}
	if got, ok := p.Match("dev/x-a.json"); !ok || got["name"] != "a" {
		t.Errorf("Match = %v, %v", got, ok)
	}
}

func TestIDs(t *testing.T) {
	id := ID("infra-live", "vm", "prod/web01")
	if id != "infra-live/vm/prod/web01" {
		t.Errorf("ID = %q", id)
	}
	repo, typ, key, err := ParseID(id)
	if err != nil || repo != "infra-live" || typ != "vm" || key != "prod/web01" {
		t.Errorf("ParseID = %q %q %q %v", repo, typ, key, err)
	}
	for _, bad := range []string{"infra-live/vm", "Infra/vm/a", "infra-live/vm/../x", "infra-live/vm/a//b", "a/b/"} {
		if _, _, _, err := ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) succeeded", bad)
		}
	}
}

func TestScanTheExamples(t *testing.T) {
	l, err := New(File, "envs/{env}/vms/{name}.yaml")
	if err != nil {
		t.Fatal(err)
	}
	regions, err := l.Scan(os.DirFS("../../../../examples/gitops-repo"))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range regions {
		keys = append(keys, r.Key)
		if want, _ := l.Pattern().Format(r.Params); r.Path != want || r.Strategy != File {
			t.Errorf("region %+v", r)
		}
	}
	if want := []string{"dev/build-agent-01", "prod/db01", "prod/legacy-ftp", "prod/web01"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
}

func TestScan(t *testing.T) {
	fsys := fstest.MapFS{
		"envs/dev/vms/a.yaml":         {Data: []byte("a")},
		"envs/dev/vms/b.yml":          {Data: []byte("not matched")},
		"envs/dev/vms/sub/c.yaml":     {Data: []byte("too deep")},
		"envs/prod/vms/b.yaml":        {Data: []byte("b")},
		"envs/prod/vms/dir.yaml/x":    {Data: []byte("a directory named like a file")},
		"envs/prod/vms/link.yaml":     {Data: []byte("/etc/passwd"), Mode: os.ModeSymlink},
		"envs/.git/vms/hidden.yaml":   {Data: []byte("in .git")},
		"apps/web/deploy.yaml":        {Data: []byte("web")},
		"apps/web/nested/config.yaml": {Data: []byte("cfg")},
		"apps/api/deploy.yaml":        {Data: []byte("api")},
		"apps/readme.md":              {Data: []byte("a file, not a resource")},
	}
	files, _ := New(File, "envs/{env}/vms/{name}.yaml")
	regions, err := files.Scan(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(regions); !reflect.DeepEqual(got, []string{"dev/a", "prod/b"}) {
		t.Errorf("file keys = %v", got)
	}
	dirs, _ := New(Dir, "apps/{name}")
	regions, err = dirs.Scan(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(regions); !reflect.DeepEqual(got, []string{"api", "web"}) {
		t.Errorf("dir keys = %v", got)
	}
	none, _ := New(File, "nothing/{name}.yaml")
	if regions, err := none.Scan(fsys); err != nil || len(regions) != 0 {
		t.Errorf("Scan of a missing directory = %v, %v", regions, err)
	}
	if _, err := New("entry", "a/{b}.yaml"); err == nil {
		t.Error("New accepted an unsupported strategy")
	}
	if _, err := New(File, "a.yaml"); err == nil {
		t.Error("New accepted a pattern without params")
	}
}

func keysOf(regions []Region) []string {
	var out []string
	for _, r := range regions {
		out = append(out, r.Key)
	}
	return out
}

func TestRegionAndFingerprint(t *testing.T) {
	fsys := fstest.MapFS{
		"envs/prod/vms/web01.yaml": {Data: []byte("cpu: 4\n")},
		"apps/web/a.yaml":          {Data: []byte("a")},
		"apps/web/sub/b.yaml":      {Data: []byte("b")},
		"apps/link/x":              {Data: []byte("/etc"), Mode: os.ModeSymlink},
		"envs/prod/vms/odd.yaml/x": {Data: []byte("a directory")},
	}
	files, _ := New(File, "envs/{env}/vms/{name}.yaml")
	r, err := files.Region(map[string]string{"env": "prod", "name": "web01", "extra": "ignored"})
	if err != nil || r.Path != "envs/prod/vms/web01.yaml" || r.Key != "prod/web01" || len(r.Params) != 2 {
		t.Fatalf("Region = %+v, %v", r, err)
	}
	fp, err := Fingerprint(fsys, r)
	sum := sha256.Sum256([]byte("cpu: 4\n"))
	if want := "sha256:" + hex.EncodeToString(sum[:]); err != nil || fp != want {
		t.Fatalf("Fingerprint = %q, %v; want %q", fp, err, want)
	}
	fsys["envs/prod/vms/web01.yaml"] = &fstest.MapFile{Data: []byte("cpu: 8\n")}
	if fp2, _ := Fingerprint(fsys, r); fp2 == fp {
		t.Error("the fingerprint did not change with the content")
	}
	missing, _ := files.Region(map[string]string{"env": "dev", "name": "new"})
	if fp, err := Fingerprint(fsys, missing); err != nil || fp != Absent {
		t.Errorf("Fingerprint of a missing file = %q, %v", fp, err)
	}
	if _, err := files.Region(map[string]string{"env": "..", "name": "x"}); err == nil {
		t.Error("Region accepted an unsafe value")
	}
	odd, _ := files.Region(map[string]string{"env": "prod", "name": "odd"})
	if _, err := Fingerprint(fsys, odd); err == nil || !strings.Contains(err.Error(), "is not a regular file") {
		t.Errorf("Fingerprint of a directory as a file: error = %v", err)
	}

	dirs, _ := New(Dir, "apps/{name}")
	web, _ := dirs.Region(map[string]string{"name": "web"})
	fp, err = Fingerprint(fsys, web)
	if err != nil || !strings.HasPrefix(fp, "sha256:") {
		t.Fatalf("dir Fingerprint = %q, %v", fp, err)
	}
	for name, change := range map[string]func(fstest.MapFS){
		"content": func(f fstest.MapFS) { f["apps/web/a.yaml"] = &fstest.MapFile{Data: []byte("A")} },
		"rename":  func(f fstest.MapFS) { f["apps/web/c.yaml"] = f["apps/web/a.yaml"]; delete(f, "apps/web/a.yaml") },
		"add":     func(f fstest.MapFS) { f["apps/web/new.yaml"] = &fstest.MapFile{} },
		"remove":  func(f fstest.MapFS) { delete(f, "apps/web/sub/b.yaml") },
	} {
		f := fstest.MapFS{}
		for k, v := range fsys {
			f[k] = v
		}
		change(f)
		if fp2, err := Fingerprint(f, web); err != nil || fp2 == fp {
			t.Errorf("%s: the directory fingerprint did not change (%v)", name, err)
		}
	}
	link, _ := dirs.Region(map[string]string{"name": "link"})
	if _, err := Fingerprint(fsys, link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("Fingerprint of a directory with a link: error = %v", err)
	}
}

// Links in a real checkout are refused, not followed.
func TestSymlinksOnDisk(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.yaml")
	if err := os.WriteFile(outside, []byte("secret: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vms := filepath.Join(dir, "envs", "prod", "vms")
	if err := os.MkdirAll(vms, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(vms, "evil.yaml")); err != nil {
		t.Skipf("cannot create a symbolic link: %v", err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "envs", "dev")); err != nil {
		t.Fatal(err)
	}
	fsys := os.DirFS(dir)
	l, _ := New(File, "envs/{env}/vms/{name}.yaml")
	regions, err := l.Scan(fsys)
	if err != nil || len(regions) != 0 {
		t.Errorf("Scan = %v, %v; links must be skipped", regions, err)
	}
	r, _ := l.Region(map[string]string{"env": "prod", "name": "evil"})
	if _, err := Fingerprint(fsys, r); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("Fingerprint through a link: error = %v", err)
	}
}
