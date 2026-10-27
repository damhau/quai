import "@testing-library/jest-dom/vitest"
import { cleanup } from "@testing-library/react"
import { server, state } from "./server"

// jsdom does not scroll; the router restores scroll positions on navigation.
window.scrollTo = () => {}

beforeAll(() => server.listen({ onUnhandledFrame: "error" }))
afterEach(() => {
  cleanup()
  server.resetHandlers()
  Object.assign(state, { user: "bob", auth: "dev", logouts: [] })
})
afterAll(() => server.close())
