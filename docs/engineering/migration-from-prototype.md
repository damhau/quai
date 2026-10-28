# Migration from the prototype

Status: Draft · 2026-10-26

The prototype (`packages/core`, `server/`, `web/`, TypeScript) stayed runnable
until the Go backend reached parity, and was removed in P0-29 once the owner
accepted the [parity review](#parity-review-p0-28). Nothing was migrated in
place. It is in the history up to commit `70dfce6`
(`git show 70dfce6:packages/core/src/yamlpatch.ts`, for instance).

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
| `examples/gitops-repo` | `examples/gitops-repo` | Kept as the demo and end-to-end fixture, VM-first: the Terraform VM files, the `vm` type and two blueprints built from it, written by hand in `v1beta1` (P0-08). The four Kubernetes blueprints are not carried over. The prototype read a frozen `v1alpha1` copy, `examples/prototype-gitops-repo`, removed with it |
| `docker-compose.yml` | `docker-compose.yml` | Quai (Go), PostgreSQL, Gitea, a fake runner |

## Steps

1. **Spike (week 1).** Port `yamlpatch` to Go against the prototype's tests plus a corpus of real files from the pilot repository. Go/no-go for the YAML approach in Go before anything else depends on it.
2. **Engine (weeks 1–3).** Port the engine packages with their tests. `quai lint` works on `examples/gitops-repo`.
3. **API parity (weeks 3–5).** Huma endpoints equivalent to the prototype's, on PostgreSQL, with OIDC. Direct delivery only.
4. **Frontend (weeks 3–6).** New `frontend/` against the Go API, page by page: catalog, request, inventory, resource, approvals, git log, designer.
5. **Parity check.** The prototype's end-to-end scenarios pass against Go + new frontend. Remove `packages/`, `server/`, `web/`.

Steps 3 and 4 overlap; the generated client lets the frontend start as soon as
the first endpoints exist.

## Parity review (P0-28)

2026-10-27, at the end of P0-27. The prototype's three walkthroughs (its README) and the
scenarios of its tests (`server/test/api.test.ts`, `types.test.ts`, `packages/core/test`) against
the Go backend and the new frontend, on the VM-first examples.

**Matches.** Covered by the Go tests, the frontend tests and the Playwright scenarios (`e2e/`).

| Prototype | Now |
| --- | --- |
| Walkthrough 1: a large production VM from a blueprint needs the platform admins and FinOps; the second approval commits it; the Git log shows it | `e2e/tests/blueprint-request.spec.ts`; `TestApprovals` |
| Walkthrough 2: resize a VM and see the minimal diff before submitting | `e2e/tests/type-request.spec.ts` (two lines, the comment kept); blueprint actions on a deployment (`TestInstanceActions`) |
| Walkthrough 3: build a blueprint in the designer and commit it | `e2e/tests/designer.spec.ts`, from the `vm` type |
| No self-approval; a denial needs a comment and rejects; cancel and retry | `TestApprovals`, `TestDenialAndCancel`, request page tests |
| A human edit to a file makes a pending approval stale | `TestStale` (ported) |
| Types read from the repository, discovered params, brownfield files in the inventory with their owners, unparseable files listed and never edited | Catalog and inventory API tests, inventory page |
| Create validates params and the document; one file at the type's path, owned by the requester; new files in the schema's key order | API and engine tests |
| Indexed `set` paths, immutable fields, owner protection, no-op changes refused, delete removes the file | `TestDayTwo`, resource page and dialogs |
| Only admins publish blueprints, validated against the repository's types | `publish-blueprint` tests |
| Live preview of the commit and of the approvals while filling a form; `visibleIf` | Request pages, `src/lib/cel` |
| Expressions, templates, input validation, policy, rendering with `when`, unsafe paths, line diff | Engine packages and their tests |

**Differs, on purpose.**

| Prototype | Now | Why |
| --- | --- | --- |
| Its own expression language | CEL: a missing key is an error, `string(x)` to concatenate numbers | ADR 0009 |
| Kubernetes blueprints and the component palette | VM-first examples; the designer's palette lists the repository's types | Owner's decision (P0-08); the `component` generator stays in the design |
| `v1alpha1` definitions | `v1beta1` only, rewritten by hand, no converter | Owner's decision (#6) |
| Quai creates and seeds its own repository | Quai connects to existing repositories; the compose stack seeds Gitea | Design 01 |
| One repository, `/api/git/*` | Several repositories, each with its log, commits and sync | Design 01 |
| Proxy authentication (`X-Forwarded-*`) and a sidebar identity switcher | OIDC, or the development switcher on the login page | Design 08 |
| Operators could change any resource | The owner or their team, with the operator role, or a platform admin | Design 08 |
| Simulated "Synced / Healthy" after each commit, sync webhook per deployment | Run reports per stack (`POST /api/v1/runs`), shown as the last apply | Design 06 |
| SQLite, one process | PostgreSQL, server and worker processes | ADR 0006 |
| Designer: YAML view | Definition as JSON; the file is written in the repository's key order on publish | Fewer dependencies; publish writes YAML |
| `docker compose up` ran the prototype | It runs the Go stack | P0-25 |

**Not there yet.** Prototype features with no equivalent, for the owner to schedule or drop:

1. **Request expiry.** The prototype expired pending requests after `requestTtlHours` and showed
   when. Design 05 has `expiry: 7d`; nothing expires requests yet.
2. **Deleting a deployment.** The prototype deleted a deployment's directory. Deleting a blueprint
   instance (its resources and its record) has no endpoint; each resource can be deleted on its
   own. Design 04 also plans re-creating or detaching a resource deleted by hand, which the
   deployment page only flags as missing.
3. **A demo runner.** Without the prototype's simulated sync, the local stack shows no apply until
   a pipeline reports one; the fake runner of the testing strategy is not built.
4. **Small things:** the designer has no preview of the blueprint's form and plan, and catalog
   cards do not count a type's resources.

None of these blocks the pilot's walkthroughs. Removing the prototype (P0-29) loses nothing else.
