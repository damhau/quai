# Frontend engineering (React)

Status: Draft · 2026-10-26 · Decision: [ADR 0006](../adr/0006-go-backend-react-frontend.md)

## Stack

| Concern | Choice | Prototype today |
| --- | --- | --- |
| Build | Vite | Vite ✅ |
| UI | React 19, TypeScript strict | Same ✅ |
| Routing | TanStack Router, file-based, typed params and search params | `react-router-dom` v7 |
| Server state | TanStack Query | Hand-written `useApi` polling hook |
| Client state | Zustand, only for UI state (designer canvas, open panels, drafts) | `useState` |
| Styling | Tailwind CSS v4 (CSS-first `@theme`) | ~320 lines of hand-written CSS |
| Components | shadcn/ui (new-york), `lucide-react` icons | Hand-written components, inline SVG icons |
| API client | Generated from the backend's OpenAPI document, with TanStack Query hooks | Hand-written `fetch` and types |
| Graph canvas | React Flow (`@xyflow/react`) | Same ✅ |
| Code editors | CodeMirror 6 (YAML, JSON, CEL) | `<textarea>` |
| Diffs | `diff` (jsdiff) for line diffs, rendered with our own component | Engine's `lineDiff` from the shared TS core |
| Expressions | A JavaScript CEL implementation for instant `visibleIf` (selected in Phase 0 against cel-go's conformance tests); server remains authoritative | Shared TS engine |
| Languages | English only; no translation framework for now. UI copy stays in components; adding a framework later is a contained refactor | English only |
| Tests | Vitest + Testing Library, MSW for API mocks, Playwright end-to-end | Playwright scripts used ad hoc |

The frontend no longer shares an engine with the backend. Previews, validation
and rendering come from the API's `plan` endpoint, which the prototype already
calls with a 300 ms debounce. The only client-side logic is form behaviour:
field visibility (CEL) and basic schema constraints (required, ranges,
patterns) for instant feedback.

## Layout

```
frontend/
├── src/
│   ├── routes/                    # TanStack Router file routes
│   │   ├── __root.tsx
│   │   ├── index.tsx              # catalog
│   │   ├── request.$item.tsx
│   │   ├── inventory/
│   │   ├── resources.$repo.$type.$key.tsx
│   │   ├── requests/
│   │   ├── approvals.tsx
│   │   ├── designer/
│   │   └── admin/                 # repositories, executors, roles, audit
│   ├── api/                       # generated client and query hooks (never edited by hand)
│   ├── components/
│   │   ├── ui/                    # shadcn components (owned source)
│   │   ├── schema-form/           # JSON Schema → form
│   │   ├── diff/                  # change set diff viewer
│   │   └── approval/              # approval chain, review box
│   ├── stores/                    # zustand stores (designer, layout)
│   └── lib/                       # cel, formatting, auth helpers
├── index.html
└── vite.config.ts
```

## Setup

Node 22.12 or later (Vitest, MSW and the client generator need it). From `frontend/`:

```bash
npm ci
npm run dev                         # Vite on :5173, proxying /api and /auth to quai server on :8080
npm run api:openapi && npm run api:gen   # after an API change: OpenAPI document, then the client
npm run routes:gen                  # after adding a route file, if the dev server is not running
```

- **API client:** `@hey-api/openapi-ts` with its TanStack Query plugin generates `src/api/generated`
  (typed SDK functions, `…Options` query options and `…Mutation` options) from `openapi.json`,
  which `quai openapi` writes from the backend's handlers. Both are committed; the frontend CI
  regenerates them, and the route tree, and fails on any difference. OpenAPI 3.1 is supported by
  this generator, which is why it was chosen over orval.
- **Signing in:** the root route sends anyone without a session to `/login`, which follows the
  server's mode from `/api/v1/meta`: the development switcher, a redirect to the OIDC login, or a
  notice when the server has no database. Requests that change something carry the session's
  CSRF token from `/api/v1/me` (an interceptor in `src/api/client.ts`).
- **Bundles:** routes are split per page; libraries go to one `vendor` chunk that stays cached
  across releases.

## Conventions

- **Server state lives in TanStack Query.** No fetched data in Zustand. Polling for live status uses query `refetchInterval` while a request is in progress, and stops when it settles.
- **Never hand-write API types.** Regenerate the client when the OpenAPI document changes; a CI job fails if the committed client is out of date.
- **Edit shadcn source directly** in `components/ui/` rather than wrapping it.
- **Use the `@/` alias**, not deep relative imports.
- **Accessibility:** WCAG 2.1 AA. Every form control has a label; keyboard paths through catalog, request, approval and inventory are covered by Playwright tests; axe checks run in CI.
- **No third-party runtime resources:** fonts, icons and scripts are bundled, so the UI works air-gapped and the CSP stays strict.
- **Feedback loop:** `npm run typecheck` after each change; `npm run build` and Vitest before a PR.

## Key components

| Component | Notes |
| --- | --- |
| Schema form | Renders a JSON Schema (nested objects, arrays of objects, maps, enums of any type, formats) with the type's `ui` hints. Ported from the prototype's `SchemaForm`, rebuilt on shadcn form controls. |
| Change set viewer | Per repository, per file: diff with folded context, plan summary per resource, policy and cost results. |
| Approval panel | Rules with progress, reviews, step-up prompt, delegate indicator. |
| Request timeline | Submitted → PR → plan → approvals → merge → apply, live. |
| Inventory table | Server-side filtering, sorting and pagination (TanStack Table); saved views. |
| Designer | React Flow canvas whose palette lists types from connected repositories; saves as a pull request to the catalog repository. |
| Command palette | `⌘K` for catalog items, resources and requests. |
| Admin | Repository registration wizard with live access checks and health pages; executors; roles; audit log viewer. |

## Quality gates

```bash
npm run typecheck
npm run lint
npm run test        # vitest
npm run build
npx playwright test # in the integration pipeline, against a running backend
```
