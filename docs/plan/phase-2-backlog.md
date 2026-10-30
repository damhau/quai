# Phase 2: how autonomous sessions work

Status: Done · 2026-10-30 (Phase 3: [Phase 3 backlog](phase-3-backlog.md))

The tasks for [Phase 2](roadmap.md#phase-2-runs-and-status-4-weeks) are GitHub issues in
milestone [M1 Pilot](https://github.com/damhau/quai/milestones) labelled `phase:2` and
`type:task`, titled `P2-NN: …`. Their order is their issue number. Each names the epic it belongs
to (#41–#45); P2-01 (#100, codeowners) moved to Phase 4. GitHub is the only task list:
descriptions and done criteria live in the issues. Read [`CLAUDE.md`](../../CLAUDE.md) first; an
iteration works as in
[Phase 0](phase-0-backlog.md#one-iteration), with `phase:2` instead of `phase:0`:

```bash
gh issue list --repo damhau/quai --label phase:2 --label type:task --state open \
  --json number,title,labels --jq 'sort_by(.number) | .[0]'
```

Commit as `P2-NN: <summary> (closes #<issue>)`; a task may take a few commits, the last one closes
it.

## Test infrastructure

Everything of [Phase 1](phase-1-backlog.md#test-infrastructure) stays: embedded PostgreSQL, Gitea
and the Kubernetes API from their binaries, kind for the chart, and the owner's Azure DevOps
sandbox with its rules for the PAT and for cleaning up. Phase 2 adds:

| What | How |
| --- | --- |
| Terraform plans | Recorded `terraform show -json` outputs in `backend/internal/engine/tfplan/testdata`, produced from the example repository's `envs/prod` stack and small root modules, all using the built-in `terraform_data` resource (no provider downloads), by `generate.sh` beside them. Tests never run Terraform |
| A runner in the compose stack | A fake runner (`backend/tools/fakerunner`, the compose stack's `fake-runner` service, P2-12): on Gitea's pull request and push webhooks, it posts plan and apply reports with `quai report`, plans with the recorded `prod.json`; a request's reason steers it (`[costly]`, `[fail-apply]`). The seed step adds a rule on the plan's cost to the seeded `vm` type (not to `examples/`) |
| Azure Pipelines | YAML pipelines in `quai-test-` repositories of the sandbox, created and deleted by the tests: `adotest`'s `AddFiles`, `Pipeline` (deleted when the test ends), `Run` and `Await`. They run on the organization's Microsoft-hosted agents (`vmImage: ubuntu-latest`): one parallel job and 1800 minutes a month, so pipelines stay short and run one at a time, a run waiting behind another. The sandbox PAT has Build (read and execute). The sandbox cannot reach a test machine: what a pipeline's `quai report` step would post is posted by the test, naming the run's results page, and its run state is read back from the service hook subscription's history, as for webhooks in Phase 1, or handed to Quai as the hook's event |

## STOP tasks

- **P2-13** was rehearsed, with the owner's go-ahead, on a stand-in for the pilot in the sandbox
  organization (`pilot-infra`: synthetic VM files, Terraform stacks and pipelines), as the pilot
  runs Azure DevOps Server and M1 is accepted on Services; the steps are in
  [onboarding an Azure DevOps repository](../operations/deployment-and-operations.md#onboarding-an-azure-devops-repository).
- **P2-08** (pipelines on the sandbox) and **P2-13** (pilot onboarding) need something only the
  owner can provide (named in the issue). If the issue has no comment from the owner saying it is
  in place, comment what is missing and **stop**. When it is there, implement the task and close
  it like any other.
- **P2-14** (Phase 2 review): write the report the issue describes, commit with `refs`, comment
  that it is ready, and **stop**. Only the owner closes it.

The rules of [when to stop and ask](phase-0-backlog.md#when-to-stop-and-ask) apply unchanged.
Open questions 7 and 8 ([open questions](open-questions.md)) have defaults the tasks follow until
the owner answers them.

## Phase 2 review (P2-14)

2026-10-30, after P2-13. The [Phase 2 exit (M1 Pilot)](roadmap.md#phase-2-runs-and-status-4-weeks)
against the tests that show it:
- **On Gitea, on every push:** the backend tests, and the Playwright scenarios with the compose
  stack's fake runner.
- **On the Azure DevOps Services sandbox:** the live tests, nightly since 2026-10-30
  (`.github/workflows/live.yml`) and run locally after each task.
- **On a stand-in for the pilot's repository:** the P2-13 rehearsal on `pilot-infra`.

**The exit, part by part.**

| Exit | Gitea | Azure DevOps Services |
| --- | --- | --- |
| The pilot's users request | e2e `type-request`, `runs` | `TestAzureDevOpsRuns`; on `pilot-infra`, bob created and resized VMs (!122–!124) |
| They approve with the plan in front of them: the plan by resource, what it destroys, its cost and policies; rules on the plan | `TestRunPlans`, `TestPlanRules`, `TestNewPlanNeedsApproval`, `TestPlanBeforeItsPullRequest`, `TestCostAfterThePlan`; e2e `runs` (a costly plan brings FinOps in) | Build validation runs are plans: `TestAzureDevOpsPipelineRuns`. With the `quai report` step's plan and cost, a rule on the cost brings FinOps in: `TestAzureDevOpsRuns`, the test standing in for the step. On `pilot-infra`, the plans' state only (the step cannot reach a local Quai) |
| They see the apply result: the request and the resource Applied or Failed | `TestRequestsAfterMerge`, `TestPipelinesRunner`, `TestResourceStatus`; e2e `runs` (applied, and a failed apply) | `TestAzureDevOpsRuns`: apply pipelines succeeding and failing, their run handed to Quai as the service hook would. Since P2-15, also by polling: in `TestAzureDevOpsRuns` the failed apply, and on `pilot-infra` a resize (!131) Applied with its VM when Quai polled after apply run 108, nothing reaching Quai |
| On their own repository and pipeline | — | A stand-in only: `pilot-infra`, with the onboarding pull requests (!119–!121) and pipelines that plan, apply and lint. The pilot's own repository waits (below) |

**Also delivered.**
- **Run reports and `quai report`:** the run report protocol and `quai report`, with plan JSON
  parsing and per-resource mapping (`internal/engine/tfplan`, `quai lint --plan`).
- **The Azure Pipelines adapter:** service hooks, build validation, artifacts, and its drift
  checks (`TestDriftChecks`, `TestAzureDevOpsDrift`).
- **Runner templates** for Azure Pipelines, GitHub Actions, GitLab CI, Argo CD and Flux
  (`TestArgoCDTemplate`, `TestFluxEvents`).
- **The compose stack's fake runner.**
- **The onboarding steps** for an Azure DevOps repository
  ([operations](../operations/deployment-and-operations.md#onboarding-an-azure-devops-repository)).

**Moved out of Phase 2.**
- **#100**, codeowners (P2-01): to Phase 4, after M1 (owner, 2026-10-29). Open question 9 stays
  open with it.
- **#98**, Azure DevOps Server 2022: in the
  [future improvements](roadmap.md#future-improvements) since Phase 1, so the pilot's own Server
  repository is not reachable yet.

**Not done, for the owner to decide or schedule.**

1. **The pilot's own repository.** Decided (owner, 2026-10-30): with no real repository
   available, M1 is accepted on the stand-in `pilot-infra`. A real repository is onboarded with
   the steps rehearsed there when one is available.
2. **Apply results without inbound access (open question 10).** Decided (owner, 2026-10-30):
   Quai polls the apply and drift-check pipelines' runs, built in P2-15 (#114). A plan's details
   still need the `quai report` step, or the service hooks, to reach Quai.
3. **Defaults to confirm.**
   - **Open question 7:** rules on the plan. P2-12 decides at once a condition whose result does
     not depend on the plan.
   - **Open question 8:** `spec.runner.pipelines`.
4. **The M1 deployment, beyond Phase 2's tasks.**
   - **Entra ID login:** OIDC is tested against a fake identity provider (`TestOIDCLogin`), not
     against Entra ID.
   - **OpenShift:** the chart is installed and upgraded on kind (`kind.yml`), not on OpenShift. A
     `Route` and the UBI image variant are in the
     [operations](../operations/deployment-and-operations.md#packaging) doc, not built.
5. **The stand-in's resources** in the sandbox organization stay, with item 1: the
   `pilot-infra` repository, its pipelines (45, 46) and its policies (68–70).
6. **The epics #41–#45** have every task done or moved; they can be closed with this issue.
