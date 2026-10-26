# Phase 0 backlog

Status: Active · 2026-10-27

Task-level plan for [Phase 0](roadmap.md#phase-0-foundations-and-port-6-weeks), sized for
autonomous sessions: one task per loop iteration, committed straight to `main`. Read
[`CLAUDE.md`](../../CLAUDE.md) first.

## How a loop iteration works

1. Pull `main`. Take the **first unchecked task** below. If it is a **STOP** task, stop the loop and report to the owner.
2. Read the documents the task links to. They are the specification.
3. Implement it, with tests, in `backend/` or `frontend/`.
4. Run the quality gates ([backend](../engineering/backend.md#quality-gates), [frontend](../engineering/frontend.md#quality-gates)). All must pass.
5. Commit as `P0-NN: <summary>` (no attribution trailers), tick the task here with the commit SHA in the same commit or a follow-up, push to `main`.
6. Check CI for the pushed commit (`gh run list --branch main --limit 1`, then `gh run watch`). If CI is red, fixing it is the next iteration's first job.

Stop the loop and ask the owner when:

- the next task is a **STOP** task;
- a decision is needed that the docs do not settle (then add it to [open questions](open-questions.md) and stop);
- the same gate fails three attempts in a row;
- an action is blocked by policy (Veto) or the sandbox, and there is no legitimate alternative.

If the implementation has to deviate from a design document, update the document in the same
commit and say so in the commit message.

## Tasks

### Engine (no Docker needed)

- [ ] **P0-01: Backend scaffolding and CI.** `backend/` Go module `github.com/damhau/quai/backend` with `go 1.27` and `toolchain go1.27.3`; `cmd/quai` with cobra root and `version`; `Makefile` with `make check` running build, vet, staticcheck, gosec, govulncheck and `go test -race` (tools run with pinned versions through `go run`); GitHub Actions workflow running `make check` on every push to `main`. Done: `make check` green locally and in CI. See [backend](../engineering/backend.md).
- [ ] **P0-02: YAML editing spike, part 1: scalar edits.** `internal/engine/edit/yamledit` with `goccy/go-yaml`: compute the semantic change between current and intended documents, splice scalar replacements by byte offset, keep quote style, verify on every write (parses, decodes to intent, bytes outside the changed spans unchanged). Port the cases from `packages/core/test/resourcetype.test.ts` (`patchYaml`) and add a corpus under `testdata/corpus/yaml/` from `examples/gitops-repo/envs/**`. See [file editing](../design/03-file-editing.md).
- [ ] **P0-03: YAML editing spike, part 2: structural edits.** Add and remove keys, append and remove list items, nested insertion into the nearest mapping, type changes; indentation and sequence-style detection, CRLF, multi-line strings; refusal cases (anchors, aliases, merge keys, BOM); a fuzz target with the invariants from the design. Done: all cases pass; 10 minutes of fuzzing without failures.
- [ ] **P0-04: STOP: spike report and go/no-go.** Write `docs/engineering/spike-yaml-editing.md`: what works, what is refused, coverage, fuzzing results, any file from the corpus Quai would touch beyond its region. Commit, push, **stop the loop**. The owner decides whether Go YAML editing is good enough to build on.
- [ ] **P0-05: CEL expressions.** `internal/engine/expr`: CEL environment with `inputs`, `params`, `doc`, `previous`, `action`, `request`, `user`; `${{ … }}` parsing (whole-value bindings keep their type, embedded bindings interpolate as strings); cost limits; compile-time checking hook for later schema-typed variables. Port the intent of `packages/core/test/core.test.ts` expression tests, adapted to CEL semantics. See [ADR 0009](../adr/0009-cel-expressions.md).
- [ ] **P0-06: v1alpha1 expression converter.** Rewrite prototype expressions to CEL (`has()` for optional fields, `string()` for concatenation of numbers, `in` on strings to `.contains()`); report what cannot be converted. Run it over every definition in `examples/gitops-repo`. Done: all example expressions convert and evaluate to the same results as the prototype on sample inputs.
- [ ] **P0-07: JSON Schema.** `internal/engine/schema` with `santhosh-tekuri/jsonschema/v6`: compile drafts 2020-12, 2019-09 and 7; validate with errors keyed by dotted field path; defaults; local `$ref`; formats; locked-field and visibility helpers. Port the schema tests from the prototype. Done: every example VM file validates; the prototype's error cases produce the same field paths.
- [ ] **P0-08: Definitions.** `internal/engine/definitions`: Go types for `quai.dev/v1beta1` `ResourceType` and `Blueprint` as specified in [resource types](../design/02-resource-types-and-generators.md) and [blueprints](../design/04-blueprints-and-change-sets.md); parsing, validation, and conversion from `v1alpha1` (types and blueprints, using P0-06). Done: everything in `examples/gitops-repo` loads, converts and validates.
- [ ] **P0-09: Locators `file` and `dir`.** `internal/engine/locate`: path patterns, safe param segments, resource keys and IDs (`<repository>/<type>/<key>`), region fingerprints, inventory scan over an `fs.FS`. Port the pattern and traversal tests.
- [ ] **P0-10: Generators `identity` and `component`, and planning.** `internal/engine/generate` and `internal/engine/plan`: identity generator with managed fields and unknown-field preservation; component generator porting `packages/core/src/components.ts` with a generic `crossplane.claim`; plan = values → file edits → diff → change set hash. Golden tests: generate expected outputs from the TypeScript prototype for every example blueprint and type, and require byte equality (or document each intentional difference).
- [ ] **P0-11: Approval policy.** `internal/engine/policy`: rule matching with CEL, thresholds, distinct approvers, no self-approval, denial; floor rules merged with type and blueprint rules. Port the prototype's policy tests.
- [ ] **P0-12: `quai lint`.** CLI command validating definitions and every matching resource file in a repository directory, with the round-trip check (`write(read(file))` is a zero diff). CI runs it on `examples/gitops-repo`. Done: passes on the examples; fails with clear messages on seeded broken copies.

### Server (needs Docker for integration tests)

- [ ] **P0-13: STOP if Docker is unavailable.** Run `docker info`. If Docker is reachable and images can be pulled, tick this task and continue. Otherwise **stop the loop** and ask the owner to start Docker and allow the container registries in `/sandbox`.
- [ ] **P0-14: Server skeleton.** `internal/config` (YAML + environment, validated), `log/slog` setup, Huma on chi with `/healthz`, `/api/v1`, OpenAPI document, RFC 9457 errors, request logging, OpenTelemetry wiring (no exporter by default). `quai server` command.
- [ ] **P0-15: PostgreSQL.** goose migrations embedded in the binary; `sqlc` configuration and generated code; testcontainers-go helper; first schema: requests, change sets, reviews, events, runs, sessions, API tokens. Store tests against a real PostgreSQL.
- [ ] **P0-16: Jobs.** River setup, `quai worker` command, transactional enqueue helper, per-repository advisory lock with snooze; tests including a worker killed mid-job.
- [ ] **P0-17: Authorization skeleton.** `internal/authz.Authorize` with the roles from [identity and access](../design/08-identity-access-tenancy.md), table-driven tests, and a test that fails if a Huma handler under `internal/api` does not call it.
- [ ] **P0-18: Authentication.** OIDC login (authorization code + PKCE), `scs` sessions in PostgreSQL, CSRF, API tokens (hashed, scoped, expiring); dev user switcher only when not in production. Tests against an in-process mock OIDC provider.
- [ ] **P0-19: Git layer with direct delivery.** `internal/gitx`: bare mirror, worktree per job, commit with author and trailers, region-fingerprint stale check, retry on non-fast-forward; parity with `server/src/git.ts`. Tests with local bare repositories (no network).
- [ ] **P0-20: API parity.** Endpoints equivalent to the prototype's: catalog (blueprints and types), plan, requests (create, list, get, review, cancel, retry), inventory, resource detail and actions, deployments, git log and commit, run/sync webhook. Port the scenarios of `server/test/api.test.ts` and `server/test/types.test.ts`. Done: all ported scenarios pass.

### Frontend

- [ ] **P0-21: Frontend scaffold.** `frontend/` with Vite, React 19, TypeScript strict, TanStack Router and Query, Tailwind v4, shadcn/ui (new-york), Zustand, Vitest, MSW; API client generated from the backend OpenAPI document with a CI check that it is up to date; app shell, navigation, login redirect. See [frontend](../engineering/frontend.md).
- [ ] **P0-22: Catalog and requests.** Catalog (blueprints and types), request pages with the schema form and live plan preview, JavaScript CEL library chosen by running cel-go's conformance cases (record the choice and results in `docs/engineering/frontend.md`).
- [ ] **P0-23: Approvals and inventory.** Requests list and detail with timeline and review, approvals inbox, inventory with filters, resource and deployment pages with day-2 action dialogs and diffs.
- [ ] **P0-24: Designer and Git log.** React Flow designer on the new stack, Git log page.

### Packaging and parity

- [ ] **P0-25: Packaging.** Multi-stage Dockerfile (Go binary + built frontend), `docker-compose.yml` with Quai, PostgreSQL and Gitea, Helm chart skeleton under `deploy/helm/quai`.
- [ ] **P0-26: Supply chain in CI.** Image build, SBOM (Syft), Trivy scan, cosign keyless signing on `main`, `npm audit` and `govulncheck` gates.
- [ ] **P0-27: End-to-end parity.** Playwright scenarios covering the prototype's walkthroughs (request, approve, commit, resize with minimal diff, ownership checks, designer) against the compose stack.
- [ ] **P0-28: STOP: parity review.** Write a short parity report in this file under the task (what matches, what differs and why). Commit, push, **stop the loop**. The owner confirms before the prototype is deleted.
- [ ] **P0-29: Remove the prototype.** Delete `packages/`, `server/`, `web/` and the old root `package.json` and lockfile; update `README.md` with the new quick start. Done: CI green, compose stack starts, e2e green.
