# Phase 0: how autonomous sessions work

Status: Active · 2026-10-27

The tasks for [Phase 0](roadmap.md#phase-0-foundations-and-port-6-weeks) are GitHub issues
**#1–#29** in milestone [M1 Pilot](https://github.com/damhau/quai/milestones), labelled `phase:0`
and `type:task`. Task `P0-NN` is issue `#NN`. GitHub is the only task list: descriptions and done
criteria live in the issues. This file describes how a loop iteration works. Read
[`CLAUDE.md`](../../CLAUDE.md) first.

## One iteration

1. `git pull` on `main`.
2. Find the next task: the lowest-numbered open issue labelled `phase:0`:

   ```bash
   gh issue list --repo damhau/quai --label phase:0 --state open \
     --json number,title,labels --jq 'sort_by(.number) | .[0]'
   gh issue view <number> --repo damhau/quai
   ```

3. If it is labelled `stop`, follow [STOP tasks](#stop-tasks) below.
4. Read the documents the issue links to. They are the specification.
5. Implement it, with tests, in `backend/` or `frontend/`.
6. Run the quality gates ([backend](../engineering/backend.md#quality-gates), [frontend](../engineering/frontend.md#quality-gates)). All must pass.
7. Commit to `main` as `P0-NN: <summary> (closes #NN)`, with no attribution trailers, and push. The push closes the issue.
8. Check CI for the pushed commit (`gh run list --branch main --limit 1`, then `gh run watch <id>`). If CI is red, fixing it comes before the next task.

## STOP tasks

Issues labelled `stop` are checkpoints where the owner decides.

- **#4 (spike go/no-go) and #28 (parity review):** do the work the issue describes, commit with `refs #NN` (not `closes`), push, comment on the issue that it is ready for the owner's decision, and **stop the loop**. If the comment is already there, stop immediately. Only the owner closes these issues; closing one means "continue".
- **#13 (Docker check):** run `docker info` and pull a small image. If both work, close the issue with a comment and continue. Otherwise comment what is missing and stop.

## When to stop and ask

Stop the loop, and comment on the current issue explaining why, when:

- the next issue is a STOP task (see above);
- a decision is needed that the docs do not settle (also add it to [open questions](open-questions.md));
- the same gate fails three attempts in a row;
- an action is blocked by policy (Veto) or the sandbox, with no legitimate alternative;
- no Git identity is configured.

If the implementation has to deviate from a design document, update the document in the same
commit and say so in the commit message.
