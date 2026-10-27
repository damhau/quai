# ADR 0009: CEL as the expression language

Status: Accepted · 2026-10-27

## Context

Definitions need expressions for conditions (`when`, `visibleIf`,
`availableIf`), bindings (`${{ … }}`), read-back and validation. The prototype
has a small custom language. A custom language must be specified, documented,
secured and implemented in every place that evaluates it.

## Decision

Use CEL (Common Expression Language) through `google/cel-go`. It is typed,
side-effect free, not Turing-complete, supports cost limits, and is already
used across the Kubernetes ecosystem (ValidatingAdmissionPolicy, CRD
validation), so platform engineers may already know it.

The frontend evaluates `visibleIf` with a JavaScript CEL implementation for
instant feedback, chosen in Phase 0 by running cel-go's conformance tests
(`@marcbachmann/cel-js`; the results are in
[frontend.md](../engineering/frontend.md#expressions-in-the-browser)); the
server's evaluation is authoritative.

## Consequences

- CEL is stricter than the prototype language: missing keys are errors, so expressions use `has(previous.cpu)`; string concatenation needs `string(x)` for numbers. There is no converter: no `v1alpha1` definitions exist outside the prototype, whose examples are rewritten by hand.
- Expressions are type-checked at definition load against the declared schemas, catching errors before anyone fills a form.
- Evaluation cost is bounded, protecting the server from expensive definitions.

## Alternatives considered

- **Keep the custom language.** Rejected: maintenance and security burden, no ecosystem.
- **JavaScript or Lua sandboxes.** Rejected: Turing-complete, harder to bound and to analyze statically.
- **JSONata / JMESPath.** Good for querying, weaker for typed conditions; less familiar to Kubernetes users.
