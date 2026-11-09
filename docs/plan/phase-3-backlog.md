# Phase 3: how autonomous sessions work

Status: In review · 2026-11-09 (P3-19 waits on the owner; Phase 4: [Phase 4 backlog](phase-4-backlog.md))

The tasks for [Phase 3](roadmap.md#phase-3-generators-and-locators-5-weeks) are GitHub issues in
milestone [M2 Beta](https://github.com/damhau/quai/milestones) labelled `phase:3` and
`type:task`, titled `P3-NN: …`. `P3-NN` is issue `#(114+NN)`, and their order is their issue
number. Each names the epic it belongs to (#46–#53), and each epic lists its tasks. GitHub is the
only task list: descriptions and done criteria live in the issues. Read
[`CLAUDE.md`](../../CLAUDE.md) first. An iteration works as in
[Phase 0](phase-0-backlog.md#one-iteration), with `phase:3` instead of `phase:0`:

```bash
gh issue list --repo damhau/quai --label phase:3 --label type:task --state open \
  --json number,title,labels --jq 'sort_by(.number) | .[0]'
```

Commit as `P3-NN: <summary> (closes #<issue>)`. A task may take a few commits; the last one
closes it.

Tests run in [three tiers](../engineering/testing.md#tiers), and an iteration waits only for
the first:
- **Before taking a task,** read the previous push's CI (tier 2) and the last nightly runs
  (tier 3: `e2e.yml`, `kind.yml`, `packaging.yml`, `live.yml`). A red one is fixed first, in a
  commit of its own (`Fix: <summary> (<workflow>)`).
- **Before committing,** run tier 1: `make quick` in `backend/`, and the frontend's checks when
  `frontend/` changed. Run the Playwright scenarios locally only when the task changes them or
  the request flows they cover, and the live tests only when it changes a connector or the runs.
  A task that changes `e2e/` dispatches the workflow (`gh workflow run e2e.yml`) and waits for
  it before closing.
- **After committing,** push and take the next task without waiting for CI.
- **The Phase 3 review** (P3-21) is written once tiers 2 and 3 are green on the same commit: it
  dispatches the nightly workflows and waits for them.

The order follows the dependencies:
- **The editors:** JSON, HCL, then formats in the pipeline (P3-01 to P3-03).
- **The shared-file locators** (P3-04 to P3-07).
- **The mapping generator, and `quai lint` for every locator and generator** (P3-08, P3-09).
- **Components and plugins** (P3-10 to P3-13).
- **Schema evolution** (P3-14).
- **The GitHub connector**, after its STOP task (P3-15 to P3-18).
- **The pilot's types, the end-to-end scenarios and the review** (P3-19 to P3-21).

The engine work comes first, so the owner has time to set up the GitHub sandbox.

## Test infrastructure

Everything of [Phase 2](phase-2-backlog.md#test-infrastructure) stays: embedded PostgreSQL,
Gitea and the Kubernetes API from their binaries, kind for the chart, recorded Terraform plans,
the compose stack's fake runner, the Azure DevOps sandbox and its pipelines, and the stand-in
pilot repository `pilot-infra`. Phase 3 adds:

| What | How |
| --- | --- |
| Editor corpora | Hand-written files in each editor's `testdata`: JSON and `*.tfvars.json` indented and minified, HCL `*.tfvars` with comments, maps of objects and `locals` blocks mixing expressions and literals. They grow with every bug, as the YAML corpus does. There is no customer data |
| Kubernetes CRDs | The CRDs the components render for (Crossplane's XRD claims, Argo CD's `Application`, Flux's `Kustomization`), vendored from their upstream releases, pinned, with a script that refreshes them (`backend/internal/k8stest/crds/refresh.sh`). They are installed in the envtest API server, which checks rendered manifests in server-side dry runs. A claim's CRD is not in any release: Crossplane generates it from its XRD at run time, so the test makes it from the example's XRD, which is itself checked against Crossplane's vendored XRD CRD |
| WebAssembly plugins | Built by the tests from Go sources with `GOOS=wasip1 GOARCH=wasm` (the Go toolchain, no TinyGo), and run with `wazero`. Nothing is downloaded |
| GitHub | The owner's throwaway organization `damhau-quai-sandbox` on github.com (free plan, with the team `quai-test-owners` for `CODEOWNERS`) and the GitHub App `quai-sandbox-damhau` installed on all its repositories (P3-15): app ID 5656119, installation ID 179566659. Its permissions: administration, checks, contents, merge queues, pull requests and commit statuses (write), members and metadata (read); without `workflows`, it cannot commit `.github/workflows/`. Its events: branch protection rule, check run, check suite, merge group, pull request, pull request review, push, repository ruleset, status. `internal/githubtest` reads the sandbox from `QUAI_TEST_GITHUB_ORG`, `QUAI_TEST_GITHUB_APP_ID`, `QUAI_TEST_GITHUB_INSTALLATION_ID`, `QUAI_TEST_GITHUB_APP_KEY_FILE` (the private key, locally `~/.config/quai/github-app.pem`) and `QUAI_TEST_GITHUB_WEBHOOK_SECRET_FILE` (the webhook's secret, locally `~/.config/quai/github-webhook-secret`); nightly (`live.yml`), those two files are written from the repository secrets `QUAI_TEST_GITHUB_APP_KEY` and `QUAI_TEST_GITHUB_WEBHOOK_SECRET`. The conformance suite's step 4 needs a second app installed on the organization, which tries to publish `quai/approval` where only the Quai app may: `QUAI_TEST_GITHUB_OTHER_APP_KEY_FILE` (locally `~/.config/quai/github-other-app.pem`, nightly written from the secret `QUAI_TEST_GITHUB_OTHER_APP_KEY`), `QUAI_TEST_GITHUB_OTHER_APP_ID` and `QUAI_TEST_GITHUB_OTHER_INSTALLATION_ID` (placeholders in `live.yml`). It does not exist yet, so step 4 skips. The Azure DevOps sandbox's rules apply: the key and the secret are never printed, logged, passed on a command line or committed; tests create `quai-test-` repositories, public because merge queues need it, and delete them; the tests are live and skipped without the key. GitHub cannot reach a test machine: the app's webhook points at `https://example.com/quai-webhook`, which GitHub cannot connect to (it records each delivery as "failed to connect to host", 502), and webhooks are replayed from the app's delivery history, whose payloads are byte for byte what GitHub signed, so a replay carries GitHub's own `X-Hub-Signature-256`. Hosts: `api.github.com`, `github.com` |

## STOP tasks

- **P3-15** (the GitHub sandbox) needs what only the owner can provide (named in the issue). If
  the issue has no comment from the owner saying it is in place, comment what is missing and
  **stop**. When it is there, implement the task and close it like any other.
- **P3-19** (the pilot's types) works on the stand-in `pilot-infra` with synthetic data: there is
  no pilot repository (the owner's decision of 2026-10-30, in
  [open questions](open-questions.md#decided)). Its pull requests are completed in order, as in
  P2-13. They are open, ready, on `pilot-infra`, each on the one before: !407 (the pipelines plan
  and apply every stack under `stacks/`, and lint with the image of `0a2e052`), !408 (namespaces
  and quotas), !409 (DNS records), !410 (firewall rules). Their build validation runs, 173 to 176,
  passed: on !410, `quai lint` (5 types, 30 resources, no problems) and `quai lint --plan` on each
  of the eight stacks' plans. They wait on [open question 16](open-questions.md#still-open), as
  `quai/approval` binds every pull request since P2-13. The types and data are copied in
  `examples/pilot-infra`, which `TestPilotTypes` and `TestPilotConcurrentEntries` request on
  Gitea.
- **P3-21** (Phase 3 review): write the report the issue describes, commit with `refs`, comment
  that it is ready, and go on to Phase 4 ([the owner's decision of 2026-11-07](open-questions.md#decided)).
  Only the owner closes it.

The rules of [when to stop and ask](phase-0-backlog.md#when-to-stop-and-ask) apply.
The GitHub tasks target github.com only; GitHub Enterprise Server is a future improvement
([#137](https://github.com/damhau/quai/issues/137), [open question 11](open-questions.md#decided)).

## Phase 3 review (P3-21)

2026-11-09, with P3-19 still open (item 1 below). The
[Phase 3 exit](roadmap.md#phase-3-generators-and-locators-5-weeks) against the tests that show it:
- **On Gitea, on every push:** the backend tests, including the pilot's types and data copied in
  `examples/pilot-infra`, and the plugins' tests (`plugins.yml`).
- **On the compose stack, nightly:** the Playwright scenarios, with shared files since P3-20
  (`e2e/tests/shared-files.spec.ts`).
- **On the Azure DevOps Services and GitHub sandboxes, nightly:** the live tests (`live.yml`),
  both jobs with their secrets.
- **On the stand-in `pilot-infra`:** P3-19's pull requests !407 to !410 and their build
  validation. There is no pilot repository ([decided](open-questions.md#decided), 2026-10-30).

Tiers 2 and 3 were green on `ece1b9b` (2026-11-09, about 20:40 UTC): on the push, `backend`
(run 42692815270), `frontend` (42692815461), `packaging` (42692815464) and `plugins`
(42692815253); dispatched, `kind` (42692812971), `e2e` (42692818394) and `live` (42692825265),
whose GitHub job passed 183 tests and subtests and skipped one,
`TestLiveConformance/4-status-identity` (item 4), and whose Azure DevOps job passed. The commits
after it change CI and docs only.

**The exit, part by part.**

| Exit | Gitea | Azure DevOps Services |
| --- | --- | --- |
| The pilot's namespace-and-quota, DNS record and firewall rule types, those backed by HCL included, pass round-trip lint on the pilot's repository | `TestPilotExamplePass`: `quai lint` on `examples/pilot-infra` (4 types, 25 resources, no problems): namespaces on a YAML list per cluster with their quota as a field, DNS records on an HCL `*.tfvars` map, firewall rules on a `*.tfvars.json` list | On `pilot-infra`'s pipeline: build validation runs 173 to 176 of !407 to !410 passed; on !410, `quai lint` (5 types, 30 resources, no problems) and `quai lint --plan` on each of the eight stacks' plans. On the pull requests' branches only: `main` gets them with item 1 |
| They are live in their Quai | `TestPilotTypes`: each type requested through a pull request and merged once the platform, network or security team approves | Waits for item 1. The M2 rehearsal on `pilot-infra` (P4-22) used the `vm` type only ([Phase 4 backlog](phase-4-backlog.md#the-m2-rehearsal-p4-22)) |
| Concurrent requests on two entries of one file do not conflict | `TestPilotConcurrentEntries`: in each of the pilot's three formats, two creates approved before either merges both merge, the second rebuilt on the first with its approval; `TestSharedFiles` on the example repository; e2e `shared-files`, two record sets of one HCL file approved together from two browsers | `TestAzureDevOpsSharedFiles`: `TestSharedFiles`'s flow on the sandbox, behind `quai/approval` and auto-complete. On `pilot-infra`, waits for item 1 |

**The GitHub connector** (epic #52), on github.com only, live on `damhau-quai-sandbox`:
- **The conformance suite:** `TestLiveConformance`, every step but step 4 (item 4).
- **Authentication, pull requests, merges and forks:** `TestLiveAuthentication`,
  `TestLiveAuthenticationPrivate`, `TestLivePullRequests`, `TestLiveMerge`, `TestLiveForks`.
- **Check runs, rulesets and branch protection pinned to the app, `CODEOWNERS` and trust:**
  `TestLiveRuleset`, `TestLiveBranchProtection`, `TestLiveOwners`, `TestGitHubEnforcement`,
  `TestGitHubAccess`, `TestGitHubTrust`.
- **Auto-merge, the merge queue and webhooks** replayed from the app's delivery history:
  `TestLiveAutoMerge`, `TestLiveMergeQueue`, `TestLiveWebhooks`, `TestDeliveries`,
  `TestGitHubReplay`, and a request delivered through both in `TestGitHubDelivery`.

Their hermetic counterparts (the connector's answers, signatures and events,
`TestMergeQueueDelivery`) run on every push.

**Also delivered**, tested on every push:
- **Editors** for JSON and HCL beside YAML, with their corpora (`TestCorpus`) and fuzz targets
  (item 6), and each file's format chosen in the pipeline (`TestEachFormat`).
- **Locators and generators:** `entry` on YAML, JSON and HCL collections
  (`TestEntryRequestsConcurrently`, `TestTfvarsMapRequests`), `field` (`TestFieldRequests`), the
  mapping generator with read-back (`TestMappingRequests`, `TestMappingRead`), `dir` regions and
  the `component` generator with Crossplane claims, Argo CD applications and Flux kustomizations,
  their manifests accepted by envtest in server-side dry runs (`TestComponentsDryRun`,
  `TestKubernetesExampleDryRun`).
- **`quai lint`** for every locator and generator, with golden examples (`TestGoldenExamples`).
- **Plugins:** the WebAssembly runtime, its sandbox and pinned digests (`TestPlugins`,
  `TestRefused`), and the SDK, template and INI example in `plugins/` (`TestINIPlugin`,
  `make -C plugins check`).
- **Schema evolution:** migrations, blueprint pins, deletion policies (`TestSchemaEvolution`,
  `TestMigrations`, `TestPins`, `TestDeletionPolicy`).

**Moved out of Phase 3.**
- **#136**, the KubeVirt VM component: left P3-10 ([open question 12](open-questions.md#decided),
  2026-10-31), a [future improvement](roadmap.md#future-improvements). VMs declared as entries of
  a chart's values are covered by the `entry` locator.
- **#137**, GitHub Enterprise Server: a future improvement
  ([open question 11](open-questions.md#decided), 2026-10-31).
- **Quai reviewing pull requests it did not open**, the alternative to open question 16's
  default: in no phase. It is not a Phase 4 task, as no design document specifies it
  ([Phase 4 backlog](phase-4-backlog.md#stop-tasks)); it becomes one if the owner decides so.

**Not done, for the owner to decide or schedule, and what the review found.**

1. **P3-19 on `pilot-infra`** ([open question 16](open-questions.md#still-open)). !407 to !410
   passed their build validation and are still active. `quai/approval` binds every pull request
   on `pilot-infra` and Quai posts it only on its own, so only an owner's policy bypass completes
   them, in order. Then the types are on `main`, and each can be requested through Quai there,
   two on one file included. Until then #133 and the epic #53 stay open. Their Gitea tests pass.
2. **The GitHub sandbox's webhook.** The app's webhook points at `example.com`, which GitHub
   cannot connect to: it records each delivery as "failed to connect to host", 502. On
   2026-11-09, from about 01:00 to 10:30 UTC, GitHub recorded deliveries late, by up to 49
   minutes, and `TestDeliveries`, `TestLiveMergeQueue` and `TestLiveWebhooks`, which need every
   delivery of their scenario, failed. A webhook URL GitHub can reach is the owner's fix; the host
   behind it then receives the sandbox's events. Until then the three fail whenever the delay
   comes back.
3. **Leftover sandbox repositories**, for the owner to delete: on GitHub `quai-test-fb683e3d`
   and `quai-test-31c3b3d3` (2026-11-08, their cleanup refused once the installation's rate limit
   was spent), on Azure DevOps `quai-test-24012989` (2026-11-09, its cleanup timed out during a
   service maintenance window).
4. **Conformance step 4 on GitHub.** `TestLiveConformance/4-status-identity` skips, the GitHub
   job's only skip: it needs a second GitHub App or identity on the organization, trying to
   publish `quai/approval` where only the Quai app may. The owner may create it
   ([test infrastructure](#test-infrastructure)).
5. **Defaults to confirm** ([open questions](open-questions.md#still-open), recorded in P3-16 to
   P3-19):
   - **13:** the GitHub App's ID in `spec.credentials.appId`, the installation found from it.
   - **14:** definitions trusted on GitHub only with both `CODEOWNERS` covering `.quai/` and a
     rule requiring code owners' review.
   - **15:** a repository webhook per connection, not the app's own webhook.
   - **16:** people's own pull requests completed with the platform owners' policy bypass.
6. **Fuzzing in CI: found in this review, fixed in 3671bba.** [Testing](../engineering/testing.md)
   and [backend](../engineering/backend.md#quality-gates) asked for 60 s per editor fuzz target
   on every push and an hour nightly, but no workflow ran `go test -fuzz`: each push ran the
   targets on their seed corpora only. `backend.yml` now has a fuzz job per editor (YAML, JSON,
   HCL), 60 s each, and `fuzz.yml` runs each for an hour nightly. Before, P3-01 and P3-02 fuzzed
   locally: `FuzzJSONEdit` 9.7 million executions clean in 60 s, `FuzzHCLEdit` 4.4 million after
   the two inputs it found were kept as regressions.
7. **Edges seen in the live runs.**
   - **The repository lock.** Since 2add902, a job that finds a repository's lock taken tries
     again 5 s later, and back-to-back holders can starve it: in `TestGitHubDelivery/auto-merge`
     the test's polling held the lock about 1.1 s in every 1.4 s, and the pull request job did
     not get it in 60 s. Fixed in the test (4b6c1a2: polling at most every 2 s). Quai itself polls
     at intervals, so the product is unchanged; a burst of webhooks on a busy repository could do
     the same, and is worth watching.
   - **GitHub's own failures on 2026-11-09**, each retried in the live tests for that answer
     only: a new fork refusing a new branch (422 "Reference update failed", 5d377a6); a new
     repository refusing the first push ("Permission … denied", e141315 and 9a30a20; "RPC failed;
     HTTP 403", 4e87b15) or answering a clone with 404 (16a5451); a check run or status
     read back as it was before it was set (3359cdf, 7332cfd); a merge queue ask answered 502, which delivery's
     job already retries (ece1b9b). For the webhook delay (item 2), the tests wait up to four
     minutes for the deliveries (a849a37).
8. **The epics.** #46 to #52 are closed; #53 stays open with #133 and this issue.
