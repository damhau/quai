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
- In CI, the live tests run nightly (`.github/workflows/live.yml`, also on demand) once the owner
  adds the `AZURE_DEVOPS_PAT` repository secret; until then the workflow says so and does
  nothing, the tests run locally, and pull requests rely on Gitea.

Network hosts to declare per command: `dev.azure.com`, `*.visualstudio.com`,
`vssps.dev.azure.com` (Azure DevOps), `dl.gitea.com` (Gitea), and the hosts of the envtest
binaries.

## Azure DevOps Services and Server

The pilot runs Azure DevOps **Server** 2022, but Phase 1 and M1 are accepted on **Services**:
running on a real Server is a [future improvement](roadmap.md#future-improvements) (#98, formerly
P1-22). The connector is one package for both editions, written against REST API 7.0, which
Services also serves: it is built and tested on the Services sandbox, with a PAT (the Server
authentication) and Server's URL forms unit-tested. Services uses the PAT as well; Entra
authentication moved to GA (#97).

## STOP task

- **P1-23** (Phase 1 review): write the report the issue describes, commit with `refs`, comment that
  it is ready, and **stop**. Only the owner closes it.

The rules of [when to stop and ask](phase-0-backlog.md#when-to-stop-and-ask) apply unchanged.

## Phase 1 review (P1-23)

2026-10-29, after P1-20. The [Phase 1 exit](roadmap.md#phase-1-repository-connections-and-pr-delivery-6-weeks)
against the tests that show it: on Gitea, on every push (backend tests and the Playwright
scenarios); on the Azure DevOps Services sandbox, the live tests (`-run 'Live|AzureDevOps'` in
`internal/connectors/azuredevops`, `internal/ingest` and `internal/api`), all passing when run
locally on 2026-10-29.

**The exit, criterion by criterion.**

| Exit criterion | Gitea | Azure DevOps Services |
| --- | --- | --- |
| A repository is connected | `TestRepositoryAccess`, `TestManageRepositories`; e2e `repository-wizard` | `TestAzureDevOpsAccess`: every access check passes, the service hook subscription is installed |
| A request opens a PR | `TestPullRequestDelivery`; e2e `blueprint-request` | `TestAzureDevOpsDelivery` |
| The build validation pipeline plans | — | **Not shown.** Quai reads policy evaluations, build validation among them, and waits for them; no test has a pipeline, and the plan itself comes with Phase 2's run reports |
| Approvals flip `quai/approval` | `TestPullRequestDelivery`; e2e `blueprint-request` reads the status on Gitea | `TestAzureDevOpsDelivery`: pending, required by a status check policy only Quai's identity satisfies, set by the last approval |
| Auto-complete merges once every policy passes | `TestPullRequestDelivery`, `TestAutoComplete`; `TestManualMerge` for manual merge | `TestAzureDevOpsDelivery`, `TestLiveAutoComplete` |
| Removing the policy or its authorized identity is detected within an hour and blocks delivery | `TestEnforcement`, `TestEnforcementChecks` (hourly, and at once on a policy change) | `TestAzureDevOpsEnforcement`: identity removed, then the policy; new PRs refused; restored |
| The conformance suite passes | `TestConformance` (`internal/connectors/gitea`) | `TestLiveConformance` |

**Also delivered.** Content-bound approvals: rebuilds on a moved base, outside pushes, stale
requests, merges that bypass Quai (`internal/api/content_test.go`, `TestAzureDevOpsContent`).
Signed commits with `Approved-by:` trailers (`TestSignedCommits`). Definitions per commit with
trust checks (`TestTrust`, `TestAzureDevOpsTrust`). Webhooks through the server, and Azure DevOps
deliveries replayed from the sandbox (`TestWebhooksThroughTheServer`, `TestAzureDevOpsReplay`).
Repository pages, health and OTLP metrics. The Helm chart installed and upgraded on kind
(`kind.yml`).

**Moved out of Phase 1.**

- **#97**, the Entra service principal for Services: to M3 GA (owner, 2026-10-28). Services uses a
  PAT, as Server does.
- **#98**, Azure DevOps Server 2022: to the [future improvements](roadmap.md#future-improvements)
  (owner, 2026-10-29). With it: version detection and the pinned API 7.0 calls on a real Server,
  Server's URL forms beyond unit tests, an internal CA on a real Server (the CA bundle is tested in
  process with a test CA), and `tf git permission` guidance (the access test names the
  permissions as the web UI does). The pilot runs Server, so the pilot's own repository waits for
  #98.

**Not done, for the owner to schedule.**

1. **The `codeowners` approver kind is not fed.** [Design 05](../design/05-delivery-and-approvals.md)
   resolves it from `CODEOWNERS`, or on Azure DevOps from the "Automatically included reviewers"
   policies whose path filters match. Both connectors read those rules (`Owners()`, covered by the
   conformance suite), but only the definitions trust check uses them: nothing fills
   `policy.Context.Codeowners`. A rule whose approvers are `codeowners` can therefore never be
   satisfied, and `ownership.codeowners: true` ([design 02](../design/02-resource-types-and-generators.md))
   is validated but not applied. Epic #34 lists it; open question 3 assumes it.
2. **Build validation.** The exit's "the build validation pipeline plans" waits for Phase 2. Accept
   that, or add a pipeline to the sandbox as a Phase 2 task.
3. **Nightly live tests.** They need the `AZURE_DEVOPS_PAT` repository secret. The workflow file
   was invalid from P1-19 until f75c18b, so no nightly run has happened; the live tests have run
   locally only.
4. **Open question 3** (Quai's approval as the only authority) was needed by Phase 1: what is built
   follows its default, apart from item 1.
5. **The epics #30–#40** have every task done or moved; they can be closed with this issue.
