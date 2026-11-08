# Backend engineering (Go)

Status: Draft · 2026-10-26 · Decision: [ADR 0006](../adr/0006-go-backend-react-frontend.md)

## Repository layout

```
quai/
├── backend/
│   ├── go.mod                          # module github.com/damhau/quai/backend
│   ├── cmd/quai/main.go                # one binary: server | worker | lint | report | login | …
│   ├── internal/
│   │   ├── engine/                     # pure: no DB, no network, no clock
│   │   │   ├── expr/                   # CEL environment, ${{ }} bindings
│   │   │   ├── schema/                 # JSON Schema compile/validate, defaults, form metadata
│   │   │   ├── locate/                 # file, dir, entry, field locators; params; fingerprints
│   │   │   ├── generate/               # identity, mapping, component, plugin (the wazero runtime)
│   │   │   ├── edit/                   # yamledit, jsonedit, hcledit + verification
│   │   │   ├── definitions/            # parse/validate types and blueprints (v1beta1)
│   │   │   ├── plan/                   # values → change set (edits, diff, hash)
│   │   │   ├── policy/                 # approval rule evaluation
│   │   │   └── tfplan/                 # terraform-json → resource changes via address patterns
│   │   ├── api/                        # Huma registration, middleware, problem details
│   │   ├── auth/                       # OIDC, sessions, tokens, step-up
│   │   ├── authz/                      # Authorize(): roles, scopes, entitlements
│   │   ├── repositories/               # connections, health, registration checks
│   │   ├── catalog/                    # definitions cache per (repo, commit), catalog index
│   │   ├── inventory/                  # resource index, incremental re-index
│   │   ├── requests/                   # request lifecycle, change sets
│   │   ├── approvals/                  # reviews, delegation, windows, break-glass
│   │   ├── delivery/                   # branches, PRs, checks, merges, staleness
│   │   ├── runs/                       # run reports, status derivation, drift
│   │   ├── actions/                    # imperative actions and executors
│   │   ├── audit/                      # hash chain, checkpoints, export and verification
│   │   │   └── keeper/                 # the worker's: hourly checkpoints and pruning, daily verification
│   │   ├── notify/                     # email, Teams, Slack, outgoing webhooks
│   │   ├── itsm/                       # ServiceNow, JSM
│   │   ├── gitx/                       # mirror cache, worktrees, git CLI wrapper
│   │   ├── ingest/                     # webhooks and polling: deliveries verified, re-read as jobs
│   │   ├── netx/                       # CA bundle and proxy shared by git and every HTTP client
│   │   ├── connectors/                 # azuredevops (first), gitea, github, gitlab, bitbucket, generic
│   │   │   └── conformance/            # shared suite every connector must pass
│   │   ├── secrets/                    # SecretStore: vault, azure key vault, kubernetes, env
│   │   ├── jobs/                       # River workers and periodic jobs
│   │   ├── store/                      # sqlc queries, generated code, transactions
│   │   └── config/                     # configuration loading and validation
│   ├── migrations/                     # goose SQL migrations
│   └── testdata/                       # corpus for file editing, golden files, fixtures
├── frontend/                           # see frontend.md
├── plugins/                            # generator plugins: sdk, template, ini (see plugins.md); modules of their own
├── deploy/helm/quai/
└── docs/
```

Rules:

- **`internal/engine` imports nothing outside the standard library and its own format libraries.** No database, network, clock, environment or logging. This keeps it deterministic and lets `quai lint` run anywhere. The one use of the clock is the plugin runtime's time limit per call (`generate/plugin`, with `wazero`): it can make a call fail, never change what it returns.
- Domain packages own their handlers, queries and jobs. No layer-wide `services/` or `utils/` packages.
- Packages talk through small interfaces defined by the consumer. The `Connector` interface ([Git connectors](../design/12-git-connectors.md)) is the exception: it is shared by `delivery`, `repositories`, `runs` and webhook ingestion, and lives in `internal/connectors`.

## Toolchain

Go 1.27: `go.mod` declares `go 1.27` and `toolchain go1.27.3`. An older local Go (1.21 or later)
downloads that toolchain automatically through the Go module proxy (`GOTOOLCHAIN=auto`, the
default), but a current local install is preferred.

## Libraries

| Concern | Choice | Why |
| --- | --- | --- |
| HTTP | `net/http` + `danielgtaylor/huma/v2` (chi adapter) | OpenAPI 3.1 and request validation generated from Go structs; RFC 9457 errors |
| Database | PostgreSQL, `jackc/pgx/v5`, `sqlc` | Type-safe SQL written by hand, generated Go; no ORM |
| Migrations | `pressly/goose` | Plain SQL migrations, embedded in the binary |
| Audit log hashes | `gowebpki/jcs` | RFC 8785 canonical JSON, which the audit chain's hashes cover ([open question 18](../plan/open-questions.md#still-open)); `encoding/json` does not sort nested keys or serialize numbers as RFC 8785 does, and the form cannot change once a chain exists. Small, no dependencies |
| Jobs | `riverqueue/river` | Durable jobs in PostgreSQL; enqueue in the same transaction as the state change; retries, uniqueness, periodic jobs |
| Expressions | `google/cel-go` | Typed, sandboxed, cost-limited; the Kubernetes standard ([ADR 0009](../adr/0009-cel-expressions.md)) |
| JSON Schema | `santhosh-tekuri/jsonschema/v6` | Draft 2020-12 and 2019-09, formats, `$ref`; RE2 regexes |
| YAML editing | `go.yaml.in/yaml/v3` | Node tree with positions; spec-conformant decoding ([file editing](../design/03-file-editing.md)) |
| JSON editing | `tidwall/gjson`, `tidwall/sjson` | Path edits that keep formatting |
| HCL | `hashicorp/hcl/v2` (`hclwrite`, `hclsyntax`) | HashiCorp's own parser and format-preserving editor |
| Terraform plans | `hashicorp/terraform-json` | Official types for `terraform show -json` |
| WASM plugins | `tetratelabs/wazero` | Pure Go, no CGO, strict sandbox |
| Policy (optional) | `open-policy-agent/opa` (`rego` package) | Evaluate the repository's own Rego bundle in-process |
| OIDC | `coreos/go-oidc/v3`, `golang.org/x/oauth2` | Standard, certified flows |
| Sessions | `alexedwards/scs` with PostgreSQL store | Server-side sessions, rotation, idle timeout |
| Azure DevOps (first connector) | `microsoft/azure-devops-go-api/azuredevops/v7`, wrapped in Quai's own client with `net/http` for gaps | Official client for Git, Policy, Pipelines and Service Hooks |
| Entra ID tokens | `Azure/azure-sdk-for-go/sdk/azidentity` | Workload identity federation, certificates, managed identity |
| Azure Key Vault | `Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets` | `SecretStore` backend for Azure-centric customers |
| Gitea (test reference) | `code.gitea.io/sdk/gitea` | Official client; runs in a container for the conformance suite |
| GitHub | `google/go-github`, at the major version `ghinstallation` builds on, + `bradleyfalzon/ghinstallation` | GitHub App installation tokens; one copy of go-github in the build |
| GitLab | `gitlab.com/gitlab-org/api/client-go` | Official client |
| Kubernetes | `k8s.io/client-go` (dynamic client) | Executors and observers without pulling every CRD's typed client |
| Git transport | `git` CLI through `os/exec` | Full protocol support: SSH, partial clone, sparse checkout, worktrees, signing |
| Logging | `log/slog` | JSON handler in production, text in development |
| Telemetry | OpenTelemetry Go SDK, OTLP/HTTP exporters | Traces and metrics, to a collector (which can serve Prometheus) |
| CLI | `spf13/cobra` | Sub-commands for every role |
| Config | YAML file + environment, validated at start-up | One typed `Config` struct; nothing else reads the environment |

Every dependency addition needs a line in the pull request: why, and why the
standard library is not enough.

## Conventions

- **Context everywhere.** Every function doing I/O takes `ctx context.Context` first; cancellation and deadlines propagate to `git` subprocesses and provider calls.
- **Errors.** Wrap with `fmt.Errorf("…: %w", err)`. Domain errors are typed (`ErrNotFound`, `ErrStale`, `ValidationError{Fields}`) and mapped to problem details in one place in `internal/api`. Handlers never build HTTP errors from domain logic directly.
- **Transactions.** A state change and the jobs it triggers are written in one transaction (River's `InsertTx`), so a crash can never leave a request "submitted" without its delivery job.
- **Audit events.** A security-relevant change adds its event to its own transaction with `audit.Add(ctx, tx, …)`, in a transaction `store.Tx` opened (any other returns `audit.ErrNotRecording`); `store.Tx` writes the events just before committing, under a transaction-level advisory lock that is the transaction's last lock, so the chain has no gaps and the lock's holder never waits on anything. Nothing reads the audit tables in the transaction that writes them. The actor is the context's (`audit.WithActor`: the authentication middleware sets the person or their token, a run report its repository's runner; Quai itself otherwise); an event can name its own. Personal data goes in as `audit.Personal`, never as a plain string, and what a person wrote (a reason, a comment) is always personal. The application role cannot change or delete events, so a fix is a new event, never an `UPDATE` ([audit log](../design/09-security-and-compliance.md#audit-log)). Pruning is the one deletion: `audit.Prune` calls the owner's `audit_prune()`, which decides by the database's clock and the retention `quai migrate` stored, and records `audit.pruned` in the same transaction; verification (`audit.Verify`, `audit.ReadExport`) is the one check, offline and in the worker.
- **Idempotency.** Every job can run twice: connector calls carry idempotency keys or check current state first (a PR for this request already exists → reuse it).
- **One job per repository at a time.** Jobs that change a repository run under its PostgreSQL advisory lock (`jobs.WithRepositoryLock`); a job that finds it taken is snoozed, not failed, and runs again shortly. Work on one request that must not interleave (publishing its status, merging it, commenting its approvals) runs under a lock of its own (`jobs.WithLock`), whose waiters try again after a pause. Locks are held on a pool of their own (`store.Locks`), not on the one their work and River query: a held lock keeps its connection, and a few held on the work's pool once left none for the work under them, every job waiting until it timed out. A waiter holds no connection, and locks do not nest. A worker that dies mid-job leaves the job running; River rescues it after `RescueStuckJobsAfter` and runs it again, which the idempotency rule makes safe. River's own tables are migrated by `quai migrate` after Quai's.
- **Authorization.** Every handler and job calls `authz.Authorize`; a static check in CI fails the build for handlers that do not.
- **Logging.** One log line per request (middleware) and per job, plus one per domain event and per caught error, at `Info` or above. Never log bodies, tokens, credentials or file contents.
- **No global state** besides the configuration loaded at start-up.
- **Time.** The engine never reads the clock; callers pass `now` where needed, so tests are deterministic.

## Quality gates

Every pull request must pass, from `backend/`:

```bash
go build ./...
go vet ./...
staticcheck ./...
gosec -exclude-generated ./...
govulncheck ./...
sqlc diff                 # the code generated from internal/store/queries is up to date
go test -race -count=1 ./...
```

`make check` runs them all with pinned tool versions; `make generate` regenerates the store code
after a query or migration changes. Generated code is excluded from gosec, whose credential
heuristics flag SQL that mentions tokens.

Store and API tests start their own embedded PostgreSQL ([testing](testing.md)), so
`go test` runs them too. Plus a fuzzing smoke run of the editors
(`go test -fuzz=. -fuzztime=60s` per fuzz target).
