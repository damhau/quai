# ADR 0004: Structured generators, no text templates; plugins in WASM

Status: Proposed · 2026-10-26

## Context

Generating YAML by text templating (Jinja, Go `text/template`) is common and
fragile: a user value containing a newline or a colon changes the document's
structure, and templates cannot read a file back into a form.

## Decision

Generators build documents as data. Bindings (`${{ … }}`) are CEL expressions
evaluated to typed values; a binding that is the whole value keeps its type.
Documents are then written by format-aware editors. Every generator has a
`read` direction so day-2 forms start from the file as it is in Git.

For needs the built-in generators do not cover, plugins are WebAssembly
modules pinned by digest and run with `wazero`, without file system, network,
clock or randomness, and with memory and CPU limits.

## Consequences

- User input cannot inject structure; there is no template language to escape.
- `mapping` generators need explicit `read` expressions for computed fields; `quai lint` rejects types whose inputs cannot be recovered.
- Plugin authors need a WASM toolchain; a Go SDK and template repository are provided.

## Alternatives considered

- **Jinja or Go templates.** Rejected: injection risk, no read-back, whole-file rewrites.
- **Scripts (shell, Python) as plugins.** Rejected: impossible to sandbox reliably inside Quai's process or worker.
