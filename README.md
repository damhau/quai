# Quai

A self-service infrastructure portal in the spirit of vRealize Automation, built the modern way: **every request becomes a reviewed Git commit**. Quai is a front end to GitOps, not a system of record. Argo CD or Flux reconciles what Quai commits, and Crossplane, KubeVirt or Terraform provision it.

The plan for turning this prototype into an enterprise product (Go backend, React frontend, multi-repository pull-request delivery) is in [`docs/`](docs/README.md).

| vRA concept | Quai | Inspired by |
| --- | --- | --- |
| Service Broker catalog | Catalog of **blueprints** with dynamic request forms | Port |
| Cloud template designer | **Canvas designer**: drag components, wire them, bind properties to form inputs, YAML kept in sync | Massdriver, Brainboard |
| Approval policies | **Access-request style approvals**: reason required, per-rule reviewer groups and thresholds, no self-approval, one denial rejects, TTL | Teleport |
| Day-2 actions | Built-in *edit* and *delete*, plus blueprint-defined actions (resize, scale, upgrade…) with their own approval rules | Port actions |
| Inventory | Read **from Git**: blueprint deployments, plus every file that matches a **resource type**, including files written before Quai existed | GitOps |
| Existing IaC | **Resource types** describe an existing repository's layout and JSON Schema; Quai edits those files with minimal diffs and leaves Terraform alone | Backstage templates, Port |
| vRO workflows | None. The commit is the integration point; controllers do the work | Crossplane, Argo CD |

## How it works

```
 request form ──► plan (render + diff + matching rules) ──► approvals ──► git commit ──► Argo CD / Flux ──► Crossplane / KubeVirt / tf-controller
                                                                              ▲                    │
                                                                              └── sync webhook ◄───┘
```

1. A user picks a blueprint and fills in the form. Quai renders the exact files it would commit and shows the diff and the required approvers live, before anything is submitted.
2. On submit, the rendered files are **frozen into the request**. Reviewers approve that diff, and that diff is what gets committed. If the deployment's directory changed in Git in the meantime (someone pushed by hand, or another request landed), the commit is refused as stale instead of applying something nobody reviewed.
3. Commits are authored as the requester, with `Quai-Request`, `Requested-by` and `Approved-by` trailers, so `git log` is the audit trail.
4. Argo CD (or Flux) reconciles. Its notifications post sync and health back to `/api/webhooks/sync`, and the portal shows live status.

SQLite only holds workflow state: requests, reviews, the event timeline and the last sync status. Delete it and the inventory still rebuilds from Git.

## Quick start

Requires Node 22+ and `git`.

```bash
npm install
npm run build        # builds the web UI
npm start            # http://localhost:8080
```

With no configuration, Quai creates a local bare repository at `data/gitops.git`, seeds it with [`examples/gitops-repo`](examples/gitops-repo) (blueprints plus a small Terraform repository with existing VMs), runs in **dev auth mode** (a user switcher in the sidebar), and simulates a GitOps controller reporting "Synced / Healthy" a few seconds after each commit.

For development with hot reload: `npm run dev` (API on :8080, UI on :5173).

Or with Docker, no Node needed:

```bash
docker compose up --build   # http://localhost:8080
docker compose down -v      # stop and start over with a fresh repository
```

The compose file has commented settings for pointing Quai at your own repository, either a bare clone on your PC or a remote over SSH with a deploy key. Set `QUAI_GIT_SEED: ""` whenever the repository is yours, so the demo content is never copied into it.

Try it like this:

1. As **Bob** (developer), request a *Linux virtual machine* with `environment: prod` and `size: large`. The preview shows that the `production` rule (platform-admins) and the `large-vm-cost` rule (finops) both apply.
2. Switch to **Alice** and then to **Fiona**, and approve from *Approvals*. The second approval commits. Open *Git log* to see the commit.
3. As Bob, open the deployment and run **Resize** or **Extend disk**. The modal shows the minimal diff before you submit.
4. As Alice, open the **Blueprint designer**, drop a VM and a PostgreSQL database onto the canvas, connect them, and click *Commit to catalog*.
5. Open **Inventory**. The four *Virtual machine (Terraform)* rows are hand-written files from the example repo, not Quai deployments. As Bob, open `web01` and **Resize** it: the diff touches only the `cpu` line and keeps its `# bumped in INC-4411` comment. `legacy-ftp` has no owner, so only Alice can change it.
6. From the catalog, request a new *Virtual machine (Terraform)*. The form is generated from `catalog/schemas/vm.schema.json`, and the preview shows the one file Quai will add under `envs/<env>/vms/`.

Tests: `npm test` (core unit tests, plus API tests that drive real commits into a temporary repository). Typecheck: `npm run typecheck`.

## Blueprints

Blueprints are YAML files in the GitOps repository under `blueprints/`, so the catalog itself is versioned and reviewable. The designer edits the same format; see [`examples/gitops-repo/blueprints/`](examples/gitops-repo/blueprints).

```yaml
apiVersion: quai.dev/v1alpha1
kind: Blueprint
metadata: { name: postgres, title: PostgreSQL database, icon: database, category: Data, version: 2.0.1 }
spec:
  inputs:                                   # the request form
    - { name: environment, type: enum, options: [dev, test, prod], default: dev, required: true }
    - { name: storageGB, type: integer, min: 10, max: 4096, default: 20 }
    - { name: highAvailability, type: boolean, default: true, visibleIf: "inputs.environment == 'prod'" }
  approval:
    rules:
      - { name: production, when: "inputs.environment == 'prod'", approvers: [group:platform-admins] }
      - { name: dba-review, when: "inputs.storageGB > 500", approvers: [group:dba], min: 1 }
  resources:                                # the canvas
    - id: db
      type: crossplane.postgres
      position: { x: 240, y: 120 }
      properties:
        storageGB: ${{ inputs.storageGB }}
        backupRetentionDays: "${{ inputs.environment == 'prod' ? 30 : 7 }}"
  connections: []                           # edges: app -> db passes db's connection secret to app
  actions:                                  # day-2
    - name: resize-storage
      inputs: [{ name: storageGB, type: integer, min: 10, max: 4096, required: true }]
      set: { storageGB: ${{ action.storageGB }} }
  path: deployments/${{ deployment.name }}  # optional; where the deployment is written
```

**Expressions** (`when`, `visibleIf`, `availableIf`, `${{ }}`) use a small sandboxed language: literals, dotted paths, `== != < <= > >= && || ! in + - * /` and `a ? b : c`. There is no `eval`, and the same engine runs in the browser and on the server. A binding that is exactly `${{ expr }}` keeps its type (a number stays a number). Manifests are built as data and serialized with a YAML library, so user input can never inject YAML structure.

**Approval rule scope:** `inputs` (the resulting inputs), `previous` (the inputs before a day-2 change), `deployment`, `request.kind`, `request.action`, `action` (the action inputs) and `user`. For example, to require FinOps only when a VM *becomes* large, use `when: "inputs.size == 'large' && previous.size != 'large'"`.

**Components** (`packages/core/src/components.ts`):

| type | renders |
| --- | --- |
| `kubevirt.vm` | KubeVirt `VirtualMachine` with a DataVolume root disk and optional cloud-init SSH key |
| `crossplane.postgres` | `PostgreSQLInstance` claim with a connection secret |
| `crossplane.bucket` | `ObjectBucket` claim with a connection secret |
| `k8s.namespace` | `Namespace` and `ResourceQuota` |
| `argocd.app` | Argo CD `Application` (Helm). Connected resources' secrets become Helm parameters |
| `terraform.module` | Flux tf-controller `Terraform` resource with variables and an outputs secret |
| `raw` | Any manifest, with bindings |

The Crossplane claim kinds (`database.platform.quai.dev`, `storage.platform.quai.dev`) are placeholders for your own XRDs; adapt the component renderers to the compositions your platform team publishes.

Each deployment directory gets the rendered manifests, a `kustomization.yaml`, and a `quai.yaml` record (blueprint, version, owner, inputs). Day-2 actions re-render the whole directory from the current blueprint, so publishing a new blueprint version upgrades deployments the next time they change, and the diff shows exactly what that means.

## Resource types: plugging into an existing IaC repository

Blueprints suit things Quai renders from scratch. Most organizations already have an IaC repository with its own conventions, for example Terraform stacks per environment that read one YAML file per resource:

```hcl
# envs/prod/main.tf
locals {
  vms = { for f in fileset("${path.module}/vms", "*.yaml") : trimsuffix(f, ".yaml") => yamldecode(file("${path.module}/vms/${f}")) }
}
module "vm" {
  source   = "../../modules/vm"
  for_each = local.vms
  cpu      = each.value.cpu
  # ...
}
```

A **resource type** teaches Quai that layout without changing it. It lives in the repository under `.quai/types/`:

```yaml
apiVersion: quai.dev/v1alpha1
kind: ResourceType
metadata: { name: vm, title: Virtual machine (Terraform), icon: server, category: Compute, version: 1.0.0 }
spec:
  locate:
    strategy: file                       # one file per resource
    path: envs/{env}/vms/{name}.yaml     # {params} identify a resource
  params:
    - { name: env, type: enum, discover: true }   # offer the environments that exist in the repo
    - { name: name, pattern: "[a-z][a-z0-9-]{1,30}" }
  schema: catalog/schemas/vm.schema.json # the file contract; CI can validate with the same file
  ownerField: owner                      # set to the requester on create; owners may run day-2
  stack: envs/{env}                      # where plan/apply runs (recorded as Quai-Stack trailer)
  ui:
    order: [description, template, cpu, memory_gb]
    fields:
      template: { immutable: true }
      backup_policy: { visibleIf: "inputs.backup == true" }
  approval:
    rules:
      - { name: production, when: "params.env == 'prod'", approvers: [group:platform-admins] }
      - { name: large-vm, when: "inputs.cpu > 8 && !(previous.cpu > 8)", approvers: [group:finops] }
  actions:
    - name: extend-disk
      inputs: [{ name: size_gb, type: integer, min: 10, max: 4096, required: true }]
      set: { disks.0.size_gb: ${{ action.size_gb }} }   # dotted paths into the file
```

What Quai does with it:

- **Inventory.** Every file matching `locate.path` is a resource, whoever wrote it. Unparseable files are listed but never edited.
- **Forms.** The request form is generated from the JSON Schema: nested objects, arrays of objects, string maps, enums of any type, `default`, `minimum`/`maximum`, `format`. Layout hints live in the type, not the schema, so the schema stays valid for strict CI validators.
- **Minimal diffs.** On day-2, Quai splices changed values into the original text, so comments, alignment and key order survive and reviewers see only the lines that changed. Structural changes (adding or removing keys, resizing lists) fall back to a tree edit that keeps comments but may normalize spacing. New files follow the schema's key order.
- **Guardrails.** Path params are validated as single safe path segments. The owner field, fields mirrored from params (`paramFields`) and `immutable`/`readOnly` fields cannot be changed through an edit. Fields the schema doesn't describe are preserved. Approval rules see `inputs` (the resulting file content), `previous`, `params`, `request` and `user`.
- **Who can change what.** The owner recorded in `ownerField`, principals listed in `operators`, and admins. Resources without an owner are admin-only.
- **Status.** Committed changes show as *Progressing* until your pipeline posts to `/api/webhooks/sync` with `deployment: <type>/<key>` (for example `vm/prod/web01`).

Only the `file` strategy exists today. Entries inside shared files (`entry`) and patching a subtree of a file owned by someone else (`field`) are next.

## Running it for real

Copy [`quai.config.example.yaml`](quai.config.example.yaml) to `quai.config.yaml`. Environment variables override it: `PORT`, `QUAI_DATA_DIR`, `QUAI_GIT_REMOTE`, `QUAI_GIT_BRANCH`, `QUAI_GIT_SEED`, `QUAI_AUTH_MODE`, `QUAI_WEBHOOK_TOKEN`, `QUAI_SIMULATE_SYNC`.

- **Git:** point `git.remote` at your GitOps repository. Quai pushes directly to the branch, retries on non-fast-forward, and picks up commits made outside Quai. If the branch is protected, give Quai's deploy key a bypass; approvals already happened in Quai.
- **Auth:** `auth.mode: proxy` trusts `X-Forwarded-User`, `X-Forwarded-Email` and `X-Forwarded-Groups` from an authenticating proxy such as oauth2-proxy (OIDC against Keycloak, Entra ID, Okta…). Only run proxy mode behind that proxy. **Dev mode lets anyone pick any identity.**
- **Status:** [`deploy/argocd-notifications.yaml`](deploy/argocd-notifications.yaml) contains an ApplicationSet (one Application per `deployments/*` directory) and the notification template that posts to Quai. Set `webhookToken`. Any other system can post `{deployment, revision, sync, health, message}` too.
- **Container:** `docker build -t quai .`, then mount `/data` and your config.

## API

| | |
| --- | --- |
| `GET /api/blueprints`, `GET/PUT /api/blueprints/:name` | catalog; `PUT` validates and commits (admins only) |
| `POST /api/plan` | dry run of a new request: files, diff, matching rules |
| `POST /api/requests` | submit a new deployment request |
| `GET /api/requests?view=mine\|inbox\|all`, `GET /api/requests/:id` | requests, with approval status per rule |
| `POST /api/requests/:id/review` | `{decision: approve\|deny, comment}` |
| `POST /api/requests/:id/cancel`, `/retry` | |
| `GET /api/deployments`, `GET /api/deployments/:name` | blueprint deployments, with files, history, actions |
| `POST /api/deployments/:name/actions/:action[/plan]` | day-2: `update`, `delete` or a blueprint action |
| `GET /api/inventory` | deployments and typed resources in one list |
| `GET /api/types`, `GET /api/types/:type` | resource types with resolved schema and discovered params |
| `POST /api/types/:type/plan`, `POST /api/types/:type/requests` | `{params, document, reason}`; errors on params are keyed `@param` |
| `GET /api/types/:type/resources/:key` | a resource (`key` is the URL-encoded params, e.g. `prod%2Fweb01`) |
| `POST /api/types/:type/resources/:key/actions/:action[/plan]` | `update` takes `{document}`, custom actions take `{inputs}` |
| `GET /api/git/log`, `GET /api/git/commits/:sha`, `POST /api/git/sync` | |
| `POST /api/webhooks/sync` | sync/health reports (`Authorization: Bearer <webhookToken>`) |

## Layout

```
packages/core   blueprints, resource types, expressions, rendering, JSON Schema, YAML patching, approval policy
server          Fastify API, Git engine, SQLite workflow store
web             React UI: catalog, schema forms, approvals, inventory, designer (React Flow)
examples        gitops-repo: seed with blueprints and a small Terraform repository
deploy          Argo CD integration
```

## Not done yet

- Pull-request mode: commit to a branch, let CI run `terraform plan` and policy checks, show the plan on the request page, and merge when Quai's approval (a required `quai/approval` status check) passes. Approvals are invalidated if the branch has to be rebuilt.
- Status reported per stack (one apply covers every resource changed in that commit).
- `entry` and `field` locate strategies for shared files such as `namespaces.yaml`.
- Imperative day-2 actions (reboot, snapshot, console) that call an API directly and are written to the audit trail, next to the Git-backed ones.
- Assisted onboarding: propose resource types for an existing repository, and accept a type only if parsing and re-writing every existing file reproduces it exactly.
- Cost estimates on the request form, and quotas per team.
- Lease or expiry on deployments, with automatic delete requests.
- Dynamic form options from live data (clusters, networks, existing secrets).
- A component SDK so platform teams can register components without touching core.
