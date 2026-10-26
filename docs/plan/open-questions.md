# Open questions

Status: Draft · 2026-10-27

## Still open

Decisions still needed, with the default the plan assumes until they are answered.

| # | Question | Why it matters | Default assumed | Needed by |
| --- | --- | --- | --- | --- |
| 1 | Where do the Azure DevOps Server conformance tests run: our own Server 2022 lab VM, a test project on the pilot's Server, or both? | Nightly tests against the real platform; acceptance before releases | Both: our lab VM (licensing to check) for nightly runs, a test project on the pilot's Server for acceptance | Phase 1 start |
| 2 | Pilot Server details: which collection(s) hold the IaC repositories, is the `/tfs` virtual directory in use, who can grant the service account its branch-folder permissions (`tf git permission`) and create the service hook subscription, and what maximum PAT lifetime does the Server allow? | URL parsing, registration wizard, rotation reminders | One collection, `/tfs` in use; a project collection administrator does the setup once; PAT lifetime 1 year | Phase 1 start |
| 3 | Is Quai's approval the only authority, or must Azure DevOps required-reviewer policies also pass? | Auto-complete vs waiting for both; approver kinds | Quai is the sole authority ([ADR 0002](../adr/0002-quai-is-the-approval-authority.md)); Azure DevOps path-based reviewers feed Quai's rules through the `codeowners` approver kind | Phase 1 |
| 4 | Is there an ITSM tool (ServiceNow, Jira Service Management) that must hold change records? | Phase 4 scope | ServiceNow in Phase 4 | Phase 4 |
| 5 | Product name. "Quai" stays the working name in code; choose the final name before beta. Candidates from earlier discussion: Talus, Guichet, Comptoir, Cairn, Arête, Mandat, Requisite, Ledger. Each needs a domain check (.com, .ch, .io) and a trademark search (Swissreg, EUIPO, class 42). | Branding, Go module path, binary and Helm chart names: cheap to rename before M2, costly after | "Quai" until decided | M2 |

## Decided

| Question | Decision | Date | Consequences |
| --- | --- | --- | --- |
| Git platform and edition | **Azure DevOps Server first, Services alongside or right after** | 2026-10-26 | [ADR 0010](../adr/0010-git-connectors-azure-devops-first.md), [Git connectors](../design/12-git-connectors.md) |
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
