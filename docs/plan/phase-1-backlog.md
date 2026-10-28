# Phase 1: how autonomous sessions work

Status: Active · 2026-10-28

The tasks for [Phase 1](roadmap.md#phase-1-repository-connections-and-pr-delivery-6-weeks) are
GitHub issues in milestone [M1 Pilot](https://github.com/damhau/quai/milestones) labelled
`phase:1` and `type:task`, titled `P1-NN: …`. Their order is their issue number. Each names the
epic it belongs to (#30–#40). GitHub is the only task list: descriptions and done criteria live
in the issues. Read [`CLAUDE.md`](../../CLAUDE.md) first; an iteration works as in
[Phase 0](phase-0-backlog.md#one-iteration), with `phase:1` instead of `phase:0`:

```bash
gh issue list --repo damhau/quai --label phase:1 --label type:task --state open \
  --json number,title,labels --jq 'sort_by(.number) | .[0]'
```

Commit as `P1-NN: <summary> (closes #<issue>)`; a task may take a few commits, the last one closes
it.

## Test infrastructure

Real instances, no mocks ([testing](../engineering/testing.md)), and everything a Go test needs
runs inside the agents' sandbox:

| What | How |
| --- | --- |
| PostgreSQL | Embedded, as in Phase 0 (`internal/pgtest`) |
| Gitea | Its binary, pinned and checksum-verified, downloaded once into the user cache and started by the tests (`internal/giteatest`, P1-05). No Docker inside the sandbox |
| Kubernetes API | The `kube-apiserver` and `etcd` binaries (envtest), downloaded once and started by the tests (P1-02) |
| Kubernetes cluster | kind, for the Helm chart (P1-20). `kind`, `kubectl` and `helm` against a cluster run outside the sandbox, like `docker` |
| Azure DevOps | The owner's throwaway organization: `https://dev.azure.com/dh-consulting`, project `quai-sandbox` |

Azure DevOps tests are live and skipped unless `QUAI_TEST_AZURE_DEVOPS_PAT_FILE` names the file
holding the PAT (locally `~/.config/quai/azure-devops-pat`) and `QUAI_TEST_AZURE_DEVOPS_URL` the
project (`https://dev.azure.com/dh-consulting/quai-sandbox`). Rules:

- The token is only ever read from that file: never printed, logged, passed on a command line or
  committed. Test failures must not echo request headers.
- Tests create what they need (repositories, branches, policies, service hooks) with a `quai-test-`
  prefix and delete it when they finish, failed or not. They delete nothing they did not create.
- Azure DevOps cannot reach a Quai running on a laptop: live tests use polling, and webhook
  handling is tested with payloads recorded from the sandbox and replayed to the handler.
- In CI, the live tests run nightly once the owner adds the `AZURE_DEVOPS_PAT` repository secret;
  until then they run locally and pull requests rely on Gitea.

Network hosts to declare per command: `dev.azure.com`, `*.visualstudio.com`,
`vssps.dev.azure.com` (Azure DevOps), `dl.gitea.com` (Gitea), and the hosts of the envtest
binaries.

## Azure DevOps Server and Services

The pilot runs Azure DevOps **Server** 2022, which is what the Phase 1 exit names; no Server
instance is available yet. The connector is one package for both editions, written against REST
API 7.0, which Services also serves: it is built and tested on the Services sandbox, with a PAT
(the Server authentication) and Server's URL forms unit-tested. Running the suite on a real Server
is P1-22, a STOP task waiting for an instance. Services uses the PAT as well; Entra
authentication moved to GA (#97).

## STOP tasks

- **P1-22** needs something only the owner can provide (named in the issue). If the issue
  has no comment from the owner saying it is in place, comment what is missing and **stop**. When
  it is there, implement the task and close it like any other.
- **P1-23** (Phase 1 review): write the report the issue describes, commit with `refs`, comment that
  it is ready, and **stop**. Only the owner closes it.

The rules of [when to stop and ask](phase-0-backlog.md#when-to-stop-and-ask) apply unchanged.
