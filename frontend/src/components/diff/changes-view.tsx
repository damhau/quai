import { useState } from "react"
import { ChevronRight } from "lucide-react"
import type { ChangeView } from "@/api/generated/types.gen"
import { fold } from "@/components/diff/fold"
import { Badge } from "@/components/ui/badge"
import { cn } from "@/lib/utils"

const opLabel = { create: "A", update: "M", delete: "D" } as const

/** ChangesView shows the files a change set touches, each with its diff. */
export function ChangesView({ changes }: { changes: ChangeView[] }) {
  const [closed, setClosed] = useState<Record<string, boolean>>({})
  if (!changes.length) return <p className="text-muted-foreground text-sm">No file changes.</p>
  const count = (op: string) => changes.reduce((n, c) => n + (c.diff ?? []).filter((l) => l.op === op).length, 0)
  return (
    <div className="grid gap-2">
      <p className="text-muted-foreground text-xs">
        {changes.length} file{changes.length > 1 ? "s" : ""} · <span className="text-emerald-600 dark:text-emerald-400">+{count("+")}</span>{" "}
        <span className="text-red-600 dark:text-red-400">−{count("-")}</span>
      </p>
      {changes.map((c) => {
        const open = !closed[c.path]
        const lines = c.op === "update" ? fold(c.diff ?? [], 3) : (c.diff ?? [])
        return (
          <div key={c.path} className="overflow-hidden rounded-md border">
            <button
              type="button"
              className="hover:bg-accent flex w-full items-center gap-2 px-3 py-2 text-left text-sm"
              aria-expanded={open}
              onClick={() => setClosed({ ...closed, [c.path]: open })}
            >
              <ChevronRight className={cn("size-3.5 transition-transform", open && "rotate-90")} aria-hidden />
              <Badge variant={c.op === "delete" ? "destructive" : "secondary"} title={c.op}>
                {opLabel[c.op]}
              </Badge>
              <code className="truncate font-mono text-xs">{c.path}</code>
            </button>
            {open && lines.length > 0 && (
              <pre className="bg-muted/40 overflow-x-auto border-t py-1 font-mono text-xs leading-5" aria-label={`Diff of ${c.path}`}>
                {lines.map((l, i) =>
                  l === null ? (
                    <div key={i} className="text-muted-foreground px-3">
                      ⋯
                    </div>
                  ) : (
                    <div
                      key={i}
                      className={cn(
                        "px-3 whitespace-pre",
                        l.op === "+" && "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
                        l.op === "-" && "bg-red-500/10 text-red-700 dark:text-red-300",
                      )}
                    >
                      <span className="text-muted-foreground mr-2 select-none">{l.op}</span>
                      {l.text}
                    </div>
                  ),
                )}
              </pre>
            )}
          </div>
        )
      })}
    </div>
  )
}
