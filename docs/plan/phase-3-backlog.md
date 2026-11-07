# Phase 3: how autonomous sessions work

Status: Active · 2026-10-30

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
| GitHub | The owner's throwaway organization `damhau-quai-sandbox` on github.com (free plan, with the team `quai-test-owners` for `CODEOWNERS`) and the GitHub App `quai-sandbox-damhau` installed on all its repositories (P3-15): app ID 5656119, installation ID 179566659. Its permissions: administration, checks, contents, merge queues, pull requests and commit statuses (write), members and metadata (read); without `workflows`, it cannot commit `.github/workflows/`. Its events: branch protection rule, check run, check suite, merge group, pull request, pull request review, push, repository ruleset, status. `internal/githubtest` reads the sandbox from `QUAI_TEST_GITHUB_ORG`, `QUAI_TEST_GITHUB_APP_ID`, `QUAI_TEST_GITHUB_INSTALLATION_ID`, `QUAI_TEST_GITHUB_APP_KEY_FILE` (the private key, locally `~/.config/quai/github-app.pem`) and `QUAI_TEST_GITHUB_WEBHOOK_SECRET_FILE` (the webhook's secret, locally `~/.config/quai/github-webhook-secret`); nightly (`live.yml`), those two files are written from the repository secrets `QUAI_TEST_GITHUB_APP_KEY` and `QUAI_TEST_GITHUB_WEBHOOK_SECRET`. The Azure DevOps sandbox's rules apply: the key and the secret are never printed, logged, passed on a command line or committed; tests create `quai-test-` repositories, public because merge queues need it, and delete them; the tests are live and skipped without the key. GitHub cannot reach a test machine: the app's webhook points at an address that answers with an error (`https://example.com/quai-webhook`), and webhooks are replayed from the app's delivery history, whose payloads are byte for byte what GitHub signed, so a replay carries GitHub's own `X-Hub-Signature-256`. Hosts: `api.github.com`, `github.com` |

## STOP tasks

- **P3-15** (the GitHub sandbox) needs what only the owner can provide (named in the issue). If
  the issue has no comment from the owner saying it is in place, comment what is missing and
  **stop**. When it is there, implement the task and close it like any other.
- **P3-19** (the pilot's types) works on the stand-in `pilot-infra` with synthetic data: there is
  no pilot repository (the owner's decision of 2026-10-30, in
  [open questions](open-questions.md#decided)). Its pull requests are completed in order, as in
  P2-13.
- **P3-21** (Phase 3 review): write the report the issue describes, commit with `refs`, comment
  that it is ready, and go on to Phase 4 ([the owner's decision of 2026-11-07](open-questions.md#decided)).
  Only the owner closes it.

The rules of [when to stop and ask](phase-0-backlog.md#when-to-stop-and-ask) apply.
The GitHub tasks target github.com only; GitHub Enterprise Server is a future improvement
([#137](https://github.com/damhau/quai/issues/137), [open question 11](open-questions.md#decided)).
