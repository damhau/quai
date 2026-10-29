# Quai

Governed self-service portal in front of infrastructure that customers already manage as code:
every request becomes a reviewed change in their Git repositories. Start with
[`docs/README.md`](docs/README.md); the design documents are the specification.

## Current state

- Go backend in `backend/`, React frontend in `frontend/`
  ([ADR 0006](docs/adr/0006-go-backend-react-frontend.md)), Playwright scenarios in `e2e/`, the demo
  repository in `examples/gitops-repo`. The TypeScript prototype was removed in P0-29; it is in the
  history up to commit `70dfce6`.
- **Tasks are GitHub issues** in [damhau/quai](https://github.com/damhau/quai/issues), grouped in
  milestones M1–M3 and labelled `phase:N`, `type:task` / `type:epic`, `area:*`. GitHub is the only
  task list. Phase 0 tasks were issues #1–#29 (`P0-NN` is `#NN`). Phase 1 tasks are issues
  #77–#99, titled `P1-NN` (`P1-NN` is `#(76+NN)`). Phase 2 tasks are issues #100–#113, titled
  `P2-NN` (`P2-NN` is `#(99+NN)`); how an iteration works, the test infrastructure (Gitea, envtest,
  kind, the Azure DevOps sandbox, recorded Terraform plans) and when to stop are in
  [`docs/plan/phase-2-backlog.md`](docs/plan/phase-2-backlog.md).

## Rules

- The docs are the spec. If an implementation must deviate, update the relevant document in the
  same commit. If a decision is not covered, add it to `docs/plan/open-questions.md` and stop to ask.
- Commit straight to `main`, one task per commit or a few small commits, message
  `P2-NN: summary (closes #<issue>)` (`refs` for STOP tasks the owner closes).
  Never add `Co-Authored-By` or other attribution trailers. Push after each task and check CI.
- If `git config user.name` is empty, stop and ask the owner to configure a Git identity.
- Never commit secrets, never disable TLS verification, never weaken a test to make it pass.
- If an action is blocked by Veto (`[BLOCKED BY POLICY]`, guardrail exception) or by the sandbox,
  stop and ask; do not work around it.
- Do not touch untracked local files at the repository root (`.bashrc`, `.idea`, `.mcp.json`, …)
  or `.claude/` directories.

## Commands

Backend (from `backend/`, Go 1.27):

```bash
make check     # build, vet, staticcheck, gosec, govulncheck, sqlc diff, go test -race
make generate  # regenerate internal/store/db after changing queries or migrations
go test ./internal/engine/...            # fast loop on the engine
go test -fuzz=FuzzYAMLEdit -fuzztime=60s ./internal/engine/edit/yamledit
```

Frontend (from `frontend/`, Node 22.12+):

```bash
npm run typecheck && npm run lint && npm run test && npm run build
npm run api:openapi && npm run api:gen   # after an API change (the client is generated, never edited)
npm run cel:cases                        # after a cel-go upgrade: cel-go's conformance cases for src/lib/cel
```

Local stack and end-to-end tests (from the repository root, Docker):

```bash
docker compose up --build --wait            # Quai on :8080 (QUAI_PORT), Gitea on :3000 (GITEA_PORT)
docker compose --profile e2e run --rm e2e   # the Playwright scenarios in e2e/, against that stack
docker compose down -v
```

## Conventions

- Backend: [docs/engineering/backend.md](docs/engineering/backend.md). `internal/engine` is pure:
  no database, network, clock, environment or logging.
- Frontend: [docs/engineering/frontend.md](docs/engineering/frontend.md). Server state in TanStack
  Query, generated API client, shadcn components edited in place.
- Testing: [docs/engineering/testing.md](docs/engineering/testing.md). No mocks for PostgreSQL or Git.
- File editing must never touch bytes outside a resource's region:
  [docs/design/03-file-editing.md](docs/design/03-file-editing.md).
