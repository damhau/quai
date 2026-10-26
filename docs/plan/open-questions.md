# Open questions

Status: Draft · 2026-10-26

Decisions needed, with the default the plan assumes until they are answered.

| # | Question | Why it matters | Default assumed | Needed by |
| --- | --- | --- | --- | --- |
| 1 | ~~Which Git platform, and which edition first?~~ **Azure DevOps Server (on-premises) first, Services alongside or right after** (decided, [ADR 0010](../adr/0010-git-connectors-azure-devops-first.md)). Still open: **which Server version and update** does the pilot run, with which collection(s), and is IIS configured with the `/tfs` virtual directory? | API version baseline, feature availability, URL parsing | Azure DevOps Server 2022 with its latest update | Phase 1 start |
| 2 | Does the pilot plan and apply Terraform in Azure Pipelines (YAML pipelines, build validation on PRs)? | First run adapter; whether plan JSON is available | Azure Pipelines with build validation, where a `quai report` step can be added | Phase 2 start |
| 2a | On Server: can we get a dedicated AD service account with a PAT (and what maximum PAT lifetime does the Server allow)? Who can grant it branch-folder permissions (`tf git permission`) and create service hook subscriptions? On Services later: an Entra service principal with workload identity federation? | Credential setup, registration wizard | PAT of a service account on Server; a project collection admin sets permissions and the service hook once | Phase 1 start |
| 2b | Can we get a test project on the pilot's Server for conformance tests, or should we run our own Server 2022 VM (licensing)? Plus a Services sandbox organization. | Nightly tests against the real platforms | Our own Server 2022 VM, plus a test project on the pilot's Server for acceptance; a Services sandbox organization owned by us | Phase 1 start |
| 2c | Can the pilot's Server reach Quai's webhook endpoint (firewall), and does it use an internal CA for TLS? | Service hooks vs polling mode; CA bundle configuration | Internal CA; service hooks allowed, polling as fallback | Phase 1 |
| 3 | Which IdP for Quai's own login (Entra ID, ADFS, Keycloak, Okta)? Are groups in the token, or is SCIM needed? With Azure DevOps Server on-premises, Active Directory through ADFS or Entra ID (hybrid) are both likely. | Team mapping, manager attribute, step-up | Entra ID (hybrid) with group claims; ADFS supported through OIDC | Phase 0 |
| 4 | Is Quai's approval the only authority, or must platform reviews (Azure DevOps required-reviewer policies, CODEOWNERS elsewhere) also be required? | `merge: auto` vs `manual`, approver kinds | Quai is the authority ([ADR 0002](../adr/0002-quai-is-the-approval-authority.md)) | Phase 1 |
| 5 | Is there an ITSM (ServiceNow?) that must hold change records? | Phase 4 scope | ServiceNow, phase 4 | Phase 4 |
| 6 | Where does Quai run: Kubernetes, OpenShift, VMs? Air-gapped? | Packaging priorities | OpenShift, with restricted egress | Phase 0 |
| 7 | Secret store: Vault, CyberArk, Azure Key Vault, Kubernetes Secrets only? | `SecretStore` backends | Kubernetes Secrets and Vault for on-premises; Azure Key Vault when Services customers come | Phase 1 |
| 8 | Which resource types does the pilot want first (VMs, namespaces and quotas, DNS, firewall rules, buckets)? | Order of locators and generators | VMs (file), then namespace quotas (entry) | Phase 1 |
| 9 | Product model: single-tenant self-hosted only, or also a managed offering? Open source, open core or proprietary? | Multi-organization support, licensing features, telemetry | Single-tenant self-hosted, proprietary | M2 |
| 10 | Are LLM-assisted onboarding features acceptable for target customers, and with which model endpoints? | Phase 7 design | Opt-in, customer-chosen endpoint | Phase 7 |
| 11 | Required languages for the UI? | i18n scope | English, French, German | Phase 8 |
| 12 | Audit retention period and any legal hold requirements? | Storage sizing, pseudonymization design | 10 years | Phase 4 |
| 13 | Do customers keep resource data in HCL (`.tfvars`, `locals`) in practice? | Priority of the HCL editor | Some do; Phase 3 | Phase 3 |
| 14 | Name and trademark: are `quai` domains and the trademark (Swissreg, EUIPO, class 42) available? | Branding before beta | Unverified | M2 |
