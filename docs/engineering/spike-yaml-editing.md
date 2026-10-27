# Spike: YAML editing in Go

Status: Decided (`yaml/v3`) · 2026-10-27 · Tasks: P0-02 ([#2](https://github.com/damhau/quai/issues/2)),
P0-03 ([#3](https://github.com/damhau/quai/issues/3)), report P0-04
([#4](https://github.com/damhau/quai/issues/4))

The question: can Quai edit human-maintained YAML in Go well enough to build the engine on it,
and with which library? The bar is [file editing](../design/03-file-editing.md): bytes outside
the edited region never change, inside it only what changed changes, the result means exactly
what was intended, and Quai refuses rather than guesses.

**Recommendation: go, on `go.yaml.in/yaml/v3` instead of `goccy/go-yaml`.** The editor meets
the four requirements on every case we have, and everything it cannot do safely is refused with a
reason. The spike was first built on `goccy/go-yaml`, which misreads some valid YAML, including a
form Quai's own blueprints use; the first version of this report therefore recommended keeping
goccy with `yaml/v3` as a second decoder. At the owner's request the editor was then ported to
`yaml/v3` and put through the same tests. It passes all of them, reads correctly everything goccy
got wrong, needs none of goccy's workarounds, and is faster; with it, no second decoder is needed
(see [the comparison](#second-candidate-goyamlinyamlv3)). The owner chose `yaml/v3`; the
decision is recorded in [open questions](../plan/open-questions.md).

The sections after the comparison describe the editor as first built and tested on goccy; except
where the comparison says otherwise, they hold for both.

## Second candidate: `go.yaml.in/yaml/v3`

The port is on the branch `spike/yaml-v3`. Only the layer that reads the syntax tree changed
(`file.go` and the tree-walking parts of `structure.go` and `render.go`); the diff, rendering and
verification, the tests, the corpus, the fuzz target and the sweep are the same. goccy is no
longer a dependency; `yaml/v3` (the YAML organization's maintained fork of `gopkg.in/yaml.v3`)
was already in the module graph through cobra.

How the port works:

- **Positions.** A `yaml/v3` node has a start line and column (counted in runes), no end and no
  byte offset. The end of each value is found by scanning the file from its start, as the goccy
  version already did because goccy's offsets were unreliable; the `:` after a key and the `-`
  before an item, which `yaml/v3` does not report, are found the same way. Every position is
  checked against the file's text before it is used: the key's or value's words must be there, a
  quoted value is parsed again on its own and compared, the items of a block list share the
  column of their `-`.
- **Values.** `yaml/v3`'s decoder turns dates into `time.Time` and a mapping with a non-string key
  (`200:`, common in OpenAPI files) into a map with `interface{}` keys; neither fits the JSON data
  model. The editor converts the tree itself: scalars are still resolved by `yaml/v3`, dates keep
  their text, keys are their text, merge keys merge as `yaml/v3` merges them, duplicate keys are
  refused, and alias expansion is bounded. Dates and keys come out as goccy's decoder produced
  them; values differ only where goccy departs from the spec (the cases below) and for integers
  beyond 64 bits, which become floats instead of strings.
- **Dropped workarounds.** No LF copy for CRLF files, no search for tokens shifted by trailing
  whitespace, no byte offsets to distrust.
- **New refusals.** Files with line breaks other than LF and CRLF (a CR alone, NEL, LS, PS),
  which `yaml/v3` counts as lines; an empty value holding only a non-specific tag (`key: !`),
  which `yaml/v3` leaves out of the tree.

Results, same machine, same inputs:

| | `goccy/go-yaml` v1.19.2 | `go.yaml.in/yaml/v3` v3.0.5 |
| --- | --- | --- |
| Unit and corpus tests | All pass | All pass. The three tests that pinned goccy bugs now check the correct result |
| Corpus, 21 cases compared byte for byte | All equal | All equal, expectations unchanged |
| Statement coverage | 91.8 % | 91.2 % |
| Fuzzing, 10 minutes | 57.1 M executions (16 workers), no failures | 84.1 M executions (32 workers), no failures; 14.0 M more on the final code, none |
| Replay of the 4,660 inputs the fuzzer kept (both runs) | 3,370 edits, 75.7 % succeed, 4.0 % stopped by verification | 2,851 edits, 79.3 % succeed, 2.2 % stopped by verification |
| Sweep without the Go toolchain's trees | 797 files, 791 editable; 97,681 edits, 97,073 succeed (99.38 %) | 797 files, 796 editable; 98,993 edits, **98,414 succeed (99.42 %)** |
| Sweep refusals caused by the library | 29 (positions 21, verification 7, tree and decoder 1) | **None**; all 579 refusals are policy (merge keys, anchors, aliases, comments in the way) |
| Sweep, all files | 867 files, 861 editable; 97,094 of 113,383 edits (85.6 %) | 867 files, 866 editable; 98,435 of 114,745 edits (85.8 %) |
| Speed | 133 µs per corpus edit; sweep 596 s | 85 µs per corpus edit; sweep 164 s |
| Memory per corpus run (21 edits) | 3.0 MB in 60,000 allocations | 1.6 MB in 21,000 allocations |

`yaml/v3` rejects more of the fuzzer's generated inputs as invalid YAML (control characters, for
example), so its replay has fewer edits to try. The two sweeps ran at the same time. The one file
both refuse whole has several documents. The sweep's one "tree and decoder disagree" refusal under goccy was an editor gap
shared by both versions (setting a value in a file that holds only comments); it is fixed on the
branch.

What goccy got wrong, read by `yaml/v3`:

| goccy finding (details below) | `yaml/v3` |
| --- | --- |
| `{ approvers: [group:admins] }` read as a mapping | Read as the string `"group:admins"`. Adding or removing an approver in the blueprints' form touches only that approver (unit tests); the sweep no longer fails on the example blueprints. |
| Lines miscounted after comments in CRLF files | Correct; the editor works on the original bytes. |
| A double-quoted string folded over CRLF lines decodes as `"one\ntwo"` | `"one two"`, as the spec says; the edit succeeds. |
| Column shifted by trailing whitespace | Correct. |
| A multi-line plain scalar starting on the next line placed at its last line | Correct; the sweep has no position refusals. |
| `{}` followed by a comment line does not parse | Parses; the 2 removals that failed in the sweep succeed. |
| Comments before `---` counted as a document | One document; 2 files of the sweep become editable. |
| A tab inside a quoted string (`prefix: "<tab>// "`) does not parse | Parses; 3 files of the sweep become editable. |
| `1e3` decodes as a string | The float 1000 (YAML 1.2). `0755` is still the integer 493 (YAML 1.1); the quoting rules cover both. |

What `yaml/v3` does that the editor must handle, besides the missing ends and indicators above:

- A lone `!` on an empty value disappears from the tree; such values are refused before editing.
- It is stricter where the spec is strict: control characters such as `\v` make a file invalid,
  and implicit keys are limited to 1,024 characters, so adding a longer key fails verification
  (seen only in fuzzing).
- It attaches comments to nodes, not always to the right one. The editor does not use that: it
  finds comments in the file's text, as it did with goccy.

Fuzzing also found editor edge cases shared by both versions, all stopped by verification:
replacing a document whose value does not start its line (`--- 0`), trailing spaces after a value
that becomes multi-line, and literal blocks with lines that start with a tab. They are not fixed
in the spike.

The owner chose `yaml/v3` on 2026-10-27 and `main` moved to the branch; the documents that named
goccy ([file editing](../design/03-file-editing.md), [backend](backend.md),
[resource types](../design/02-resource-types-and-generators.md),
[migration](migration-from-prototype.md), [ADR 0006](../adr/0006-go-backend-react-frontend.md))
were updated with it.

## What was built

`backend/internal/engine/edit/yamledit`, about 1,850 lines with comments (2,150 in the `yaml/v3`
port), one entry point:

```go
func Edit(src []byte, want any) ([]byte, error)
```

`want` is the intended document in the JSON data model. Edit parses `src` with comments and
positions, computes the difference between the decoded file and `want`, turns each difference
into a text edit on the original bytes, applies the edits, and **verifies the result before
returning it**: it parses as one document, decodes to exactly `want`, and every byte outside the
edited spans is identical. Any failure is a `*RefusalError` with a path and a reason; nothing
half-done is ever returned.

## What works

| Change | Result |
| --- | --- |
| Scalar changed | Only the scalar's bytes; trailing comments and their alignment stay (`cpu: 8          # bumped in INC-4411`). |
| Scalar in a flow collection | Replaced inside the brackets (`network: { vlan: 130, ip: 10.0.13.21 }`). |
| Quoting | The original's quote style is kept. Strings that YAML 1.2 *or* 1.1 parsers would read as another type are quoted: `"4711"`, `"yes"`, `"y"`, `"on"`, `"1e3"`, `"0755"`, `"12:30"`, dates. IP addresses and versions stay plain. New quoting follows the file's majority style. |
| Multi-line strings | Literal blocks indented like their siblings (`|` or `|-`); double-quoted in flow collections or when a literal cannot represent the value; existing block scalars replaced in place. |
| Empty values (`key:`) | Filled after the `:`; a comment after it stays. |
| Key added | After the mapping's last entry, at its siblings' indentation; nested values go into the nearest existing mapping; several new keys in sorted order. |
| Key removed | Its lines, its line comment and the comment lines directly above it (not above the first entry, where comments describe the collection or the file). |
| List item appended, inserted, removed | In the siblings' style (`- ` offset, indentation, indentless lists); items are paired so a changed item is edited in place. |
| Flow collections | Entries added and removed inside the brackets with their separators. |
| Type changes | The node is replaced; the new subtree uses the file's indentation, sequence style and `- ` offset; a line comment stays on the key's line. |
| Whole documents | Empty and comment-only files get content after the comments; root type changes replace the body. |
| Line endings, whitespace, characters | CRLF and LF per line (including CRLF files with comments), trailing spaces and tabs after values, multi-byte characters before values on the same line, files without a final line break. |

## What is refused

Each with a `RefusalError` naming the path and the reason:

- Files: a byte-order mark, invalid UTF-8, invalid YAML (including duplicate keys), more than one
  document, values outside the JSON data model (`.inf`, `.nan`).
- Anchors and aliases: editing, removing or replacing an anchored node; editing through an alias;
  any change in a mapping with a merge key (`<<`).
- Explicit tags on a changed value (`!!str 12`, custom tags).
- Block scalars with an indentation indicator (`|2`) or a comment after the header.
- Removals in a flow collection that would take a comment with them; a comment between a key and
  a value that is replaced.
- Elements that share a line with other content (`- - a`), except the first key of a sequence
  item (`- name: x`), which is handled.
- Files whose syntax tree contradicts the decoder or the bytes: a value or a number of entries the
  decoder does not see, positions out of order, a token whose text is not where the tree puts it.
- Any edit whose result fails verification (see below for how often that happens).

## Tests and coverage

| Suite | Content |
| --- | --- |
| Prototype parity | The TypeScript `patchYaml` tests (web01 resize, db01 flow edit, db01 structural edit, no-op), ported. |
| Table tests | 89 scalar cases, 90 structural cases, 23 refusal cases, 6 invalid-intent cases, verification and helper tests. |
| Corpus | 21 cases under `backend/testdata/corpus/yaml/` built from the prototype's `examples/gitops-repo/envs/**` files: input, edit, and a **hand-written** expected output compared byte for byte. |
| Fuzzing | `FuzzYAMLEdit`: random documents, one random change each (new scalar of any type, removal, addition, insertion, type change). Invariants: the result parses, decodes to the intent, and the same edit is a no-op; the bytes-outside check runs inside Edit. |
| Sweep | `TestSweep` (opt-in, `QUAI_YAML_SWEEP=<dirs>`): every kind of edit on every node of real-world files. |

Statement coverage of the package: **91.8 %** (target 90 %). The uncovered statements are
defensive branches for syntax trees whose positions do not match the file.

## Fuzzing results

- Final run, 10 minutes on 16 workers: **57.1 million executions, no failures**, on the code as
  committed. An earlier 10-minute run on the P0-03 code (28.2 million executions) was clean too.
- Two later runs each found **a panic**, both on malformed input where goccy's tree disagrees
  with the file: a list whose tree has fewer items than the decoder sees (`- &0\n#\n-`), and a
  value the tree places before its own `-` (`-\t\v-`). Fixed by checking, before any structural
  edit, that the tree holds exactly the decoded keys or items and that element positions are in
  order. Both inputs are kept under `testdata/fuzz/` and run with every `go test`. As defence in
  depth, `Edit` now turns any panic into a refusal ("internal error") so a malformed customer file
  cannot crash a worker; the fuzz target and the sweep treat such a refusal as a failure, so these
  bugs stay visible.
- Replaying the 3,572 inputs the fuzzer kept gives 2,582 edits (the rest are not valid documents
  to start from): **2,008 succeed (78 %)**. Most refusals are about the generated file itself:
  invalid UTF-8, block scalars at the document root, tags, anchors, or positions the syntax tree
  gets wrong.
- **106 edits (4.1 %) were stopped by verification**: computed, applied, then rejected because
  the result did not parse or did not decode to the intent. Every one inspected was a
  pathological input (form feeds and tabs inside block scalars, `<<` used as a plain key, a
  block scalar whose last line is only spaces, a root-level flow collection followed by a
  comment). This is the safety net doing its job, and it is why verification on every write is
  not optional.
- Fuzzing also found a real bug in realistic input: a value followed by a line comment that
  became a block scalar pushed the comment into the block's content (refused by verification, so
  never written). Fixed: the comment stays on the key's line. It is now a corpus case.
- Throughput occasionally drops to zero for several seconds: some inputs are slow to parse.
  Not investigated further; see the conditions below.

## Real-world sweep

`TestSweep` ran every kind of edit on every node (new scalar of the same type, removal, added
key, appended item, mapping in place of a scalar; at most 400 edits per file) of the YAML files
available on the development machine: the public dependencies in the Go module cache (CI
workflows, linter, release and documentation configs) and this repository's
own YAML (examples, deploy, workflows). Test fixtures (`testdata/`, `test/`) and duplicates were
left out. There are no Kubernetes manifests or Helm values among them.

| Set | Files | Editable | Edits | Succeeded |
| --- | --- | --- | --- | --- |
| All | 867 | 861 | 113,383 | 97,094 (85.6 %) |
| Without the Go toolchain's source trees | 797 | 791 | 97,681 | **97,073 (99.4 %)** |

The six files refused whole have several documents (3) or are not valid YAML (3). The gap
between the two rows is the 70 YAML files of the Go toolchain's source trees in the module cache,
mostly code-generator inputs for SIMD instructions that put a custom tag on nearly every value:
15,298 of the 16,289 refusals in the first row are "explicit tag". Tagged values are refused by
design; resource files do not use them.

Refusals without those files, by reason:

| Reason | Edits | Kind |
| --- | --- | --- |
| Merge key (`<<`) in the mapping | 256 | Policy |
| Anchored value edited or removed | 136 | Policy |
| Comment between an entry and its value (`key: # note` followed by a block) | 76 | Policy |
| Edit through an alias | 59 | Policy |
| Comment after a block scalar header | 52 | Policy |
| Positions the syntax tree gets wrong (2 files: a multi-line plain scalar starting on the next line) | 21 | Library |
| Verification failed: 5 in the example blueprints (goccy's nested-flow decoding bug), 2 removing the last key of a file that ends with comments (`{}` plus comments does not parse) | 7 | Library |
| Tree and decoder disagree | 1 | Library |

Every successful edit also passed the sweep's independent checks: the result decodes to the
intent, the same edit leaves it alone, and a one-line scalar edit changes at most one line. The
sweep found three library bugs (CRLF positions after comments, trailing whitespace, the
nested-flow decoding); the first two are fixed in the editor, the third is the reason for the
condition above. Before those fixes, 133 edits failed on positions instead of 21.

## Did Quai touch any corpus file beyond its region?

**No.** All 21 corpus outputs equal their hand-written expectations byte for byte, and every
edit (corpus, tests, fuzzing, sweep) passed the bytes-outside-the-edited-spans check before it
was returned. For one-line scalar edits in the sweep, an independent line comparison confirms
that at most one line changed. Regions narrower than a file (`entry` and `field` locators) arrive
with P0-09; the same check then runs against the region's byte range.

## What `goccy/go-yaml` gets wrong, and how the editor copes

| Finding | Handling |
| --- | --- |
| **Decoding bug:** in a flow list nested in a flow mapping, `{ approvers: [group:admins] }`, goccy reads `group:admins` as the mapping `{group: admins}`. The YAML spec, `yaml.v3` and the prototype's TypeScript parser read the string `"group:admins"`; at the top level (`a: [group:admins]`) goccy does too. All three example blueprints use this form. | Editing fails safe but not cleanly: an edit to such a list succeeds and quotes the *unchanged* items (`["group:platform-admins", "group:dba"]`), because goccy believes they were mappings. Anything that reads values with goccy alone (definitions in P0-08) would get them wrong. **This is the reason for the condition above.** |
| Token byte offsets drift after comments and multi-byte characters. | Offsets are computed from line and column (columns count runes), and the token's text must be at that offset or the edit is refused. Recorded in the design. |
| **In CRLF files, goccy counts an extra line after every comment**, so every position below the first comment is off by one line per comment. Found by the sweep; it made everything below a comment in a Windows-edited file uneditable. | The tree is built from a copy with CRLF turned into LF, all offsets are computed on that copy, and each edit is mapped back to the original's bytes (exact, since only the `\r` of `\r\n` is removed). New lines use the line ending of the line they go into. Unit tests and a CRLF corpus case cover it. |
| In CRLF files, a double-quoted string folded over several lines decodes as `"one\ntwo"`; in LF files as `"one two"`, the spec's reading. | The tree (from the LF copy) and the decoder (on the original) disagree, and the edit is refused. Covered by a refusal test. |
| A plain scalar that starts on the line after its key and continues over several lines gets the position of its last line. | Refused; seen once in the sweep (a `golangci-lint` argument list). |
| The column of a token followed by trailing spaces or tabs is shifted right by the length of that whitespace (`a: 1   ` reports column 7, not 4). Common in hand-maintained files; found by the sweep. | When the text is not at the reported column, it is looked for exactly that far to the left, where the trailing whitespace begins; it must match exactly. Unit tests and a corpus case cover it. |
| Block scalar content positions are wrong (column 0, wrong line). | Block scalar content is found by scanning its lines from the header. |
| `1e3` decodes as a string, `0755` as an octal integer: neither YAML 1.2 nor consistent. | New strings that any YAML version could read as non-strings are quoted; new floats keep a decimal point (`1.0e+21`). |
| A root-level flow collection followed by a comment line (`{}` then `# c`) fails to parse although it is valid YAML. | Such files are refused as invalid. It also means that removing the last key of a file that ends with comment lines is refused, because the result (`{}` plus the comments) does not parse; the sweep hit this twice. |
| `Unmarshal` returns only the first document of a multi-document file. | Multi-document files are refused until `dir` regions address documents explicitly. |
| The parser is not known to be panic-free on arbitrary input. | Parser and decoder calls recover from panics and refuse. No parser panic was seen; the two panics fuzzing found were in the editor (above). |

## Decisions taken during the spike

Recorded in [file editing](../design/03-file-editing.md):

- Documents compare in the JSON data model: `8` and `"8"` differ, `8` and `8.0` are equal.
- Several new keys in one mapping are written in sorted order; schema order for new files stays
  with the generators.
- Which comments a removed entry takes with it (above).
- How lists are paired, and that collections sharing nothing with the intent are replaced whole.

## Conditions and open points

These follow from the decision:

1. **The library.** `yaml/v3` alone, no second decoder; read definitions in P0-08 with the same
   conversion.
2. **Verification on every write stays mandatory**, including for formats added later. It is
   what turns library quirks into refusals instead of bad pull requests.
3. **Bound input size and time.** Each edit re-parses the file, and some inputs parse slowly.
   Quai should cap the size of files it edits (1 MB is far above any resource file seen) and run
   edits under a deadline. A corpus edit takes 85 µs with `yaml/v3` (133 µs with goccy).
4. **Corpus from the pilot.** The corpus has four source files and the sweep has no Kubernetes
   manifests or Helm values. Anonymized files from the pilot's repositories (P1) are the real
   test; `TestSweep` can run on a checkout as is.
5. **Not covered yet**, by design of the Phase 0 plan: multi-document files and document
   addressing for `dir` regions (P0-09), the `# managed by quai` full-rewrite fallback, region
   fingerprints, and check 4 of the design (changed lines are only those of changed paths), which
   for now holds because every edit's span comes from the node it changes.
6. **Upstream.** With `yaml/v3`, the dropped non-specific tag on empty values is worth
   reporting. With goccy, the decoding bugs (nested flow, CRLF quoted strings), the parse bugs
   (root flow collection before a comment, tabs in quoted strings, comments before `---`) and the
   position bugs (offsets, trailing whitespace, CRLF after comments, multi-line plain scalars).

If the owner declines both, the remaining alternative is a Go port of the TypeScript `yaml`
package's approach (a concrete syntax tree that round-trips), which would reuse the tests,
corpus, fuzz target and sweep unchanged.
