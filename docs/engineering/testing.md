# Testing strategy

Status: Draft · 2026-10-26

## Tiers

Tests run in three tiers, so that work does not wait for what a later tier catches.

| Tier | When | Who waits for it | What |
| --- | --- | --- | --- |
| **1. Quick** | Before each commit, locally (`make quick` in `backend/`; `npm run typecheck && npm run lint && npm run test` in `frontend/` when it changed) | The developer or the session | Build and vet everything; staticcheck and the tests, without `-race`, of the changed packages and of those that import them (or whose tests do); `sqlc diff` when queries or migrations changed; `quai lint` on the example repository when it changed |
| **2. Full** | On every push, in CI (`backend.yml`, `frontend.yml`, `packaging.yml`) | Nobody: the next iteration reads the previous push's result before it starts, and a red one is fixed first | `make check` in parallel jobs (static checks, `internal/api`'s tests, the other tests, all with `-race`), the frontend, the chart lint and the image build and smoke test |
| **3. Slow** | Nightly, and on demand before a phase review or a milestone (`gh workflow run`) | The phase review and the milestone: they close with tiers 2 and 3 green on the same commit | The Playwright scenarios (`e2e.yml`), the Helm chart on kind (`kind.yml`), the image's vulnerability scan (Trivy, `packaging.yml`), the live tests on the Azure DevOps sandbox (`live.yml`) |

Tests that need no shared state run in parallel (`t.Parallel()`) where they are slow: each has
its own database (`pgtest`) and its own Gitea repositories. At most four use a package's Gitea at
once (`giteatest`): it runs on SQLite, and its queues fall behind under more. The live tests on
the sandboxes stay sequential: the sandbox has one pipeline agent.

| Level | Scope | Tools | Runs |
| --- | --- | --- | --- |
| Engine unit | Expressions, schemas, locators, generators, editors, policy | `go test`, table-driven | Tiers 1 and 2 |
| Golden | Generator and editor output, byte for byte | `testdata/golden/`, `-update` flag to regenerate deliberately | Tiers 1 and 2 |
| File-editing corpus | Real-world YAML, JSON, HCL files with edits and expected results | `testdata/corpus/` | Tiers 1 and 2 |
| Fuzzing | Editors: parse, decode-equals-intent, bytes-outside-region-unchanged, idempotence | `go test -fuzz` | 60 s per target on PRs; 1 h nightly |
| Store | SQL queries and migrations against real PostgreSQL | Embedded PostgreSQL 17 (`internal/pgtest`, `fergusstrange/embedded-postgres`), or the database in `QUAI_TEST_DATABASE_URL` | Tiers 1 and 2 |
| Connector conformance | The shared suite in [Git connectors](../design/12-git-connectors.md#conformance-suite): branch, PR, status, enforcement, merge, policies, webhooks, throttling | **Azure DevOps Services (first):** nightly run against a sandbox organization. **Azure DevOps Server** ([future improvement](../plan/roadmap.md#future-improvements)): recorded fixtures on every PR, plus a nightly run against a Server 2022 lab VM and, before each release to a Server customer, against a test project on their Server. **Gitea:** a real Gitea started by the tests from its binary (`internal/giteatest`) on every PR. GitHub and GitLab added with their connectors. | PR (containers, fixtures), nightly (real platforms) |
| API | Handlers end to end with a real database and real Git repositories | `httptest`, embedded PostgreSQL, local bare repositories, and Gitea (`internal/giteatest`) | Tiers 1 and 2 |
| Jobs | Delivery, staleness, merge, run ingestion, retries after crashes | River test helpers, fault injection (kill a worker mid-job) | Tiers 1 and 2 |
| Frontend unit | Components, schema form, hooks | Vitest, Testing Library, MSW | Tiers 1 and 2 |
| End to end | Catalog → request → PR → approve → merge → run report → status, in a browser | Playwright against a compose stack (Quai, PostgreSQL, Gitea, a fake runner). Today (`e2e/`, run by `docker compose --profile e2e run --rm e2e` on a running stack): the prototype's walkthroughs on the VM-first examples, through pull requests on Gitea, whose `main` requires `quai/approval` and whose webhook reaches Quai: a blueprint request approved by two groups, its pull request linked from the request page and `quai/approval` pending on Gitea until the last approval merges it; a VM created and resized through the `vm` type with a minimal diff, each merged through its pull request; ownership; the designer publishing a blueprint that is then requested; the registration wizard connecting a repository; runs, through the stack's fake runner (`backend/tools/fakerunner`, the `fake-runner` service), which plans each pull request and applies `main` on Gitea's webhooks with `quai report` and a recorded plan, a request's reason steering it (`[costly]`: a plan costing 900 a month, `[fail-apply]`: a failed apply): the plan on the request page while approvals wait, a seeded rule on its cost bringing FinOps in, the request and its resource applied after the merge, a failed apply failing both. The same scenarios on the Azure DevOps sandbox are live tests, run locally and nightly (`.github/workflows/live.yml`): `TestAzureDevOpsDelivery` (pull request with a pending status, approval, auto-complete) and `TestAzureDevOpsRuns` (a plan pipeline as the pull requests' build validation, apply pipelines succeeding and failing, through the Azure Pipelines adapter) | Nightly (`e2e.yml`), and on demand |
| Accessibility | Main journeys | Playwright + axe | Nightly (`e2e.yml`), and on demand |
| Load | 2,000 requests/day profile, 500 users, 500 repositories | k6 | Before each release |
| Security | ZAP baseline, dependency and image scans | ZAP, govulncheck, `npm audit`, Trivy | govulncheck in tier 2; the image scan (Trivy) in tier 3; ZAP every release |
| Helm chart | Install and upgrade on a cluster, migrations as hooks, smoke test | kind (`kind.yml`) | Tier 3 |
| Upgrade | Migrate a database from the previous release with real data shapes | Scripted | Every release |
| DR | Restore from backup, re-index, reconcile open PRs | Scripted runbook | Twice a year |

Principles:

- **The engine is tested hardest.** It decides what gets written to customer repositories. Target 90 %+ line coverage in `internal/engine`, plus fuzzing.
- **Every editing bug becomes a corpus case** before it is fixed.
- **No mocks for PostgreSQL or Git.** Use the real thing; mocks for these hide exactly the bugs that matter. Tests start their own PostgreSQL with embedded-postgres: a real server process, listening on TCP only, whose binaries are downloaded once into the user cache directory. It needs no Docker and runs inside the agents' sandbox, where commands have their own network namespace and cannot reach a container's ports. Code that reads Kubernetes gets a real API server the same way (`internal/k8stest`): the `kube-apiserver` and `etcd` of the envtest release, checksum-verified, downloaded once and started with controller-runtime's envtest. Git transport is tested the same way, in process: `internal/gitx` serves its test repositories with `git http-backend` over HTTPS, with a certificate from a test CA and a server that requires the credentials header, through a CONNECT proxy, and over SSH from a Go SSH server. Gitea runs the same way (`internal/giteatest`): the release's binary (the version the compose stack runs), checksum-verified and downloaded once, started on a free local port with SQLite, an admin user and a token, with helpers for throwaway repositories, users, branch protection and webhooks. CI caches these downloads (`~/.cache/quai`). Azure DevOps tests run live against the sandbox organization through `internal/adotest`, skipped without its PAT; Azure DevOps cannot reach a test machine, so its webhooks are tested by replaying what it tried to deliver, read back from the service hook subscription's history. Containers are for what has no lighter form (the compose stack).
- **Prototype tests are ported first.** The TypeScript tests (expressions, policy, rendering, `patchYaml`, the API scenarios including stale approvals) become the first Go tests, so the port starts from proven behaviour.
