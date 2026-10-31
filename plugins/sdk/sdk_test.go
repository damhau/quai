package sdk

import (
	"errors"
	"strings"
	"testing"
)

// upper writes its values as KEY=value lines, upper-cased, and refuses an empty value.
type upper struct{}

func (upper) Write(r WriteRequest) (string, error) {
	var b strings.Builder
	if r.Current != nil {
		b.WriteString(*r.Current)
	}
	for _, k := range []string{"a", "b"} {
		if v, ok := r.Values[k]; ok {
			if v == "" {
				return "", errors.New(k + " is empty")
			}
			b.WriteString(strings.ToUpper(k) + "=" + v.(string) + "\n")
		}
	}
	return b.String(), nil
}

func (upper) Read(r ReadRequest) (map[string]any, error) {
	if r.Region == "nil" {
		return nil, nil
	}
	return map[string]any{"region": r.Region, "name": r.Params["name"], "config": r.Config["c"]}, nil
}

// TestHandle: the ABI's JSON in and out of a plugin; its refusals are errors in the output.
func TestHandle(t *testing.T) {
	for _, tt := range []struct{ fn, in, want string }{
		{"write", `{"values": {"a": "x"}}`, `{"region":"A=x\n"}`},
		{"write", `{"values": {"b": "y"}, "current": "# kept\n"}`, `{"region":"# kept\nB=y\n"}`},
		{"write", `{}`, `{"region":""}`},
		{"write", `{"values": {"a": ""}}`, `{"error":"a is empty"}`},
		{"read", `{"region": "R", "params": {"name": "web"}, "config": {"c": 1}}`, `{"values":{"config":1,"name":"web","region":"R"}}`},
		{"read", `{"region": "nil"}`, `{"values":{}}`},
		{"read", `not json`, `{"error":"the request is not valid JSON: invalid character 'o' in literal null (expecting 'u')"}`},
		{"delete", `{}`, `{"error":"unknown call \"delete\""}`},
	} {
		if got := string(Handle(upper{}, tt.fn, []byte(tt.in))); got != tt.want {
			t.Errorf("%s %s = %s, want %s", tt.fn, tt.in, got, tt.want)
		}
	}
	if got := string(Handle(nil, "write", []byte(`{}`))); !strings.Contains(got, "no plugin is registered") {
		t.Errorf("without a plugin = %s", got)
	}
}

func TestRegister(t *testing.T) {
	defer func() { registered = nil }()
	Register(upper{})
	defer func() {
		if recover() == nil {
			t.Error("a second Register did not panic")
		}
	}()
	Register(upper{})
}
