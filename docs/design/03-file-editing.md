# File editing

Status: Draft · 2026-10-26

How Quai writes to files that humans also maintain, in Go. This is the riskiest
code in Quai: a careless write reformats a file and makes every pull request
unreviewable, or worse, changes something nobody asked for.

## Requirements

1. **Only the region changes.** Bytes outside the resource's region are identical after the edit: comments, blank lines, key order, quoting, indentation, line endings.
2. **Inside the region, only what changed changes.** Resizing a VM touches the `cpu` line and nothing else, including the comment at the end of that line.
3. **The result means exactly what was intended.** Re-parsing the edited file yields the intended document, verified on every write.
4. **Refuse rather than guess.** When a safe edit is impossible (anchors, non-literal HCL, ambiguous layout), Quai refuses with a clear reason instead of rewriting the file.

The prototype meets 1–3 for YAML in TypeScript, with tests on hand-written
files. The Go engine ports the same approach; a Phase 0 spike validates it on a
corpus of real files before anything else builds on it (see the
[roadmap](../plan/roadmap.md)).

## Approach: edit the text, guided by the syntax tree

Parsing a file and serializing it again always reformats something. So Quai
never writes a whole file it did not create. Instead:

1. Parse the original into a syntax tree that keeps comments and the byte position of every node.
2. Compute the semantic change: the list of paths whose values differ between the current content and the intended content.
3. Turn each change into a **text edit** (a byte range and its replacement) using the positions from the tree.
4. Apply the edits to the original bytes, from the end of the file backwards so earlier positions stay valid.
5. **Verify** (below). If verification fails, the write is refused.

### Kinds of change

| Change | Text edit |
| --- | --- |
| Scalar value changed (`cpu: 4` → `8`) | Replace the scalar's bytes only. Trailing comments and alignment stay. |
| Scalar inside a flow collection (`{ vlan: 130, ip: 10.0.13.20 }`) | Same: replace the scalar's bytes inside the braces. |
| Key added to a block mapping | Insert `key: value` lines after the mapping's last entry, with the indentation of its siblings. |
| Key removed | Remove the entry's lines, including its own comments, and nothing else. |
| List item added | Insert after the last item, matching the sibling items' style (`- ` offset, indentation, and the flow style of the values the sibling writes in flow: `quota: { cpu: "4" }`). |
| List item removed | Remove that item's lines. |
| Nested value added (`network.ip` where `network` has no `ip`) | Insert into the nearest existing mapping. |
| Value changes type (scalar to mapping) | Replace the whole node; render the new subtree in block style with the detected indentation. |

New content is rendered with the file's own conventions, detected from its
siblings: indentation width, sequence indentation style, quote style for
strings, line ending (LF or CRLF).

Details the table leaves open:

- **Several new keys** in one mapping are written in sorted order; the document model has no key order. New files list their keys in the schema's order instead: each key ranks where the schema first declares a property of that name, breadth first from the root and through `$ref`s (for the VM schema: `owner`, `description`, …, `tags`, then `vlan`, `ip`, then `size_gb`, `tier`), and keys the schema does not declare follow in sorted order. They are written by the same code as edits, in the style above, and verified to decode to the intended document.
- **A removed entry's own comments** are its line comment and the comment lines directly above it at its indentation, with no blank line in between. Above the first entry of a mapping or list such comments usually describe the whole collection (or the file), so they stay.
- **Flow collections** get the same changes inside their brackets: new entries after the last one with `, `, removed ones with their separator. A removal that would take a comment with it is refused.
- **Lists** are compared item by item: equal items at both ends are kept, the items in between are compared pairwise (so a changed item is edited in place), and the surplus is inserted or removed. When no original item survives, the whole list is replaced. Mappings that share no key with the intended value are replaced the same way.
- **Emptied collections** become `{}` or `[]`.
- **Placement options**, for the `entry` locator: a new entry of a mapping can go in key order (before the first remaining entry whose key sorts after it) instead of at the end, and the keys of new mappings can be ranked like a new file's. The three editors take the same options.
- **A line comment after a value** that becomes several lines stays on the first line (`key: # note` followed by the new block), so it never ends up inside a block scalar's content.

### Scalars

- Plain scalars are replaced by plain scalars when that is unambiguous. A string that would be read as another type (`"4711"`, `"yes"`, `"1e3"`, `"null"`) is quoted.
- If the original value was quoted, the replacement keeps the same quote style.
- Multi-line strings are written as block literals (`|`) indented like their siblings.

## Verification on every write

After applying the edits, the engine checks all of these. Any failure refuses the write.

1. The edited file parses without errors or warnings.
2. Decoding it yields exactly the intended document (deep equality in the JSON data model, with type-aware comparison so `8` and `"8"` differ; numbers compare by value, so `8` and `8.0` are equal).
3. Every byte outside the region's span is unchanged. For `entry` and `field` regions this is checked against the original byte ranges of the other items: each other entry keeps its content and its place. In HCL the padding after a key that aligns the `=` signs is the collection's layout, not the entry's: formatting realigns the neighbours of a new or longer key, and their content, without that padding, stays.
4. Within the region, the changed lines are only those that contain changed paths (computed from node positions).
5. For YAML: no anchor or alias is involved in a changed node. Editing a node that other nodes alias would silently change them too.

## YAML

- Library: `go.yaml.in/yaml/v3`, parsed into its node tree. Each node has the line and column (counted in runes) where it starts, its anchor, tag and style; it has no end and no byte offset, and the `:` after a key and the `-` before a list item are not nodes. Quai computes byte offsets from line and column, finds where each value ends and where the indicators are by scanning the file's text, and checks that the node's text is there before editing (a quoted value is parsed again on its own and compared); block scalar content is located by scanning its lines. Comments are found in the file's text, not taken from the tree. The document is converted from the same tree: scalars resolved by `yaml/v3`, dates kept as text and keys as their text (the JSON data model has neither timestamps nor non-string keys), merge keys merged as `yaml/v3` does, duplicate keys refused. Before any edit, the tree must agree with the decoded document (values, entries and items) and its positions must be in order. See the [spike report](../engineering/spike-yaml-editing.md).
- Multi-document files (`---`) are supported for `dir` regions (Kubernetes manifests); each document is addressed by index or by `kind`/`metadata.name`.
- Refused: edits to anchored or aliased nodes; merge keys (`<<:`); custom tags other than the core schema; files with a byte-order mark, line breaks other than LF and CRLF, or mixed indentation inside the region.
- Fallback for files Quai created and that nobody else edits: if a file has a `# managed by quai` header, a full rewrite is allowed. It is off by default.

## JSON and `*.tfvars.json`

- A parser of its own (`internal/engine/edit/jsonedit`) keeps the byte range of every key and value; the standard library decodes each string and number. Changes are text edits on those ranges, as for YAML, so the rest of the document stays byte-identical. (`tidwall/gjson` and `sjson`, the first plan, neither validate a file nor report duplicate keys, and `sjson` writes a new key without the file's indentation.)
- Inserted values follow their container: on a line of their own with the siblings' indentation in a multi-line object or array, with the siblings' spacing in a one-line one. New objects and arrays are written on several lines, indented by the file's unit (two spaces, four, a tab), or on one line in a file written on one line. Line endings follow the file.
- Key order is preserved; new keys are appended to their object, in sorted order. A removed entry takes its separator with it.
- Refused: duplicate keys (which value counts depends on the reader) and a byte-order mark.

## HCL (`*.tfvars`, `locals`, simple module inputs)

- Library: `github.com/hashicorp/hcl/v2` (`internal/engine/edit/hcledit`). `hclsyntax` parses the file and keeps the byte range of every attribute, object key and value; changes are text edits on those ranges, as for YAML and JSON (`hclwrite`'s `SetAttributeValue` rewrites a whole attribute, so it cannot change one value of a nested object). `hclwrite` renders new scalars, quoting and escaping strings (`${` becomes `$${`).
- The document is a body's attributes: a `*.tfvars` file's top-level attributes, or the attributes of a file's one `locals` block.
- **Only literal values are managed:** numbers, quoted strings without interpolation, booleans, null, and objects and tuples of literals. If a value is an expression (`cpu = var.default_cpu`, `name = "${local.prefix}-web"`, a function call) or a heredoc, Quai shows it as "defined in code", keeps it as it is and refuses to write it: overwriting it with a literal would change the meaning of the configuration. A tuple with code in any item is code as a whole, so that no code sits at an index that shifts; an object with code in it is edited key by key around it.
- New entries go on a line of their own after their siblings in a multi-line object, tuple or body, with a blank line before them when the siblings are separated by one, and in the style of the entry they follow (an object written on one line, a tuple of one-line objects). A removed entry takes its lines and its own comment lines, and the blank line above it when it was the last.
- **A formatted file stays formatted.** When `hclwrite.Format` (what `terraform fmt` runs) leaves the original as it is, the result is formatted too, which realigns the equals signs of the touched groups; verification checks that formatting changed only spaces. A file `terraform fmt` would change is left as it is around the edit.
- Verification decodes the edited file with `hclsyntax` and compares literal values; values defined in code must be byte-identical.
- Typical use: an `entry` locator on a map in `terraform.tfvars` (`vms = { web01 = { cpu = 4 } }`) or a `locals` block.

## Concurrency and fingerprints

Each region has a fingerprint: a SHA-256 over its bytes (for `entry`, the
item's bytes; for `field`, the listed fields' bytes; for `dir`, each file's path
relative to the directory and the SHA-256 of its content, in path order, so that
renaming, adding or removing a file counts). A region that does not exist yet,
such as the file of a resource being created, has the fingerprint `absent`.
Symbolic links are never followed: a link where a region should be is refused. A request stores the
fingerprints at plan time. When the worker builds the branch, it recomputes them
on the current base:

- same fingerprints: apply the stored edits as they are; approvals remain valid;
- different fingerprints: the request is stale; approvals are void and the requester re-plans with the new content.

Edits from different requests to different regions of the same file are
independent, because each is re-applied by path on the current base rather than
replayed as a stored patch. A change set names its regions by reference: a file
or directory by its path, an entry by its file, collection and key
(`clusters/prod-1/namespaces.yaml#namespaces[name=orders]`). A change to entries
of a shared file keeps the entry changes that made it (create, update or delete,
with the entry's new value); when the file is no longer what the request was
planned on, delivery makes them again on the file as it is, verified as any edit
is. The change set's hash and its approvals stay: they are bound to the entry's
content, which its fingerprint shows unchanged. An entry's fingerprint leaves out
HCL's alignment padding (see verification), so realigning it is not a change.

## Testing

| Test | Purpose |
| --- | --- |
| Corpus tests | A directory of real-world files (anonymized from pilot repositories, Kubernetes manifests, Helm values, tfvars) with edits and expected outputs. Grows with every bug. |
| Golden tests | Per generator and per format: input values → exact expected bytes. |
| Fuzzing (`go test -fuzz`) | Random documents and random edits. Invariants: the result parses; decodes to the intended value; bytes outside the region are unchanged; applying the same edit twice is a no-op. |
| Round-trip on customer repositories | `quai lint` checks `write(read(file))` is a zero diff for every existing resource before a type is accepted. |
| Prototype parity | The TypeScript `patchYaml` tests are ported as the first Go tests, so the port starts from proven behaviour. |
