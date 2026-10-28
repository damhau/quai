import { screen } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { submitted } from "@/test/catalog"
import { renderApp } from "@/test/render"
import { server } from "@/test/server"

test("a request delivered by pull request links it, and its merge commit once merged", async () => {
  server.use(
    http.get("*/api/v1/requests/:id", ({ params }) =>
      HttpResponse.json({
        ...submitted,
        id: String(params.id),
        state: "applied",
        delivery: { branch: "quai/r-7-web01-create", pullRequest: "12", url: "https://git.example.com/quai/infra-live/pulls/12", mergeCommit: "4b825dc642cb6eb9a060e54bf8d69288fbee4904" },
      }),
    ),
  )
  renderApp(`/requests/${submitted.id}`)
  const link = await screen.findByRole("link", { name: "Pull request #12" })
  expect(link).toHaveAttribute("href", "https://git.example.com/quai/infra-live/pulls/12")
  expect(screen.getByText("quai/r-7-web01-create")).toBeInTheDocument()
  expect(screen.getByText("4b825dc642cb")).toBeInTheDocument()
})

test("while the pull request is being opened, the page says so", async () => {
  server.use(http.get("*/api/v1/requests/:id", ({ params }) => HttpResponse.json({ ...submitted, id: String(params.id), delivery: { branch: "quai/r-7-web01-create" } })))
  renderApp(`/requests/${submitted.id}`)
  expect(await screen.findByText("Opening the pull request")).toBeInTheDocument()
})

test("the timeline says what happened to the pull request", async () => {
  const at = "2026-10-28T10:00:00Z"
  server.use(
    http.get("*/api/v1/requests/:id", ({ params }) =>
      HttpResponse.json({
        ...submitted,
        id: String(params.id),
        state: "stale",
        delivery: { branch: "quai/r-7-web01-create", pullRequest: "12" },
        events: [
          { kind: "rebuilt", data: { base: "4b825dc642cb6eb9a060e54bf8d69288fbee4904" }, at },
          { kind: "modified_outside_quai", data: { reason: "the pull request was changed outside Quai: its approvals are void, approve the new content" }, at },
          { kind: "stale", data: { region: "envs/prod/vms/web01.yaml" }, at },
        ],
      }),
    ),
  )
  renderApp(`/requests/${submitted.id}`)
  expect(await screen.findByText(/rebuilt the pull request on the new base 4b825dc;/)).toBeInTheDocument()
  expect(screen.getByText(/changed outside Quai: its approvals are void/)).toBeInTheDocument()
  expect(screen.getByText(/envs\/prod\/vms\/web01.yaml changed on the base branch/)).toBeInTheDocument()
})
