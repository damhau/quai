# Repository connections

Status: Draft · 2026-10-26

How Quai connects to customer repositories, and how catalog items find the
repository, branch, path and pipeline they belong to.

## The problem

To turn a form into a change, Quai has to know, for every resource:

- which repository and branch it lives in, and with which credentials to reach it;
- where in that repository it lives, and in which file format;
- how a form becomes file content, and how file content becomes a form again;
- how the change is delivered (pull request or direct commit) and who must approve it;
- which pipeline applies it, and where the result is reported.

The prototype answered all of this with one hard-coded repository. The
enterprise design splits it into three layers, each owned by different people.

## Three layers

```mermaid
flowchart TB
  subgraph Q[Quai — owned by platform admins]
    RC[Repository connection<br/>platform · URL · credentials · delivery · runner]
  end
  subgraph IAC[Each IaC repository — owned by its maintainers]
    DEF[.quai/types/*.yaml<br/>locator · schema · generator · stack · approvals · actions]
    FILES[envs/prod/vms/web01.yaml<br/>the resources themselves]
  end
  subgraph CAT[Catalog repository — owned by the catalog team]
    BP[blueprints/*.yaml<br/>composite catalog items that reference types]
  end
  RC -->|reads definitions from| DEF
  DEF -->|locates| FILES
  BP -->|"type: {repository, name}"| DEF
```

| Layer | Question it answers | Owner | Stored in |
| --- | --- | --- | --- |
| **Repository connection** | How do I reach this repository, deliver changes to it and learn their outcome? | Platform admins | Quai (UI/API), optionally config-as-code |
| **Resource types** | What lives in this repository, where, in which shape, and how do I edit it? | The repository's maintainers | The repository itself, under `.quai/` |
| **Blueprints** | What can people request, and which types in which repositories does it combine? | Catalog team | A catalog repository |

Why types live in the IaC repository and not in Quai: they describe that
repository's layout, so they must change in the same pull request as the layout
does, and be reviewed by the same people. See
[ADR 0003](../adr/0003-definitions-live-next-to-the-iac.md).

A blueprint is optional. Every type in a connected repository appears in the
catalog as a simple item (one resource, the type's own form) unless it is
marked `catalog: hidden`. Blueprints are for composite items: "a namespace with
its quota, a VM and a DNS record, approved once".

## Repository connection

A connection is created by an admin in the UI or through the API, or declared
as config-as-code (a YAML file loaded at start-up, or a Helm value). Both forms
share one schema:

```yaml
apiVersion: quai.dev/v1beta1
kind: Repository
metadata:
  name: infra-live                 # used in resource IDs: infra-live/vm/prod/web01
  title: Infrastructure (Terraform)
  owners: [team:platform]          # who administers this connection
spec:
  connector: azure-devops          # azure-devops | github | gitlab | bitbucket | gitea | generic
  url: https://devops.acme.ch/tfs/DefaultCollection/Infrastructure/_git/live   # Azure DevOps Server
  defaultBranch: main
  credentials:
    ref: k8s:quai/infra-live-ado-pat     # Kubernetes Secret (pilot); or vault:…, azkv:…, env:…
  definitions:
    path: .quai                    # where types live in this repository
    branch: main                   # definitions are read from here, pinned per request by commit
  delivery:
    mode: pull-request             # pull-request | direct
    branchPrefix: quai/
    merge: auto                    # auto: Quai merges when approved and green | manual: a human merges
    mergeMethod: squash            # merge | squash | rebase (as the platform supports)
    waitForChecks: [plan, policy]  # other checks or branch policies that must pass before merge
    deleteBranch: true
  enforcement:
    requiredCheck: quai/approval   # Quai verifies the branch protection really requires it
    floorRules:                    # approval rules every type in this repo inherits and cannot remove
      - name: production-floor
        when: "params.env == 'prod'"
        approvers: [group:platform-admins]
  runner:
    kind: azure-pipelines          # see runs-status-and-drift.md
    reportToken:
      ref: k8s:quai/infra-live-run-token
  webhooks:
    secret:
      ref: k8s:quai/infra-live-webhook-secret
  sync:
    pollInterval: 5m               # safety net for lost webhooks
```

| Field | Notes |
| --- | --- |
| `metadata.name` | Immutable after creation: it is part of every resource ID and request record. |
| `spec.credentials.ref` | A reference, never the secret itself. Resolved at use time; never logged or returned by the API. |
| `spec.definitions` | One repository can host types for itself only. A type cannot point its locator into another repository. |
| `spec.delivery.mode` | `direct` exists for sandboxes and demos. Production repositories should use pull requests ([ADR 0008](../adr/0008-pull-requests-by-default.md)). |
| `spec.enforcement.floorRules` | Lets admins guarantee, for example, that nothing reaches `prod` without platform approval, whatever a type's author writes. Types can add rules, never remove floor rules. |
| `spec.runner` | Where plan and apply results come from. Optional: without it, resources show *In repo* after merge. |

## Connectors

Each Git platform has its own connector: authentication, pull requests,
statuses, branch policies, ownership rules, webhooks and pipelines. Git
transport (clone, fetch, push) always uses the `git` CLI, so SSH, HTTPS,
proxies and partial clones behave as customers expect; the connector supplies
the credentials. Azure DevOps is built first: Server (on-premises), then Services. The interface, the conformance
suite and the per-platform mappings are in [Git connectors](12-git-connectors.md).

How each platform enforces Quai's approval. Features marked † depend on the
platform's edition or plan; confirm them against the customer's installation
during onboarding.

| Platform | Order | Enforcing `quai/approval` |
| --- | --- | --- |
| Azure DevOps Server (on-premises) | **1st** | Branch policy "Status check" requiring the `quai/approval` PR status, with Quai as the authorized identity and status reset on new changes |
| Azure DevOps Services | **1st, right after or alongside Server** | Same as Server |
| Gitea / Forgejo | with the 1st (tests) | Required status check in branch protection |
| GitHub / GitHub Enterprise Server | 2nd | Required check in branch protection or a ruleset, pinned to the Quai GitHub App |
| GitLab.com / self-managed | 3rd | External status checks†, an approval rule requiring the Quai bot†, or a protected branch only the Quai bot may merge |
| Bitbucket Cloud / Data Center | 4th | Required builds or merge checks† |
| Generic Git | — | Not enforceable; direct delivery only |

## Credentials

Quai needs to: read the repository, push branches under its prefix, open and
complete pull requests, publish statuses, read branch policies, and manage its
webhook subscription.

| Platform | Recommended | Why |
| --- | --- | --- |
| Azure DevOps Server | PAT of a dedicated Active Directory service account, with expiry and rotation | Entra service principals are not available on-premises; Quai warns before the PAT expires. |
| Azure DevOps Services | PAT, as on Server; a Microsoft Entra service principal (workload identity federation, certificate or client secret) for customers who forbid PATs, before GA | The PAT works on both editions with one code path. A service principal has no personal account behind it and Entra's conditional access and audit apply. |
| GitHub | GitHub App installed on the selected repositories | Short-lived installation tokens, fine-grained permissions, verified commits, no user seat. |
| GitLab | Project or group access token for a bot, with `api` and `write_repository` | Scoped to the project or group; rotate with expiry. |
| Generic | SSH deploy key with write access | The only option without an API. |

The exact permissions per platform, and how the registration wizard verifies
them, are in [Git connectors](12-git-connectors.md#permissions).

Secrets are resolved through a `SecretStore` interface. Kubernetes Secrets and
environment variables come first (the pilot uses Kubernetes Secrets); HashiCorp
Vault (KV v2, with Kubernetes or JWT auth) and Azure Key Vault follow when a
customer needs them. Quai checks expiry where the
platform exposes it and warns 30 days ahead.

## Definitions

Layout of `.quai/` in an IaC repository:

```
.quai/
  repository.yaml        # optional defaults for this repo: stacks, labels, catalog category
  types/
    vm.yaml
    namespace-quota.yaml
  schemas/               # optional; schemas can live anywhere in the repo (e.g. catalog/schemas/)
  plugins/
    tfvars.wasm          # WASM generator plugins, pinned by digest in the type
  instances/             # instance records when blueprints target only this repo
```

How Quai loads definitions:

1. On a push webhook (or poll) for the definitions branch, a worker fetches, reads `.quai/` at the new commit, validates every definition with the engine, and stores the parsed result keyed by `(repository, commit)`.
2. Invalid definitions are reported on the repository's health page and in a check on the commit that introduced them. The previous valid version stays active per type, so a broken edit never takes a catalog item down.
3. Every plan records the definitions commit it used. A request is always delivered with the definitions it was planned with; if the type changes before merge, the request is marked stale and must be re-planned.
4. Authors can preview definitions from any branch or open PR ("try this type before merging it"). Previews are visible only to the author and catalog admins and cannot be submitted.

### Trust

Definitions are code: whoever can merge to `.quai/` decides what forms show,
what gets written and which approval rules apply. Quai therefore:

- checks that `.quai/` is covered by `CODEOWNERS` (on Azure DevOps, which has no `CODEOWNERS` file: a required-reviewer branch policy with path filter `/.quai/*`) and that the default branch is protected, and shows a red warning on the repository and its catalog items if not;
- applies the connection's `floorRules` regardless of what types say;
- records in the audit log every change to definitions, with the commit and its author;
- never runs code from the repository except WASM plugins pinned by digest, in a sandbox with no file system or network access ([ADR 0004](../adr/0004-structured-generators-no-text-templates.md)).

## Binding catalog items to repositories

A type belongs to exactly one repository, so a type-backed catalog item needs
no binding. Blueprints reference types by repository and name:

```yaml
apiVersion: quai.dev/v1beta1
kind: Blueprint
metadata: { name: app-environment, title: Application environment }
spec:
  inputs: { $ref: schemas/app-environment.schema.json }
  resources:
    - id: ns
      type: { repository: platform-k8s, name: namespace-quota }
      params: { cluster: "${{ inputs.cluster }}", name: "${{ inputs.app + '-' + inputs.env }}" }
      values: { cpu: "${{ inputs.size == 'large' ? 16 : 4 }}" }
    - id: vm
      type: { repository: infra-live, name: vm, version: ">=1.2" }
      params: { env: "${{ inputs.env }}", name: "${{ inputs.app + '-vm01' }}" }
      values: { cpu: 4, memory_gb: 16, template: ubuntu-24.04 }
      dependsOn: [ns]
```

- `repository` may be omitted for a type defined in the blueprint's own repository.
- `version` constrains the type's `metadata.version`. When a type changes incompatibly, the catalog flags affected blueprints instead of breaking at request time.
- `quai lint` in the catalog repository's CI resolves every reference against the connected repositories and validates `values` against each type's input schema.
- One request on this blueprint produces one change set with one pull request per repository. See [blueprints and change sets](04-blueprints-and-change-sets.md).

## Sync

| Mechanism | Purpose |
| --- | --- |
| Webhooks (push, pull request, check/status, merge) | Primary signal. Verified with the per-repository secret, de-duplicated by delivery ID, processed as jobs. |
| Polling | Safety net: fetch every `pollInterval`, compare heads, reconcile open PRs. Covers lost webhooks and platforms without webhooks. |
| Mirror cache | One bare mirror per repository (`git clone --mirror`, partial clone with `--filter=blob:none` for large monorepos). Each job gets a temporary `git worktree`, with sparse checkout limited to the paths it needs. |
| Inventory index | On every new head, only the paths changed since the last indexed commit are re-read (`git diff --name-only`). A full re-index runs nightly and on demand. |

## Registering a repository

The admin flow, in the UI or API:

1. **Platform and URL.** Choose the platform, paste the URL, select a credential reference.
2. **Access test.** Quai checks read access, that it can push a branch under its prefix (then deletes it), open and close a draft pull request, write a check, and read branch protection. Each check shows pass or fail with the missing permission.
3. **Scan.** Quai looks for `.quai/`. If there is none, it offers the [onboarding assistant](10-onboarding.md), which proposes types from the repository's existing files.
4. **Delivery and enforcement.** Choose the delivery mode and merge policy. Quai shows exactly what branch protection must look like and verifies it.
5. **Runner.** Choose how plan and apply results come back and copy the generated report token.
6. **Webhook.** Installed automatically through the platform API where the credential allows (on Azure DevOps, a service hook subscription), or shown for manual setup.

### Health

Each repository has a health page and metrics:

- credential validity and days to expiry;
- last webhook received, last successful fetch;
- branch protection still requires `quai/approval` (checked hourly). If not, approvals are not enforced: Quai blocks new pull requests to that repository and alerts admins;
- definitions valid at head, with errors per file;
- open Quai pull requests, oldest age, stale count.
