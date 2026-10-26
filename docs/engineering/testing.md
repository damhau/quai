# Testing strategy

Status: Draft · 2026-10-26

| Level | Scope | Tools | Runs |
| --- | --- | --- | --- |
| Engine unit | Expressions, schemas, locators, generators, editors, policy | `go test`, table-driven | Every PR |
| Golden | Generator and editor output, byte for byte | `testdata/golden/`, `-update` flag to regenerate deliberately | Every PR |
| File-editing corpus | Real-world YAML, JSON, HCL files with edits and expected results | `testdata/corpus/` | Every PR |
| Fuzzing | Editors: parse, decode-equals-intent, bytes-outside-region-unchanged, idempotence | `go test -fuzz` | 60 s per target on PRs; 1 h nightly |
| Store | SQL queries and migrations against real PostgreSQL | testcontainers-go | Every PR |
| Connector conformance | The shared suite in [Git connectors](../design/12-git-connectors.md#conformance-suite): branch, PR, status, enforcement, merge, policies, webhooks, throttling | **Azure DevOps:** recorded fixtures on every PR, plus a nightly run against a sandbox Services organization and, if the pilot runs Server, a Server instance on a VM. **Gitea:** testcontainers on every PR. GitHub and GitLab added with their connectors. | PR (containers, fixtures), nightly (real platforms) |
| API | Handlers end to end with a real database and a Gitea repository | `httptest`, testcontainers-go | Every PR |
| Jobs | Delivery, staleness, merge, run ingestion, retries after crashes | River test helpers, fault injection (kill a worker mid-job) | Every PR |
| Frontend unit | Components, schema form, hooks | Vitest, Testing Library, MSW | Every PR |
| End to end | Catalog → request → PR → approve → merge → run report → status, in a browser | Playwright against a compose stack (Quai, PostgreSQL, Gitea, a fake runner) | Every PR to `main` |
| Accessibility | Main journeys | Playwright + axe | Every PR to `main` |
| Load | 2,000 requests/day profile, 500 users, 500 repositories | k6 | Before each release |
| Security | ZAP baseline, dependency and image scans | ZAP, govulncheck, Trivy | Every release |
| Upgrade | Migrate a database from the previous release with real data shapes | Scripted | Every release |
| DR | Restore from backup, re-index, reconcile open PRs | Scripted runbook | Twice a year |

Principles:

- **The engine is tested hardest.** It decides what gets written to customer repositories. Target 90 %+ line coverage in `internal/engine`, plus fuzzing.
- **Every editing bug becomes a corpus case** before it is fixed.
- **No mocks for PostgreSQL or Git.** Use the real thing in containers; mocks for these hide exactly the bugs that matter.
- **Prototype tests are ported first.** The TypeScript tests (expressions, policy, rendering, `patchYaml`, the API scenarios including stale approvals) become the first Go tests, so the port starts from proven behaviour.
