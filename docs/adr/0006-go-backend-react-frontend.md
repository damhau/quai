# ADR 0006: Go backend, React frontend

Status: Accepted · 2026-10-26

## Context

The prototype is TypeScript end to end, with an engine shared by browser and
server. For the enterprise product the options were: stay on Node/TypeScript,
move the backend to Python (FastAPI) or Go, or Rust. Most code will be written
with an AI assistant, so the deciding factors are library quality for the hard
problems, how easily the owner can review and operate the code, how much the
compiler catches, and distribution on-premises.

## Decision

- **Backend: Go.** One static binary serves the API, runs workers and provides the CLI (`quai lint`, `quai report`).
- **Frontend: React 19 + TypeScript + Vite, TanStack Router and Query, Tailwind CSS v4, shadcn/ui**, with an API client generated from the backend's OpenAPI document.

## Rationale

- Go has the infrastructure ecosystem's libraries natively: `hclwrite` (the only format-preserving HCL editor), `terraform-json`, `client-go`, OPA, CEL, and `goccy/go-yaml` with positions for minimal-diff YAML editing.
- A single static binary suits air-gapped installs, OpenShift and customer CI.
- A small dependency surface and the Go checksum database answer security reviews in regulated organizations.
- A strict compiler and plain style make generated code easier to review.
- Huma gives FastAPI-like ergonomics: OpenAPI and validation from structs.

## Consequences

- The shared TypeScript engine is gone. Previews and validation come from the API's `plan` endpoint (already debounced); the frontend keeps only form behaviour (CEL visibility, basic constraints).
- The prototype must be ported (Phase 0). The riskiest piece, YAML editing, is validated first in a one-week spike.
- The project has two languages: Go and TypeScript.

## Alternatives considered

- **Node/TypeScript.** Viable, no rewrite, shared engine. Rejected mainly for HCL editing (would need a Go component anyway), distribution and npm supply-chain exposure.
- **Python/FastAPI.** Best YAML round-trip library (`ruamel.yaml`) and fastest CRUD development; rejected because development speed matters less with AI-written code, and Python lacks HCL editing and single-binary distribution.
- **Rust.** Rejected: no mature comment-preserving YAML editor, harder to review, its strengths do not match Quai's bottlenecks (Git and network I/O).
