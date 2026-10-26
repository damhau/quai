# Open questions

Status: Draft · 2026-10-26

Decisions needed, with the default the plan assumes until they are answered.

| # | Question | Why it matters | Default assumed | Needed by |
| --- | --- | --- | --- | --- |
| 1 | ~~Which Git platform does the pilot use?~~ **Azure DevOps** (decided, [ADR 0010](../adr/0010-git-connectors-azure-devops-first.md)). Still open: **Services (cloud) or Server (on-premises), and which Server version?** | API version, authentication (Entra vs PAT), conformance test environment | Azure DevOps Services | Phase 1 start |
| 2 | Does the pilot plan and apply Terraform in Azure Pipelines (YAML pipelines, build validation on PRs)? | First run adapter; whether plan JSON is available | Azure Pipelines with build validation, where a `quai report` step can be added | Phase 2 start |
| 2a | Can Quai use an Entra service principal with workload identity federation, or only a PAT? Who can grant it branch-folder permissions and create service hook subscriptions? | Credential setup, registration wizard | Service principal; a project admin creates the service hook once | Phase 1 start |
| 2b | Can we get a sandbox Azure DevOps project (and a Server test instance if they run Server) for conformance tests? | Nightly tests against the real platform | A sandbox Services organization owned by us | Phase 1 start |
| 3 | Which IdP (Entra ID, Keycloak, Okta, ADFS)? Are groups in the token, or is SCIM needed? | Team mapping, manager attribute, step-up | Entra ID with group claims (consistent with Azure DevOps) | Phase 0 |
| 4 | Is Quai's approval the only authority, or must platform reviews (Azure DevOps required-reviewer policies, CODEOWNERS elsewhere) also be required? | `merge: auto` vs `manual`, approver kinds | Quai is the authority ([ADR 0002](../adr/0002-quai-is-the-approval-authority.md)) | Phase 1 |
| 5 | Is there an ITSM (ServiceNow?) that must hold change records? | Phase 4 scope | ServiceNow, phase 4 | Phase 4 |
| 6 | Where does Quai run: Kubernetes, OpenShift, VMs? Air-gapped? | Packaging priorities | OpenShift, with restricted egress | Phase 0 |
| 7 | Secret store: Vault, Azure Key Vault, CyberArk, Kubernetes Secrets only? | `SecretStore` backends | Azure Key Vault (likely with Azure DevOps and Entra) and Kubernetes Secrets | Phase 1 |
| 8 | Which resource types does the pilot want first (VMs, namespaces and quotas, DNS, firewall rules, buckets)? | Order of locators and generators | VMs (file), then namespace quotas (entry) | Phase 1 |
| 9 | Product model: single-tenant self-hosted only, or also a managed offering? Open source, open core or proprietary? | Multi-organization support, licensing features, telemetry | Single-tenant self-hosted, proprietary | M2 |
| 10 | Are LLM-assisted onboarding features acceptable for target customers, and with which model endpoints? | Phase 7 design | Opt-in, customer-chosen endpoint | Phase 7 |
| 11 | Required languages for the UI? | i18n scope | English, French, German | Phase 8 |
| 12 | Audit retention period and any legal hold requirements? | Storage sizing, pseudonymization design | 10 years | Phase 4 |
| 13 | Do customers keep resource data in HCL (`.tfvars`, `locals`) in practice? | Priority of the HCL editor | Some do; Phase 3 | Phase 3 |
| 14 | Name and trademark: are `quai` domains and the trademark (Swissreg, EUIPO, class 42) available? | Branding before beta | Unverified | M2 |
