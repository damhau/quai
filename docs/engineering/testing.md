# Testing strategy

Status: Draft · 2026-10-26

| Level | Scope | Tools | Runs |
| --- | --- | --- | --- |
| Engine unit | Expressions, schemas, locators, generators, editors, policy | `go test`, table-driven | Every PR |
| Golden | Generator and editor output, byte for byte | `testdata/golden/`, `-update` flag to regenerate deliberately | Every PR |
| File-editing corpus | Real-world YAML, JSON, HCL files with edits and expected results | `testdata/corpus/` | Every PR |
| Fuzzing | Editors: parse, decode-equals-intent, bytes-outside-region-unchanged, idempotence | `go test -fuzz` | 60 s per target on PRs; 1 h nightly |
| Store | SQL queries and migrations against real PostgreSQL | Embedded PostgreSQL 17 (`internal/pgtest`, `fergusstrange/embedded-postgres`), or the database in `QUAI_TEST_DATABASE_URL` | Every PR |
| Connector conformance | The shared suite in [Git connectors](../design/12-git-connectors.md#conformance-suite): branch, PR, status, enforcement, merge, policies, webhooks, throttling | **Azure DevOps Server (first):** recorded fixtures on every PR, plus a nightly run against a Server 2022 lab VM and, before each pilot release, against a test project on the pilot's Server. **Azure DevOps Services:** nightly run against a sandbox organization. **Gitea:** a real Gitea started by the tests from its binary (`internal/giteatest`) on every PR. GitHub and GitLab added with their connectors. | PR (containers, fixtures), nightly (real platforms) |
| API | Handlers end to end with a real database and real Git repositories | `httptest`, embedded PostgreSQL, local bare repositories, and Gitea (`internal/giteatest`) | Every PR |
| Jobs | Delivery, staleness, merge, run ingestion, retries after crashes | River test helpers, fault injection (kill a worker mid-job) | Every PR |
| Frontend unit | Components, schema form, hooks | Vitest, Testing Library, MSW | Every PR |
| End to end | Catalog → request → PR → approve → merge → run report → status, in a browser | Playwright against a compose stack (Quai, PostgreSQL, Gitea, a fake runner). Today (`e2e/`, run by `docker compose --profile e2e run --rm e2e` on a running stack): the prototype's walkthroughs on the VM-first examples, with direct delivery: a blueprint request approved by two groups, then committed; a VM created and resized through the `vm` type with a minimal diff; ownership; the designer publishing a blueprint that is then requested. Pull requests and the fake runner come with their tasks | Every PR to `main` |
| Accessibility | Main journeys | Playwright + axe | Every PR to `main` |
| Load | 2,000 requests/day profile, 500 users, 500 repositories | k6 | Before each release |
| Security | ZAP baseline, dependency and image scans | ZAP, govulncheck, `npm audit`, Trivy | Dependency and image scans on every PR; ZAP every release |
| Upgrade | Migrate a database from the previous release with real data shapes | Scripted | Every release |
| DR | Restore from backup, re-index, reconcile open PRs | Scripted runbook | Twice a year |

Principles:

- **The engine is tested hardest.** It decides what gets written to customer repositories. Target 90 %+ line coverage in `internal/engine`, plus fuzzing.
- **Every editing bug becomes a corpus case** before it is fixed.
- **No mocks for PostgreSQL or Git.** Use the real thing; mocks for these hide exactly the bugs that matter. Tests start their own PostgreSQL with embedded-postgres: a real server process, listening on TCP only, whose binaries are downloaded once into the user cache directory. It needs no Docker and runs inside the agents' sandbox, where commands have their own network namespace and cannot reach a container's ports. Code that reads Kubernetes gets a real API server the same way (`internal/k8stest`): the `kube-apiserver` and `etcd` of the envtest release, checksum-verified, downloaded once and started with controller-runtime's envtest. Git transport is tested the same way, in process: `internal/gitx` serves its test repositories with `git http-backend` over HTTPS, with a certificate from a test CA and a server that requires the credentials header, through a CONNECT proxy, and over SSH from a Go SSH server. Gitea runs the same way (`internal/giteatest`): the release's binary (the version the compose stack runs), checksum-verified and downloaded once, started on a free local port with SQLite, an admin user and a token, with helpers for throwaway repositories, users, branch protection and webhooks. CI caches these downloads (`~/.cache/quai`). Containers are for what has no lighter form (the compose stack).
- **Prototype tests are ported first.** The TypeScript tests (expressions, policy, rendering, `patchYaml`, the API scenarios including stale approvals) become the first Go tests, so the port starts from proven behaviour.
