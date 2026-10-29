# Open questions

Status: Draft · 2026-10-27

## Still open

Decisions still needed, with the default the plan assumes until they are answered.

| # | Question | Why it matters | Default assumed | Needed by |
| --- | --- | --- | --- | --- |
| 1 | Where do the Azure DevOps Server conformance tests run: our own Server 2022 lab VM, a test project on the pilot's Server, or both? | Nightly tests against the real platform; acceptance before releases | Both: our lab VM (licensing to check) for nightly runs, a test project on the pilot's Server for acceptance | Server support ([future improvement](roadmap.md#future-improvements)) |
| 2 | Pilot Server details: which collection(s) hold the IaC repositories, is the `/tfs` virtual directory in use, who can grant the service account its branch-folder permissions (`tf git permission`) and create the service hook subscription, and what maximum PAT lifetime does the Server allow? | URL parsing, registration wizard, rotation reminders | One collection, `/tfs` in use; a project collection administrator does the setup once; PAT lifetime 1 year | Server support ([future improvement](roadmap.md#future-improvements)) |
| 3 | Is Quai's approval the only authority, or must Azure DevOps required-reviewer policies also pass? | Auto-complete vs waiting for both; approver kinds | Quai is the sole authority ([ADR 0002](../adr/0002-quai-is-the-approval-authority.md)); Azure DevOps path-based reviewers feed Quai's rules through the `codeowners` approver kind | Phase 1 |
| 4 | Is there an ITSM tool (ServiceNow, Jira Service Management) that must hold change records? | Phase 4 scope | ServiceNow in Phase 4 | Phase 4 |
| 6 | Which PostgreSQL major version must Quai support at the pilot (the version the pilot can run on OpenShift)? | Tests run against one version (embedded PostgreSQL); the Helm chart and docs name it | PostgreSQL 17; tests run on 17 | P0-15 |

## Decided

| Question | Decision | Date | Consequences |
| --- | --- | --- | --- |
| Azure DevOps Server in Phase 1 and M1 | **Neither: Phase 1 and M1 are accepted on Azure DevOps Services**; Server 2022 support is a future improvement | 2026-10-29 | #98 (formerly P1-22) leaves M1 and has no phase ([roadmap](roadmap.md#future-improvements)); [ADR 0010](../adr/0010-git-connectors-azure-devops-first.md) amended. Questions 1 and 2 wait for it |
| How Quai authenticates to Azure DevOps Services | **A PAT, as on Server**; the Entra service principal moves to GA, for customers who forbid PATs | 2026-10-28 | One authentication path for both editions in M1; #97 (formerly P1-21) in M3 GA |
| Where Phase 1 develops and tests the Azure DevOps connector | **On the owner's throwaway Azure DevOps Services organization** (`dh-consulting`, project `quai-sandbox`, PAT), and Gitea; a Server 2022 instance when available | 2026-10-28 | The connector targets API 7.0 and is tested on Services with PAT authentication; Server support became a future improvement (above) |
| The prototype's Kubernetes blueprints | **Not carried over; the examples are VM-first** | 2026-10-27 | The examples keep the `vm` type and add blueprints built from it; the `component` generator leaves Phase 0 (#10); the walkthroughs of #27 use VM blueprints |
| YAML editing library | **`go.yaml.in/yaml/v3` alone**, no second decoder; goccy dropped | 2026-10-27 | [Spike report](../engineering/spike-yaml-editing.md); definitions (P0-08) are read with the same conversion |
| Product name | **Keep "Quai"** | 2026-10-27 | Revisit only if a domain or trademark check (Swissreg, EUIPO, class 42) shows a conflict |
| Git platform and edition | **Azure DevOps Server first, Services alongside or right after** | 2026-10-26 | [ADR 0010](../adr/0010-git-connectors-azure-devops-first.md), [Git connectors](../design/12-git-connectors.md). Amended 2026-10-29: Services for M1, Server a future improvement (above) |
| Pilot's Azure DevOps Server version | **Server 2022, latest update** (REST API 7.0) | 2026-10-27 | API 7.0 is the connector's baseline |
| Authentication to Azure DevOps Server | **PAT of a dedicated Active Directory service account** | 2026-10-27 | Scopes Code (read and write), Code (status), Build (read and execute) for pipelines; expiry warnings |
| Network between Server and Quai | **Service hooks can reach Quai; Server uses an internal CA** | 2026-10-27 | Webhooks are the primary signal, polling stays as a safety net; Quai is configured with the internal CA bundle |
| Runner | **Azure Pipelines (YAML)**: plan through build validation on PRs, apply on merge | 2026-10-27 | First runner adapter; a `quai report` step in their pipelines |
| Identity provider for Quai | **Microsoft Entra ID (hybrid AD)** | 2026-10-27 | OIDC with app roles or group claims; see [identity](../design/08-identity-access-tenancy.md#directory) |
| Hosting | **OpenShift** | 2026-10-27 | UBI-based image, `restricted-v2` SCC, Routes ([operations](../operations/deployment-and-operations.md)) |
| Secret store | **Kubernetes Secrets** for the pilot | 2026-10-27 | `SecretStore` Kubernetes backend first; Vault and Azure Key Vault later |
| First resource types | **VMs at the pilot; namespaces and quotas, DNS records, firewall rules at beta** | 2026-10-27 | M1 stays ~week 14 with the `file` locator; the other three types need `entry`/`field` locators and arrive with Phase 3 |
| HCL data | **Some data in HCL; HCL editor at beta** (Phase 3, as planned) | 2026-10-27 | HCL-backed types are a beta requirement for the pilot |
| Product model | **Self-hosted, single-tenant, proprietary** | 2026-10-27 | No multi-organization work before GA |
| LLM-assisted onboarding | **Opt-in, customer-chosen endpoint**; structure only, values redacted | 2026-10-27 | As in [onboarding](../design/10-onboarding.md) |
| Audit retention | **Configurable, default 1 year** | 2026-10-27 | Customers with regulatory duties set longer periods; see [security](../design/09-security-and-compliance.md#data-protection) |
| UI languages | **English only** | 2026-10-27 | No translation framework for now ([frontend](../engineering/frontend.md)) |
| Expressions, state and plugins | **CEL; PostgreSQL with River; WASM plugins** | 2026-10-27 | ADRs [0009](../adr/0009-cel-expressions.md), [0007](../adr/0007-postgresql-for-state-and-jobs.md), [0004](../adr/0004-structured-generators-no-text-templates.md) |
| Backend and frontend stack | **Go backend, React frontend** | 2026-10-26 | [ADR 0006](../adr/0006-go-backend-react-frontend.md) |
| Where autonomous work is committed | **Straight to `main`** | 2026-10-27 | [Phase 0 backlog](phase-0-backlog.md) |
