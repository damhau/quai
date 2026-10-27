import { expect, test } from "@playwright/test"
import { signIn } from "./helpers"

test("only a resource's owner, or a platform admin, can change it", async ({ page }) => {
  await signIn(page, "carol", "/resources/infra-live/vm/prod/web01")
  await expect(page.getByText("Only bob or an operator can change this resource.")).toBeVisible()
  await expect(page.getByRole("toolbar", { name: "Day-2 actions" })).toBeHidden()

  await signIn(page, "bob", "/resources/infra-live/vm/prod/db01")
  await expect(page.getByText("Only dana or an operator can change this resource.")).toBeVisible()

  await signIn(page, "alice", "/resources/infra-live/vm/prod/db01")
  await expect(page.getByRole("toolbar", { name: "Day-2 actions" }).getByRole("button", { name: "Resize" })).toBeEnabled()
})

test("a requester has nothing to approve", async ({ page }) => {
  await signIn(page, "carol", "/approvals")
  await expect(page.getByText("Nothing needs your approval right now.")).toBeVisible()
})
