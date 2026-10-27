import { createContext, useContext } from "react"
import { createStore, useStore } from "zustand"
import type { Doc } from "@/components/designer/blueprint"

// The designer's draft: the blueprint being edited, which resource is selected, and whether the
// draft differs from what was published. It is client state, one store per designer; the
// published blueprint is server state, read through TanStack Query.

export type DesignerState = {
  draft: Doc
  selected: string | null
  dirty: boolean
  /** edit applies a change; a change that returns null is refused, and edit returns false. */
  edit: (fn: (d: Doc) => Doc | null) => boolean
  select: (id: string | null) => void
  /** published marks the draft as what the repository now holds. */
  published: () => void
}

export function createDesignerStore(draft: Doc) {
  return createStore<DesignerState>()((set, get) => ({
    draft,
    selected: null,
    dirty: false,
    edit: (fn) => {
      const next = fn(get().draft)
      if (next === null) return false
      set({ draft: next, dirty: true })
      return true
    },
    select: (selected) => set({ selected }),
    published: () => set({ dirty: false }),
  }))
}

export const DesignerContext = createContext<ReturnType<typeof createDesignerStore> | null>(null)

export function useDesigner<T>(selector: (s: DesignerState) => T): T {
  const store = useContext(DesignerContext)
  if (!store) throw new Error("useDesigner outside a designer")
  return useStore(store, selector)
}
