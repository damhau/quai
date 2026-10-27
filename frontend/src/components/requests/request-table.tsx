import { Link } from "@tanstack/react-router"
import type { RequestView } from "@/api/generated/types.gen"
import { StateBadge } from "@/components/requests/state-badge"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { ago } from "@/lib/format"
import { requestTitle } from "@/lib/requests"

export function RequestTable({ requests, showRequester }: { requests: RequestView[]; showRequester?: boolean }) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead className="w-16">#</TableHead>
          <TableHead>Request</TableHead>
          {showRequester && <TableHead>Requested by</TableHead>}
          <TableHead>State</TableHead>
          <TableHead className="text-right">Updated</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {requests.map((r) => (
          <TableRow key={r.id}>
            <TableCell className="text-muted-foreground">#{r.number}</TableCell>
            <TableCell className="max-w-md">
              <Link to="/requests/$id" params={{ id: r.id }} className="font-medium hover:underline">
                {requestTitle(r)}
              </Link>
              {r.reason && <p className="text-muted-foreground truncate text-xs">{r.reason}</p>}
            </TableCell>
            {showRequester && <TableCell>{r.requester}</TableCell>}
            <TableCell>
              <StateBadge state={r.state} />
            </TableCell>
            <TableCell className="text-muted-foreground text-right text-xs" title={new Date(r.updatedAt).toLocaleString()}>
              {ago(r.updatedAt)}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
