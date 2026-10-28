# Quai

A governed self-service portal in front of infrastructure that teams already manage as code:
**every request becomes a reviewed change in their Git repositories**. Quai does not provision
anything and does not own the source of truth. Terraform, Argo CD, Flux or Crossplane do the work;
Git holds the result.

- **Resource types** describe an existing repository's layout and the JSON Schema of its files
  (`.quai/types/`). Every file that matches is in the inventory, including those written before
  Quai.
- **Forms** come from that schema, with live feedback: while you type, Quai shows the exact
  diff it would commit and who will have to approve it.
- **Minimal diffs.** Quai edits YAML in place: comments, alignment and key order survive, and
  reviewers see only the lines that change.
- **Approvals** from rules on the repository, the type and the blueprint, bound to the content
  they approve: a change made in Git meanwhile makes the request stale. Nobody approves their own
  request.
- **Blueprints** compose types into one request, built in a canvas designer, with day-2 actions
  that fan out to their resources.
- **Runs** are reported by the repository's pipelines, per stack.

The design documents are the specification: start with [`docs/`](docs/README.md).

## Try it

Requires Docker. The stack runs Quai (server and worker), PostgreSQL, and a Gitea holding the
example repository [`examples/gitops-repo`](examples/gitops-repo): Terraform VMs, one YAML file
each, with the `vm` type and two blueprints built from it.

```bash
docker compose up --build --wait   # Quai on http://localhost:8080, Gitea on http://localhost:3000
docker compose down -v             # stop and start over
```

`QUAI_PORT` and `GITEA_PORT` choose other ports. Sign in as one of the demo users: **alice**
(platform admin), **bob** and **carol** (developers), **fiona** (FinOps). The account menu signs
you out to switch.

1. As bob, request a **Linux virtual machine** in `prod`, size `large`. The preview shows the file
   it adds and that the platform admins and FinOps must approve.
2. The request opens a pull request on Gitea (`quai/infra-live`, whose `main` requires the
   `quai/approval` status); the request page links it. Approve it from **Approvals** as alice,
   then as fiona: the second approval turns `quai/approval` to success and Quai merges the pull
   request; it shows in the **Git log**. The repository is public on the local Gitea: follow the
   link to see the pull request and its status.
3. In the **Inventory**, open `prod/web01` as bob and **Resize** it. The diff touches only the
   `cpu` line and keeps its `# bumped in INC-4411` comment.
4. As carol, open the same VM: she cannot change it, bob owns it.
5. As alice, open the **Designer**, start a blueprint from the `vm` type, publish it, and request
   it from the catalog.

## Develop

| | |
| --- | --- |
| `backend/` | Go: the engine (definitions, CEL, YAML editing, planning, policy), the API, the worker; one binary, `quai` |
| `frontend/` | React, TypeScript, TanStack Router and Query, Tailwind, shadcn/ui; the API client is generated |
| `e2e/` | Playwright scenarios against the compose stack |
| `examples/gitops-repo` | The demo repository, also the tests' fixture |
| `deploy/` | The compose stack's scripts and the Helm chart |
| `docs/` | Design, decisions (ADRs), engineering, operations and the plan |

```bash
cd backend && make check                                   # build, lint, security checks, tests
cd frontend && npm ci && npm run dev                       # :5173, against quai server on :8080
docker compose --profile e2e run --rm e2e                  # end to end, on a running stack
```

Conventions are in [backend](docs/engineering/backend.md), [frontend](docs/engineering/frontend.md)
and [testing](docs/engineering/testing.md).

## Status

Phase 0 (the foundations, and the port of the TypeScript prototype) is done. Phase 1 delivers
through pull requests whose approval the Git platform enforces, on Azure DevOps and Gitea: the
local stack runs that way. Next are run and plan status from pipelines. The
[roadmap](docs/plan/roadmap.md) has the phases; the work is tracked in
[GitHub issues](https://github.com/damhau/quai/issues), by milestone.
