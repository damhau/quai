# Migration from the prototype

Status: Draft · 2026-10-26

The prototype (`packages/core`, `server/`, `web/`, TypeScript) stays runnable
until the Go backend reaches parity, then it is removed. Nothing is migrated
in place.

## What carries over

| Prototype | Becomes | How |
| --- | --- | --- |
| `packages/core/src/expr.ts` (custom expressions) | `internal/engine/expr` (CEL) | Re-implemented on cel-go ([ADR 0009](../adr/0009-cel-expressions.md)); no converter, the example expressions are rewritten by hand |
| `inputs.ts`, `blueprint.ts`, `resourcetype.ts` | `internal/engine/schema`, `definitions`, `locate` | Ported; only `v1beta1` is read, `v1alpha1` is refused |
| `yamlpatch.ts` (minimal-diff YAML) | `internal/engine/edit/yamledit` | Ported to `go.yaml.in/yaml/v3`; TS tests ported first |
| `components.ts` (K8s renderers) | `internal/engine/generate/component`, later | Not ported in Phase 0: the examples are VM-first and the prototype's Kubernetes blueprints are not carried over. The `component` generator stays in the design for when a customer needs it |
| `render.ts`, `policy.ts` | `internal/engine/plan`, `policy` | Ported |
| `server/src/git.ts` | `internal/gitx` + `internal/delivery` | Re-designed for mirrors, worktrees and PRs |
| `server/src/service.ts`, `resources.ts` | `requests`, `approvals`, `inventory`, `catalog` | Re-designed for PostgreSQL, jobs and providers |
| `server/src/store.ts` (SQLite) | `store` + `migrations` (PostgreSQL) | New schema; no data migration (prototype data is demo data) |
| `web/` | `frontend/` | Rebuilt on TanStack Router/Query, Tailwind and shadcn, page by page, reusing component logic (schema form, diff folding, designer) |
| `examples/gitops-repo` | `examples/gitops-repo` | Kept as the demo and end-to-end fixture, VM-first: the Terraform VM files, the `vm` type and two blueprints built from it, written by hand in `v1beta1` (P0-08). The four Kubernetes blueprints are not carried over. Until the prototype is removed, it reads a frozen `v1alpha1` copy in `examples/prototype-gitops-repo` |
| `docker-compose.yml` | `docker-compose.yml` | Quai (Go), PostgreSQL, Gitea, a fake runner |

## Steps

1. **Spike (week 1).** Port `yamlpatch` to Go against the prototype's tests plus a corpus of real files from the pilot repository. Go/no-go for the YAML approach in Go before anything else depends on it.
2. **Engine (weeks 1–3).** Port the engine packages with their tests. `quai lint` works on `examples/gitops-repo`.
3. **API parity (weeks 3–5).** Huma endpoints equivalent to the prototype's, on PostgreSQL, with OIDC. Direct delivery only.
4. **Frontend (weeks 3–6).** New `frontend/` against the Go API, page by page: catalog, request, inventory, resource, approvals, git log, designer.
5. **Parity check.** The prototype's end-to-end scenarios pass against Go + new frontend. Remove `packages/`, `server/`, `web/`.

Steps 3 and 4 overlap; the generated client lets the frontend start as soon as
the first endpoints exist.
