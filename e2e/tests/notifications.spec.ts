import { expect, request, test } from "@playwright/test"
import { delivered, preview, signIn, unique } from "./helpers"

type Message = { ID: string; Subject: string; To: { Address: string }[] }

/** mailTo returns the messages to an address whose subject holds text, from the local mail server
 * (Mailpit, docker-compose.yml), which catches what Quai sends. */
async function mailTo(address: string, text: string): Promise<Message[]> {
  const api = await request.newContext({ baseURL: process.env.E2E_MAILPIT_URL ?? "http://localhost:8025" })
  try {
    const res = await api.get("/api/v1/messages?limit=1000")
    expect(res.ok(), `Mailpit: ${res.status()}`).toBe(true)
    const { messages } = (await res.json()) as { messages: Message[] }
    return messages.filter((m) => m.Subject.includes(text) && m.To.some((t) => t.Address === address))
  } finally {
    await api.dispose()
  }
}

/** text returns the text part of a message. */
async function text(id: string): Promise<string> {
  const api = await request.newContext({ baseURL: process.env.E2E_MAILPIT_URL ?? "http://localhost:8025" })
  try {
    return ((await (await api.get(`/api/v1/message/${id}`)).json()) as { Text: string }).Text
  } finally {
    await api.dispose()
  }
}

test("the requester hears by email that their request was approved, merged and applied", async ({ page }) => {
  const name = `vm${unique()}`
  await signIn(page, "bob")
  await page.getByRole("link", { name: "Request Virtual machine (Terraform)" }).click()
  await page.getByRole("radio", { name: "dev" }).click()
  await page.getByRole("textbox", { name: /VM name/ }).fill(name)
  await expect(preview(page).getByText(`envs/dev/vms/${name}.yaml`)).toBeVisible()
  await page.getByRole("button", { name: "Submit and commit" }).click()
  await delivered(page)
  const id = new URL(page.url()).pathname.split("/").pop() ?? ""

  const target = `Create infra-live/vm/dev/${name}`
  await expect
    .poll(async () => (await mailTo("bob@example.com", target)).map((m) => m.Subject.replace(/#\d+ /, "")).sort(), { timeout: 60_000 })
    .toEqual([`[Quai] Applied: ${target}`, `[Quai] Approved: ${target}`, `[Quai] Merged: ${target}`])
  const [first] = await mailTo("bob@example.com", target)
  expect(await text(first?.ID ?? "")).toContain(`/requests/${id}`)
})
