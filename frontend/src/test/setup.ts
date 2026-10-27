import "@testing-library/jest-dom/vitest"
import { cleanup } from "@testing-library/react"
import { sent } from "./catalog"
import { resetRequests, server, state } from "./server"

if (typeof window !== "undefined") {
  // jsdom does not scroll (the router restores scroll positions on navigation) nor lay out
  // (Radix measures some controls).
  window.scrollTo = () => {}
  window.ResizeObserver ??= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
}

beforeAll(() => server.listen({ onUnhandledFrame: "error" }))
afterEach(() => {
  cleanup()
  server.resetHandlers()
  Object.assign(state, { user: "bob", auth: "dev", logouts: [] })
  Object.assign(sent, { plans: [], requests: [] })
  resetRequests()
})
afterAll(() => server.close())
