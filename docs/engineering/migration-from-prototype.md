# Migration from the prototype

Status: Draft · 2026-10-26

The prototype (`packages/core`, `server/`, `web/`, TypeScript) stays runnable
until the Go backend reaches parity, then it is removed. Nothing is migrated
in place.

## What carries over

| Prototype | Becomes | How |
| --- | --- | --- |
| `packages/core/src/expr.ts` (custom expressions) | `internal/engine/expr` (CEL) | Re-implemented on cel-go; a converter rewrites `v1alpha1` expressions (see [ADR 0009](../adr/0009-cel-expressions.md)) |
| `inputs.ts`, `blueprint.ts`, `resourcetype.ts` | `internal/engine/schema`, `definitions`, `locate` | Ported; `v1alpha1` definitions are converted on read |
| `yamlpatch.ts` (minimal-diff YAML) | `internal/engine/edit/yamledit` | Ported to `goccy/go-yaml`; TS tests ported first |
| `components.ts` (K8s renderers) | `internal/engine/generate/component` | Ported; Crossplane renderer made generic |
| `render.ts`, `policy.ts` | `internal/engine/plan`, `policy` | Ported |
| `server/src/git.ts` | `internal/gitx` + `internal/delivery` | Re-designed for mirrors, worktrees and PRs |
| `server/src/service.ts`, `resources.ts` | `requests`, `approvals`, `inventory`, `catalog` | Re-designed for PostgreSQL, jobs and providers |
| `server/src/store.ts` (SQLite) | `store` + `migrations` (PostgreSQL) | New schema; no data migration (prototype data is demo data) |
| `web/` | `frontend/` | Rebuilt on TanStack Router/Query, Tailwind and shadcn, page by page, reusing component logic (schema form, diff folding, designer) |
| `examples/gitops-repo` | `examples/gitops-repo` | Kept as the demo and end-to-end fixture; definitions upgraded to `v1beta1` |
| `docker-compose.yml` | `docker-compose.yml` | Quai (Go), PostgreSQL, Gitea, a fake runner |

## Steps

1. **Spike (week 1).** Port `yamlpatch` to Go against the prototype's tests plus a corpus of real files from the pilot repository. Go/no-go for the YAML approach in Go before anything else depends on it.
2. **Engine (weeks 1–3).** Port the engine packages with their tests. `quai lint` works on `examples/gitops-repo`.
3. **API parity (weeks 3–5).** Huma endpoints equivalent to the prototype's, on PostgreSQL, with OIDC. Direct delivery only.
4. **Frontend (weeks 3–6).** New `frontend/` against the Go API, page by page: catalog, request, inventory, resource, approvals, git log, designer.
5. **Parity check.** The prototype's end-to-end scenarios pass against Go + new frontend. Remove `packages/`, `server/`, `web/`.

Steps 3 and 4 overlap; the generated client lets the frontend start as soon as
the first endpoints exist.
