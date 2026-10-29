# Phase 2: how autonomous sessions work

Status: Active · 2026-10-29

The tasks for [Phase 2](roadmap.md#phase-2-runs-and-status-4-weeks) are GitHub issues in
milestone [M1 Pilot](https://github.com/damhau/quai/milestones) labelled `phase:2` and
`type:task`, titled `P2-NN: …`. Their order is their issue number. Each names the epic it belongs
to (#41–#45). GitHub is the only task list: descriptions and done criteria live in the issues.
Read [`CLAUDE.md`](../../CLAUDE.md) first; an iteration works as in
[Phase 0](phase-0-backlog.md#one-iteration), with `phase:2` instead of `phase:0`:

```bash
gh issue list --repo damhau/quai --label phase:2 --label type:task --state open \
  --json number,title,labels --jq 'sort_by(.number) | .[0]'
```

Commit as `P2-NN: <summary> (closes #<issue>)`; a task may take a few commits, the last one closes
it.

## Test infrastructure

Everything of [Phase 1](phase-1-backlog.md#test-infrastructure) stays: embedded PostgreSQL, Gitea
and the Kubernetes API from their binaries, kind for the chart, and the owner's Azure DevOps
sandbox with its rules for the PAT and for cleaning up. Phase 2 adds:

| What | How |
| --- | --- |
| Terraform plans | Recorded `terraform show -json` outputs in `testdata`, produced from small root modules that use the built-in `terraform_data` resource (no provider downloads), with the script that regenerates them beside them. Tests never run Terraform |
| A runner in the compose stack | A fake runner (P2-12): on Gitea's pull request and push webhooks, it posts plan and apply reports with `quai report` and a recorded plan |
| Azure Pipelines | YAML pipelines in `quai-test-` repositories of the sandbox, created and deleted by the tests (P2-08). The sandbox cannot reach a test machine: what a pipeline would post to Quai is tested on Gitea, and its run state is read back from the service hook subscription's history, as for webhooks in Phase 1 |

## STOP tasks

- **P2-08** (pipelines on the sandbox) and **P2-13** (pilot onboarding) need something only the
  owner can provide (named in the issue). If the issue has no comment from the owner saying it is
  in place, comment what is missing and **stop**. When it is there, implement the task and close
  it like any other.
- **P2-14** (Phase 2 review): write the report the issue describes, commit with `refs`, comment
  that it is ready, and **stop**. Only the owner closes it.

The rules of [when to stop and ask](phase-0-backlog.md#when-to-stop-and-ask) apply unchanged.
Open questions 7 and 8 ([open questions](open-questions.md)) have defaults the tasks follow until
the owner answers them.
