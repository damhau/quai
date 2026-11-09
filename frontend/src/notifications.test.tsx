import { screen } from "@testing-library/react"
import { renderApp } from "@/test/render"
import { pendingVM, requests } from "@/test/requests"

test("a request says whom its notifications do not reach", async () => {
  requests.current = {
    ...pendingVM(),
    requesterNotNotified: true,
    rules: [
      { names: ["type/production"], approvers: ["group:platform-admins"], min: 1, approvals: [], satisfied: false },
      { names: ["type/large-vm"], approvers: ["group:finops"], min: 1, approvals: [], satisfied: false, noOneToNotify: true },
    ],
  }
  renderApp(`/requests/${requests.current.id}`)
  expect(await screen.findByText(/gets no notifications of this request: Quai knows no verified email of theirs/)).toBeInTheDocument()
  expect(screen.getAllByText(/No one who may approve is notified/)).toHaveLength(1)
})

test("a request whose notifications reach everyone says nothing of them", async () => {
  requests.current = pendingVM()
  renderApp(`/requests/${requests.current.id}`)
  expect(await screen.findByText("Approval")).toBeInTheDocument()
  expect(screen.queryByText(/gets no notifications/)).not.toBeInTheDocument()
  expect(screen.queryByText(/No one who may approve is notified/)).not.toBeInTheDocument()
})
