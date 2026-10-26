# ADR 0005: Minimal-diff editing of human-maintained files

Status: Accepted · 2026-10-26

## Context

Quai edits files people also maintain. Parsing and re-serializing a file
reformats it (comment alignment, quoting, flow collections), so every pull
request would show unrelated changes and reviewers would stop reading diffs.

## Decision

Quai never re-serializes a file it did not create. Editors parse with
positions, compute the semantic change, and splice replacement text into the
original bytes. Every write is verified: it parses, decodes to exactly the
intended content, and leaves every byte outside the resource's region
unchanged. When a safe edit is impossible (YAML anchors, non-literal HCL
values), Quai refuses with a reason. Details: [file editing](../design/03-file-editing.md).

## Consequences

- Diffs show only what changed, including in shared files.
- The editors are the riskiest code in Quai and get the heaviest testing: corpus, golden files, fuzzing, round-trip checks on every customer repository.
- Some unusual files are not editable by Quai; they are reported, not rewritten.

## Alternatives considered

- **Round-trip libraries only (re-serialize with comment preservation).** Rejected as the sole mechanism: even the best ones normalize spacing; acceptable only for files Quai created.
- **Text patches computed by diff tools.** Rejected: not semantic, fragile against concurrent edits.
