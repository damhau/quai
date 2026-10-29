import { screen, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import type { RequestView } from "@/api/generated/types.gen"
import { resources } from "@/test/inventory"
import { pendingVM } from "@/test/requests"
import { renderApp } from "@/test/render"
import { server, state } from "@/test/server"

const slow = { timeout: 5000 } // the first render of a page loads its chunk
const at = "2026-10-28T10:00:00Z"

function request(change: Partial<RequestView>) {
  server.use(http.get("*/api/v1/requests/:id", () => HttpResponse.json({ ...pendingVM(), ...change })))
}

test("the timeline follows the request from its pull request to the apply", async () => {
  request({
    state: "applied",
    events: [
      { kind: "submitted", actor: "bob", data: null, at },
      { kind: "pull_request_opened", data: { pullRequest: "412" }, at },
      {
        kind: "plan_reported",
        data: { stack: "envs/prod", status: "succeeded", url: "https://ci.example.com/88121", summary: { add: 1, change: 0, destroy: 0 }, cost: { currency: "CHF", monthlyDelta: 142.4 }, policies: { passed: 1, warning: 1 } },
        at,
      },
      { kind: "approve", actor: "alice", data: null, at },
      { kind: "merged", data: { commit: "7dc96c1e00", pullRequest: "412" }, at },
      { kind: "applied", data: { stack: "envs/prod", commit: "7dc96c1e00", url: "https://ci.example.com/88130" }, at },
    ],
  })
  renderApp(`/requests/${pendingVM().id}`)
  const timeline = await screen.findByRole("list", { name: "Timeline" }, slow)
  expect(within(timeline).getByText(/opened pull request #412/)).toBeInTheDocument()
  expect(within(timeline).getByText(/the plan of envs\/prod succeeded: \+1 ~0 −0 · CHF \+142.4\/month · policies: 1 warning/)).toBeInTheDocument()
  expect(within(timeline).getByText(/the pull request was merged as 7dc96c1/)).toBeInTheDocument()
  expect(within(timeline).getByText(/the apply of envs\/prod succeeded at 7dc96c1/)).toBeInTheDocument()
  expect(within(timeline).getAllByRole("link", { name: "run" }).map((a) => a.getAttribute("href"))).toEqual(["https://ci.example.com/88121", "https://ci.example.com/88130"])
})

test("a request waiting for its apply says so, and a failed apply offers no retry", async () => {
  request({ state: "applying", events: [{ kind: "committed", data: { commit: "7dc96c1e00" }, at }] })
  const { unmount } = renderApp(`/requests/${pendingVM().id}`)
  expect(await screen.findByText("Applying", {}, slow)).toBeInTheDocument()
  unmount()

  state.user = "bob"
  request({ state: "failed", canRetry: false, events: [{ kind: "apply_failed", data: { stack: "envs/prod", commit: "7dc96c1e00" }, at }] })
  renderApp(`/requests/${pendingVM().id}`)
  expect(await screen.findByText(/the apply of envs\/prod failed at 7dc96c1/, {}, slow)).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: /Retry/ })).not.toBeInTheDocument()
})

test("a resource's page lists the applies of its requests", async () => {
  state.user = "bob"
  server.use(
    http.get("*/api/v1/resource", () =>
      HttpResponse.json({
        ...resources[0],
        state: "applied",
        commit: "abc123",
        raw: "cpu: 4\n",
        actions: [],
        canOperate: false,
        requests: [],
        history: [],
        applies: [{ kind: "applied", request: "11111111-1111-4111-8111-111111111111", data: { stack: "envs/prod", commit: "7dc96c1e00" }, at }],
      }),
    ),
  )
  renderApp("/resources/infra-live/vm/prod/web01")
  const applies = await screen.findByRole("list", { name: "Applies" }, slow)
  expect(within(applies).getByRole("link", { name: /the apply of envs\/prod succeeded at 7dc96c1/ })).toHaveAttribute("href", "/requests/11111111-1111-4111-8111-111111111111")
})
