# Day-2 actions and executors

Status: Draft · 2026-10-26

What owners can do with a resource after it exists.

## Two kinds of action

| | Declarative | Imperative |
| --- | --- | --- |
| Examples | Resize, add disk, change quota, rotate template, delete | Reboot, power cycle, snapshot, open console, restart pods, run a playbook |
| What changes | Desired state in Git | Nothing in Git; an operation on the live system |
| Path | Request → change set → PR → approval → merge → apply | Request → approval (if a rule matches) → executor job |
| Reversible by Git revert | Yes | No |

Both appear side by side on the resource page, so for users it is simply
"manage my VM". Some operations can be either: KubeVirt power state can be a
declarative `runStrategy` field, while a reboot or a snapshot is an action, not
a state. The type decides.

## Declarative actions

Implemented in the prototype for blueprints and types. In the enterprise
version:

```yaml
actions:
  - name: extend-disk
    kind: declarative
    title: Extend system disk
    availableIf: "doc.disks[0].size_gb < 4096"
    inputs:
      - { name: size_gb, type: integer, min: 10, max: 4096, required: true }
    validate: "action.size_gb > doc.disks[0].size_gb"   # disks only grow
    set:
      disks.0.size_gb: "${{ action.size_gb }}"
    approval:
      rules: []                        # extending a disk needs no approval outside prod floor rules
```

- `set` paths are dotted document paths, including list indices.
- Action inputs are prefilled from the current values of the paths they set.
- `validate` is a CEL expression checked at plan time, with a message on failure.

## Imperative actions

```yaml
actions:
  - name: restart
    kind: imperative
    title: Restart
    executor: kubevirt-prod            # an executor configured by admins
    operation: restart
    target:
      namespace: "${{ params.env }}"
      name: "${{ params.name }}"
    confirm: true                      # the UI asks to type the resource name
    timeout: 5m
    approval:
      rules:
        - { name: prod-restart, when: "params.env == 'prod'", approvers: [owners] }
```

### Executors

Admins configure executors; types only reference them by name. A type author
cannot point an action at an arbitrary endpoint.

| Executor | Operations | Credentials |
| --- | --- | --- |
| `kubernetes` | Subresource calls (KubeVirt `restart`, `stop`, `start`, `migrate`), create a `VirtualMachineSnapshot`, roll out a Deployment, scale | A service account per cluster with RBAC limited to the verbs and namespaces the executor allows; optionally impersonating the requester |
| `awx` (Ansible Automation Platform) | Launch a job template with extra vars | An AWX token scoped to the allowed templates |
| `pipeline` | Trigger a GitHub Actions workflow, GitLab pipeline or Azure pipeline with inputs | The repository connection's credentials |
| `webhook` | POST a signed JSON payload to an allow-listed URL; poll or receive a callback for the result | HMAC secret; URL allow-list per executor |
| `vsphere` (later) | Power operations, snapshots, console tickets | A vCenter role limited to these privileges |

### Execution

1. The request is approved (or no rule matched).
2. A worker job resolves the executor, renders `target` and inputs, and calls it with an idempotency key derived from the request ID.
3. Progress and output stream to the request page. Output is truncated and scanned for secrets before it is stored.
4. The result, with the executor's own reference (job ID, operation ID), goes to the audit log.

Imperative actions never run in the API process, and executors never receive
credentials from the browser.

## Lifecycle features

### Leases

```yaml
lease:
  default: 30d
  max: 90d
  when: "params.env != 'prod'"
```

- A resource created with a lease gets an expiry date (stored in the file through a managed field, or in the instance record when the schema has no room for it).
- Owners are reminded 7 days and 1 day before. They can extend up to `max`.
- At expiry, Quai creates a delete request under the type's rules. The default policy for lease-expiry deletes is "owners are notified, no approval needed"; a type can require one.

### Quotas

Per team and per type, computed from the inventory:

```yaml
quotas:
  - team: "*"
    when: "params.env == 'dev'"
    limit:
      count: 20
      sum: { cpu: 64, memory_gb: 256 }
```

A request that would exceed a quota is refused at plan time, or routed to an
extra approval rule if the quota says `onExceed: approval`.

### Cost

- From the runner: Infracost (or the runner's own estimate) in the run report shows the monthly delta for cloud resources.
- For on-premises resources without a price list, types can declare a showback formula: `cost: "${{ inputs.cpu * 12.0 + inputs.memory_gb * 3.5 }}"` (CHF per month). Shown on the form, the request and team reports.
- Cost can drive approval rules (`plan.cost.monthlyDelta > 500`).

### Bulk operations

Select resources in the inventory, run one action on all of them: one request,
one change set, one PR per repository and stack. See
[blueprints and change sets](04-blueprints-and-change-sets.md#bulk-changes).
