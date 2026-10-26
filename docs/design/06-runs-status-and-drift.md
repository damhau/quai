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

- `plan.ref` is optional. When present, the runner uploads the `terraform show -json` output (or Quai fetches it from the CI artifact), and Quai maps resource changes to Quai resources through each type's `address` pattern, using `hashicorp/terraform-json`.
- Plan JSON can contain sensitive values. Quai stores only addresses, actions and changed attribute names by default; attribute values are kept only for attributes marked non-sensitive, and only if the repository opts in.
- The same CLI binary provides `quai report`, a step customers drop into any pipeline: it reads the plan file and posts the report.

## Adapters

| Runner | How results reach Quai |
| --- | --- |
| Azure Pipelines (first) | A `quai report` step (or task) after plan and apply. Build validation results are also read through the Azure DevOps policy evaluations API, and pipeline run state changes through service hooks. Drift checks are queued with the Pipelines runs API. |
| GitHub Actions, GitLab CI, Jenkins | A `quai report` step after plan and apply (container image or binary). Quai also reads check and pipeline status from platform webhooks as a fallback. |
| Atlantis | A custom workflow step calling `quai report`; Quai also reads the `atlantis/plan` and `atlantis/apply` statuses. |
| Spacelift | Spacelift webhooks for run state changes; plan JSON through its API. |
| HCP Terraform / Terraform Enterprise | Run notifications (webhook); plan JSON through the API. |
| Argo CD | Notifications controller template posting sync and health per application (the prototype's template, upgraded to the run protocol). |
| Flux | Notification-controller `Alert` with a generic webhook provider. |

## Resource status

For each resource, Quai knows the commit that last changed its region (from
the inventory index). Status is derived from the runs of its stack:

| Condition | Status |
| --- | --- |
| An open request touches the resource | Pending change |
| Latest run for the stack at or after the resource's last-change commit is `plan` or `apply` and running | Planning / Applying |
| Latest `apply` at or after the last-change commit succeeded | Applied |
| Latest `apply` that includes the last-change commit failed | Failed |
| Latest `drift-check` reports changes for the resource's address | Drifted |
| No run reported since the last change | In repo |

Human commits are handled the same way: when a push changes resource files,
the index records the new last-change commit, and the next apply for that stack
updates the status. The resource page shows commits by people and by Quai on
one timeline.

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
