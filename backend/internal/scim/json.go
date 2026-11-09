package scim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Bounds of what a request may hold: its body, the keys of an object, a PATCH's operations, the
// members a request adds or removes, a string's length (userName, externalId, displayName), an
// attribute path's.
const (
	maxBody       = 1 << 20
	maxKeys       = 100
	maxOperations = 100
	maxMembers    = 1000
	maxString     = 256
	maxPath       = 512
)

// object is a JSON object with its keys folded to lower case: SCIM's attribute names are
// case-insensitive. Two keys equal but for case are refused, as is a key past maxKeys.
type object map[string]json.RawMessage

// decodeObject decodes raw, which must be one JSON object and nothing after it.
func decodeObject(raw []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	out, err := readObject(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the body must be one JSON object")
	}
	return out, nil
}

// asObject decodes a value that must be a JSON object.
func asObject(raw json.RawMessage) (object, error) {
	return decodeObject(raw)
}

func readObject(dec *json.Decoder) (object, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, errors.New("malformed JSON")
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, errors.New("a JSON object was expected")
	}
	out := object{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, errors.New("malformed JSON")
		}
		key, _ := t.(string)
		folded := strings.ToLower(key)
		if _, dup := out[folded]; dup {
			return nil, fmt.Errorf("the attribute %q is given twice", key)
		}
		if len(out) == maxKeys {
			return nil, fmt.Errorf("an object has more than %d attributes", maxKeys)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, errors.New("malformed JSON")
		}
		out[folded] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, errors.New("malformed JSON")
	}
	return out, nil
}

// null reports whether a value is absent or JSON null.
func null(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// text decodes a string attribute: at most maxString characters, no control character.
func text(name string, raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s must be a string", name)
	}
	if len([]rune(s)) > maxString {
		return "", fmt.Errorf("%s is longer than %d characters", name, maxString)
	}
	if strings.ContainsFunc(s, unicode.IsControl) {
		return "", fmt.Errorf("%s has a control character", name)
	}
	return s, nil
}

// boolean decodes active: true or false, or as Microsoft Entra ID may send it, the strings "True"
// and "False" in any case.
func boolean(name string, raw json.RawMessage) (bool, error) {
	var b bool
	if null(raw) { // which json.Unmarshal takes as false
		return false, fmt.Errorf("%s must be true or false", name)
	}
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(s) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, fmt.Errorf("%s must be true or false", name)
}

// reference decodes a reference to a resource, its id: "id", {"value": "id"}, or a list of one
// such object, as Microsoft Entra ID sends the manager; "" for null, an empty string or an empty
// list, which clear it.
func reference(name string, raw json.RawMessage) (string, error) {
	if null(raw) {
		return "", nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		switch len(list) {
		case 0:
			return "", nil
		case 1:
			raw = list[0]
		default:
			return "", fmt.Errorf("%s has more than one value", name)
		}
	}
	if s, err := text(name, raw); err == nil {
		return s, nil
	}
	o, err := asObject(raw)
	if err != nil {
		return "", fmt.Errorf("%s must be an id or an object with a value", name)
	}
	if null(o["value"]) {
		return "", nil
	}
	return text(name+".value", o["value"])
}

// references decodes a list of references, {"value": "id"} each: the members a request adds or
// removes.
func references(name string, raw json.RawMessage) ([]string, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("%s must be a list", name)
	}
	if len(list) > maxMembers {
		return nil, fmt.Errorf("%s has more than %d values", name, maxMembers)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		o, err := asObject(item)
		if err != nil {
			return nil, fmt.Errorf("%s must be objects with a value", name)
		}
		v, err := text(name+".value", o["value"])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
