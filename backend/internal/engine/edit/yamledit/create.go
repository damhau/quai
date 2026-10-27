package yamledit

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// Create returns a new YAML document for doc, a value in the JSON data model, written as Edit
// writes new content: block style, two-space indentation, list items indented under their key,
// strings quoted only when they must be. Mapping keys listed in keyOrder come first, in that
// order; the others follow in sorted order. The result is verified to decode to doc.
func Create(doc any, keyOrder []string) ([]byte, error) {
	want, err := normalize(doc, path{})
	if err != nil {
		return nil, err
	}
	f, err := parse(nil)
	if err != nil {
		return nil, err
	}
	f.rank = make(map[string]int, len(keyOrder))
	for i, k := range keyOrder {
		if _, ok := f.rank[k]; !ok {
			f.rank[k] = i
		}
	}
	out := []byte(f.valueText(want, -1, false, "\n") + "\n")
	got, err := decode(out)
	if err != nil {
		return nil, fmt.Errorf("yamledit: %s: the new document does not parse: %v", internalError, err)
	}
	if !equal(got, want) {
		return nil, fmt.Errorf("yamledit: %s: the new document does not decode to the intent", internalError)
	}
	return out, nil
}

// Keys returns, for each mapping in the first document of src, its keys in the order they are
// written, by the JSON pointer of the mapping: "" for the root, "/properties" for the mapping
// under the root's properties key, "/disks/0" for the first item of a list. Merge keys and
// aliases are not followed. It lets callers keep the order of a document they decoded.
func Keys(src []byte) (map[string][]string, error) {
	docs, err := parseTree(src)
	if err != nil {
		return nil, fmt.Errorf("the file is not valid YAML: %v", err)
	}
	out := map[string][]string{}
	if len(docs) == 0 || len(docs[0].Content) == 0 {
		return out, nil
	}
	var walk func(n *yaml.Node, ptr string)
	walk = func(n *yaml.Node, ptr string) {
		switch n.Kind {
		case yaml.MappingNode:
			keys := []string{}
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Kind != yaml.ScalarNode || k.Tag == "!!merge" {
					continue
				}
				keys = append(keys, k.Value)
				walk(n.Content[i+1], ptr+"/"+escapePointer(k.Value))
			}
			out[ptr] = keys
		case yaml.SequenceNode:
			for i, item := range n.Content {
				walk(item, fmt.Sprintf("%s/%d", ptr, i))
			}
		}
	}
	walk(docs[0].Content[0], "")
	return out, nil
}

func escapePointer(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '~':
			b = append(b, '~', '0')
		case '/':
			b = append(b, '~', '1')
		default:
			b = append(b, s[i])
		}
	}
	return string(b)
}
