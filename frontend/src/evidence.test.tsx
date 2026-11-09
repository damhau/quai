import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { archiveName, evidenceURL } from "@/lib/evidence"
import { renderApp } from "@/test/render"
import { evidence, sampled } from "@/test/evidence"
import { state } from "@/test/server"

const slow = { timeout: 5000 } // the first render of a page loads its chunk
const id = "11111111-1111-4111-8111-111111111111"

test("the request's page offers its evidence bundle to its requester, auditors and platform admins only", async () => {
  state.user = "bob"
  const { unmount } = renderApp(`/requests/${id}`)
  await screen.findByText("Capacity for the launch", undefined, slow)
  expect(screen.getByRole("link", { name: "JSON bundle" })).toHaveAttribute("href", `/api/v1/requests/${id}/evidence`)
  expect(screen.getByRole("link", { name: "PDF summary" })).toHaveAttribute("href", `/api/v1/requests/${id}/evidence?format=pdf`)
  expect(screen.getByRole("link", { name: "JSON bundle" })).toHaveAttribute("download")
  unmount()

  state.user = "dave"
  renderApp(`/requests/${id}`)
  await screen.findByText("Capacity for the launch", undefined, slow)
  expect(screen.queryByRole("link", { name: "JSON bundle" })).not.toBeInTheDocument()
  expect(screen.getByRole("navigation", { name: "Main" })).not.toHaveTextContent("Evidence")
})

test("an auditor samples a period's requests, filters them, and downloads a selection as one archive", async () => {
  state.user = "dana"
  const saved: string[] = []
  const created = vi.fn(() => "blob:archive")
  Object.assign(URL, { createObjectURL: created, revokeObjectURL: vi.fn() })
  const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    saved.push(`${this.download} ${this.href}`)
  })
  const { router } = renderApp("/admin/evidence")
  const table = await screen.findByRole("table", { name: "Requests" }, slow)
  expect(screen.getByRole("navigation", { name: "Main" })).toHaveTextContent("Evidence")
  // The last 30 days, all repositories.
  const q = evidence.queries.at(-1)!
  const days = (new Date(q.get("until")!).getTime() - new Date(q.get("since")!).getTime()) / 86_400_000
  expect(Math.round(days)).toBe(31)
  expect(q.get("repository")).toBeNull()
  expect(within(table).getAllByRole("row")).toHaveLength(4)
  expect(within(table).getByRole("link", { name: "JSON bundle of #3" })).toHaveAttribute("href", evidenceURL(sampled[2]!.id, "json"))
  expect(within(table).getByRole("link", { name: "PDF summary of #3" })).toHaveAttribute("href", `${evidenceURL(sampled[2]!.id, "json")}?format=pdf`)
  expect(within(table).getByText("Break-glass")).toBeInTheDocument()

  const download = screen.getByRole("button", { name: /Download 0 as an archive/ })
  expect(download).toBeDisabled()
  await userEvent.click(within(table).getByRole("checkbox", { name: "Select #1" }))
  await userEvent.click(within(table).getByRole("checkbox", { name: "Select #3" }))
  await userEvent.click(screen.getByRole("button", { name: "Download 2 as an archive" }))
  await waitFor(() => expect(evidence.archives).toHaveLength(1))
  expect(evidence.archives[0]).toEqual({ requests: [sampled[0]!.id, sampled[2]!.id], csrf: "csrf-dana" })
  await waitFor(() => expect(saved).toEqual(["quai-evidence-20261109T120000Z.zip blob:archive"]))
  expect(created).toHaveBeenCalledOnce()

  // The filters go to the address and the server; the selection starts again.
  const form = screen.getByRole("form", { name: "Sample" })
  await userEvent.selectOptions(within(form).getByLabelText("State"), "rejected")
  await userEvent.clear(within(form).getByLabelText("From"))
  await userEvent.type(within(form).getByLabelText("From"), "2026-11-01")
  await userEvent.click(within(form).getByRole("button", { name: "List" }))
  await waitFor(() => expect(router.state.location.search).toMatchObject({ state: "rejected", from: "2026-11-01" }))
  await waitFor(() => expect(evidence.queries.at(-1)?.get("state")).toBe("rejected"))
  expect(new Date(evidence.queries.at(-1)!.get("since")!).getTime()).toBe(new Date("2026-11-01T00:00").getTime())
  const rejected = await screen.findByRole("table", { name: "Requests" })
  await waitFor(() => expect(within(rejected).getAllByRole("row")).toHaveLength(2))
  expect(screen.getByRole("button", { name: /Download 0 as an archive/ })).toBeDisabled()

  // Select the first ones at once.
  await userEvent.click(within(rejected).getByRole("checkbox", { name: "Select the first 1" }))
  expect(screen.getByRole("button", { name: "Download 1 as an archive" })).toBeEnabled()
  click.mockRestore()
})

test("an auditor of one repository samples that repository only", async () => {
  state.user = "rita"
  renderApp("/admin/evidence")
  await screen.findByRole("table", { name: "Requests" }, slow)
  expect(evidence.queries.at(-1)?.get("repository")).toBe("infra-live")
  const repository = within(screen.getByRole("form", { name: "Sample" })).getByLabelText("Repository")
  expect(within(repository).queryByRole("option", { name: "All" })).not.toBeInTheDocument()
  expect(within(repository).getByRole("option", { name: "Infrastructure" })).toBeInTheDocument()
})

test("an archive keeps the server's name only when it is one Quai gives", () => {
  expect(archiveName(`attachment; filename="quai-evidence-20261109T120000Z.zip"`)).toBe("quai-evidence-20261109T120000Z.zip")
  expect(archiveName(`attachment; filename="../../evil.exe"`)).toBe("quai-evidence.zip")
  expect(archiveName(null)).toBe("quai-evidence.zip")
})
