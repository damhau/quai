# Runs, status and drift

Status: Draft · 2026-10-26

How Quai learns what happened after it committed: plans, applies, syncs and
drift. Quai never runs Terraform or touches cloud APIs for declarative
changes; it reads what the customer's runners report.

## Concepts

| Concept | Meaning |
| --- | --- |
| **Stack** | What plans and applies as a unit: a Terraform root module (`envs/prod`), an Argo CD application, a Flux Kustomization. Derived from the type's `stack` pattern. |
| **Run** | One execution for a stack at a commit: `plan`, `apply`, `sync`, `drift-check`. |
| **Run report** | What a runner sends Quai about a run. |

Several resources share a stack, and one apply covers every change merged
since the previous apply. Status is therefore reported **per stack and commit**
and derived per resource, not reported per resource as in the prototype.

## Run report protocol

Quai records each report as a run and shows the latest apply of a resource's stack on the
resource. The run token is configured per repository as its SHA-256, in hex
(`spec.runner.reportToken`, a reference to a secret holding the hash: Quai never stores the
token itself). `POST /api/v1/repositories/{name}/run-token` generates one.

Every adapter ends up calling one endpoint, with a token scoped to one repository:

```http
POST /api/v1/runs
Authorization: Bearer <repository run token>
Content-Type: application/json
```

```json
{
  "repository": "infra-live",
  "stack": "envs/prod",
  "commit": "7dc96c1e…",
  "pullRequest": "412",
  "kind": "plan",
  "status": "succeeded",
  "startedAt": "2026-10-26T09:12:03Z",
  "finishedAt": "2026-10-26T09:13:40Z",
  "url": "https://gitlab.acme.ch/infra/live/-/pipelines/88121",
  "summary": { "add": 1, "change": 0, "destroy": 0 },
  "plan": { "format": "terraform-json", "ref": "artifact://plan.json" },
  "policies": [
    { "name": "vm-tags-required", "result": "passed" },
    { "name": "no-public-ip", "result": "warning", "message": "…" }
  ],
  "cost": { "currency": "CHF", "monthlyDelta": 142.4 }
}
```

- `kind` is `plan`, `apply`, `sync` or `drift-check`; `status` is `running`, `succeeded` or `failed`. `pullRequest`, the times, `url`, `summary`, `policies` (each `passed`, `warning` or `failed`) and `cost` (an ISO 4217 currency) are optional; without `startedAt`, a run starts when Quai receives its first report, and without `finishedAt` it finishes when Quai receives the result.
- **One run, several reports.** A report continues the run of the same repository, stack and kind that has the same `url`, whatever its status (a run's page names it: a re-run keeps it, and an adapter reading the same run again changes nothing), or, when either has no `url`, the running one with the same commit and pull request: a runner reports `running` when it starts and the result when it ends, and Quai keeps one run. Anything else is a new run. What a later report leaves out (`url`, `summary`, `policies`, `cost`, `startedAt`) is kept.
- **The request.** A run belongs to the request whose pull request it names, or else to the request whose change set its commit landed (the direct commit, or the pull request's merge commit; an abbreviated commit matches by prefix).
- `GET /api/v1/repositories/{name}/runs` lists a repository's runs, newest first, narrowed by `stack` and `kind`.
- **The plan** is optional. The runner sends the `terraform show -json` output as `plan.content` (as `quai report` does), or names it with `plan.ref`, `artifact://<artifact>/<file>`, for Quai to read from the artifacts of the run its `url` names, through the runner's adapter (on Azure Pipelines, the run's signed content: the PAT goes only to the collection's host). A report whose plan cannot be read is refused, saying why. Quai parses it with `hashicorp/terraform-json` and maps each change to a Quai resource through the types on the repository's default branch: a change belongs to the resource of the type whose `stack` pattern matches the run's stack and whose `address` pattern matches its address, or a module that contains it (`module.vm["{name}"]` owns `module.vm["web01"].terraform_data.vm`). Changes no type claims, and changes several resources claim, are listed apart. Without a `summary`, the counts come from the plan, a replace counting as one added and one destroyed.
- Plan JSON can contain sensitive values, so Quai never keeps the plan: only, for each change, its address, its action and, for an update or a replace, the dotted paths of the attributes it changes. With `spec.runner.planValues`, it also keeps their values before and after, except those Terraform marks sensitive and any value holding a string marked sensitive elsewhere in the plan: Terraform does not mark every copy of one (`terraform_data`'s output holds its input unmarked).
- The same CLI binary provides `quai report`, a step customers drop into any pipeline: it reads the plan file and posts the report.

## Adapters

| Runner | How results reach Quai |
| --- | --- |
| Azure Pipelines (first) | A `quai report` step (or task) after plan and apply. `spec.runner.pipelines` names the pipelines, each with its kind and the stacks it covers ([repository connections](01-repository-connections.md)). Their runs' state changes arrive through a service hook (`ms.vss-pipelines.run-state-changed-event`), and a pull request's build validation runs are read through its policy evaluations when Quai polls; Quai reads each run again and records it as a run of each stack its pipeline covers (a pattern: each stack of the repository's resources it matches), named by its results page, the page `quai report` names: both describe one run. Drift checks are queued with the Pipelines runs API. |
| GitHub Actions, GitLab CI, Jenkins | A `quai report` step after plan and apply (container image or binary). Quai also reads check and pipeline status from platform webhooks as a fallback. |
| Atlantis | A custom workflow step calling `quai report`; Quai also reads the `atlantis/plan` and `atlantis/apply` statuses. |
| Spacelift | Spacelift webhooks for run state changes; plan JSON through its API. |
| HCP Terraform / Terraform Enterprise | Run notifications (webhook); plan JSON through the API. |
| Argo CD | Notifications controller template posting sync and health per application (the prototype's template, upgraded to the run protocol). |
| Flux | Notification-controller `Alert` with a generic webhook provider. |

## Resource status

For each resource, Quai knows the commit that last changed its region (from
the inventory index: for a directory, the newest change under it). Status is derived from the
runs of its stack on the default branch (runs without a pull request: a pull request's plans
belong to its request), and only from those at or after that commit, its descendants in Git.
The first row that holds decides:

| Condition | Status |
| --- | --- |
| An open request changes the resource's region and is not merged yet | Pending change |
| The latest `plan` or `apply` is running | Planning / Applying |
| A finished `drift-check` reports changes to the resource, newer than the latest finished `apply` | Drifted |
| The latest `apply` succeeded | Applied |
| The latest `apply` failed | Failed |
| No run reported since the last change, or no `spec.runner` | In repo |

The status comes with the run it follows from, and one apply covers every change merged
before its commit.

Human commits are handled the same way: when a push changes resource files,
the index records the new last-change commit, and the next apply for that stack
updates the status. The resource page shows commits by people and by Quai on
one timeline, Quai's with the request each delivered (its `Quai-Request` trailer). The
repository's page lists the latest runs of each stack.

## Runs and requests

With `spec.runner`, a merged request (its pull request merged, or its direct commit made) is
*Applying*. It waits for an apply of each stack its change is in (the stack of the resource,
or of each resource of a blueprint) that includes the commit that landed it: the latest such
apply of each stack decides, *Applied* when each succeeded, *Failed* when one failed. An apply
Quai receives before it sees the merge counts too; a request whose resources have no stack is
applied at once, and without a runner, merged means applied. A failed apply is not retried by
delivering again: the change is in Git, and the next apply reports on it.

A request's timeline records each finished plan of its pull request (`plan_reported`, with its
summary, cost and policy results) and its apply (`applied` or `apply_failed`, with the run).
The apply events are also on the timeline of the request's target, which the resource's page
lists (for a blueprint instance, the instance's). Sending them by email, Teams or Slack comes
with the notification channels ([notifications](05-delivery-and-approvals.md#notifications)).

## The request timeline

```
Submitted by bob · 09:11
PR !412 opened · 09:11
Plan succeeded · 09:13      +1 ~0 −0 · CHF +142/month · policies 1 warning
Approved by alice · 09:40   (step-up auth)
Merged · 09:41              7dc96c1
Apply succeeded · 09:46     web01 created
```

## Drift

Drift is the difference between Git and reality. Quai detects it through the
runner:

1. A scheduled job per stack (default daily) asks the runner for a drift check: it triggers a pipeline through the platform API (Azure Pipelines runs API first), or uses the runner's own feature (Spacelift drift detection, HCP Terraform health assessments).
2. The check runs `terraform plan -detailed-exitcode` (exit code 2 means changes) and reports `kind: drift-check` with plan JSON.
3. Quai maps the drifted addresses to resources and marks them *Drifted*, with the attributes that differ.

On Azure Pipelines, the drift check is the pipeline of `spec.runner.pipelines` whose kind is
`drift-check` (one at most). Quai queues it on the default branch for each stack it covers once
per `spec.runner.driftInterval` (24h by default, at least 1h): a stack is due when its latest
drift check started that long ago, or never. A pipeline whose `stacks` is a pattern is queued
once per stack with the template parameter `stack`, which its YAML declares, and its run counts
for that stack only. The run is recorded as soon as it is queued, so the next schedule does not
queue it again; its state and its `quai report` continue it. A repository admin queues the checks
at once with `POST /api/v1/repositories/{name}/drift-checks` (the repository's page: *Check
drift*), for every stack or one. The latest finished drift check decides: a clean one, newer
than the drifted one, clears *Drifted*.

Actions on a drifted resource:

| Action | Effect |
| --- | --- |
| **Reconcile** | Trigger an apply of the stack, which restores what Git says. Subject to the type's approval rules. |
| **Adopt into Git** | Change the resource file to match reality. Requires an `adopt` mapping in the type (state attributes → file fields). Later phase. |
| **Acknowledge** | Record that the drift is known and accepted until a date. |

## Actual state

Quai does not read Terraform state by default: state files contain secrets and
access to them is the most sensitive permission in an IaC estate. Observed
state comes from run reports. Two optional, read-only observers are planned:

- **Kubernetes:** status and conditions of the resources a `dir` region renders (KubeVirt VMs, Crossplane claims), through a read-only service account.
- **Provider APIs:** per-type observer plugins (vSphere, cloud) for attributes worth showing live, such as power state or IP address.
