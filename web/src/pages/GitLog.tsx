import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api, ago, useApi, type CommitView } from "../api";
import { ErrorNote } from "../components/Bits";
import { Icon } from "../components/Icon";

export function GitLog() {
  const { data, error, reload } = useApi<CommitView[]>("/git/log?limit=100", 5000);
  const [params, setParams] = useSearchParams();
  const sha = params.get("sha") ?? data?.[0]?.sha;
  const [show, setShow] = useState<string>();
  const [syncing, setSyncing] = useState(false);

  useEffect(() => {
    if (!sha) return;
    api<string>(`/git/commits/${sha}`).then(setShow).catch(() => setShow("Commit not found."));
  }, [sha]);

  const pull = async () => {
    setSyncing(true);
    try {
      await api("/git/sync", { body: {} });
      await reload();
    } finally {
      setSyncing(false);
    }
  };

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>Git log</h1>
          <p className="muted">The GitOps repository is the system of record. Quai is just one committer among humans, bots and CI.</p>
        </div>
        <button className="btn" onClick={pull} disabled={syncing}>
          <Icon name="refresh" /> Pull from remote
        </button>
      </header>
      <ErrorNote error={error} />
      <div className="split">
        <div className="panel">
          <ol className="commits">
            {data?.map((c) => (
              <li key={c.sha} className={c.sha === sha ? "on" : ""}>
                <button onClick={() => setParams({ sha: c.sha })}>
                  <code className="sha-plain">{c.sha.slice(0, 7)}</code>
                  <div>
                    <div className="strong">{c.subject}</div>
                    <div className="small muted">
                      {c.author} · {ago(c.date)}
                    </div>
                  </div>
                </button>
              </li>
            ))}
          </ol>
        </div>
        <div className="panel">
          <pre className="show">
            {show?.split("\n").map((l, i) => (
              <div key={i} className={l.startsWith("+") && !l.startsWith("+++") ? "add" : l.startsWith("-") && !l.startsWith("---") ? "del" : l.startsWith("diff ") ? "hdr" : ""}>
                {l || " "}
              </div>
            ))}
          </pre>
        </div>
      </div>
    </div>
  );
}
