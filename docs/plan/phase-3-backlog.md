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

The Playwright scenarios and the live tests run every night, not on each push (`e2e.yml`,
`live.yml`). So:
- **Before taking a task,** read the last nightly runs (`gh run list --workflow e2e.yml` and
  `--workflow live.yml`). A red run is fixed first, in a commit of its own
  (`Fix: <summary> (nightly <workflow>)`).
- **A task's local gates** are `make check` and the frontend's checks. The Playwright scenarios
  run locally only when the task changes them or the request flows they cover; the live tests
  only when it changes a connector or the runs. A task that changes `e2e/` dispatches the
  workflow (`gh workflow run e2e.yml`) and waits for it before closing.

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
| Kubernetes CRDs | The CRDs the components render for (Crossplane's XRD claims, Argo CD's `Application`, Flux's `Kustomization`, KubeVirt's `VirtualMachine`), vendored from their upstream releases, pinned, with a script that refreshes them. They are installed in the envtest API server, which checks rendered manifests in server-side dry runs |
| WebAssembly plugins | Built by the tests from Go sources with `GOOS=wasip1 GOARCH=wasm` (the Go toolchain, no TinyGo), and run with `wazero`. Nothing is downloaded |
| GitHub | The owner's throwaway organization on github.com and a GitHub App installed on it (P3-15): the app ID, the installation ID, and the private key in `~/.config/quai/github-app.pem`, named by `QUAI_TEST_GITHUB_APP_KEY_FILE`. The Azure DevOps sandbox's rules apply: the key is never printed, logged, passed on a command line or committed; tests create `quai-test-` repositories, public because merge queues need it, and delete them; the tests are live and skipped without the key; GitHub cannot reach a test machine, so webhooks are replayed from the app's delivery history. Hosts: `api.github.com`, `github.com` |

## STOP tasks

- **P3-15** (the GitHub sandbox) needs what only the owner can provide (named in the issue). If
  the issue has no comment from the owner saying it is in place, comment what is missing and
  **stop**. When it is there, implement the task and close it like any other.
- **P3-19** (the pilot's types) works on the stand-in `pilot-infra` with synthetic data: there is
  no pilot repository (the owner's decision of 2026-10-30, in
  [open questions](open-questions.md#decided)). Its pull requests are completed in order, as in
  P2-13.
- **P3-21** (Phase 3 review): write the report the issue describes, commit with `refs`, comment
  that it is ready, and **stop**. Only the owner closes it.

The rules of [when to stop and ask](phase-0-backlog.md#when-to-stop-and-ask) apply unchanged.
Open question 11 ([open questions](open-questions.md)) has a default the GitHub tasks follow
until the owner answers it.
