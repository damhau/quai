import { createContext, lazy, Suspense, useContext, useEffect, useState } from "react";
import { NavLink, Route, Routes } from "react-router-dom";
import { api, currentUserName, setCurrentUserName, useApi, type Me, type RequestView } from "./api";
import { Icon } from "./components/Icon";
import { Catalog } from "./pages/Catalog";
import { NewRequest } from "./pages/NewRequest";
import { Requests } from "./pages/Requests";
import { RequestDetail } from "./pages/RequestDetail";
import { Deployments } from "./pages/Deployments";
import { DeploymentDetail } from "./pages/DeploymentDetail";
import { GitLog } from "./pages/GitLog";
import type { Principal } from "@quai/core";

// The designer pulls in React Flow; load it only when a platform admin opens it.
const Designer = lazy(() => import("./pages/Designer").then((m) => ({ default: m.Designer })));

const MeContext = createContext<Me | undefined>(undefined);
export const useMe = () => useContext(MeContext)!;

export function App() {
  const [me, setMe] = useState<Me>();
  const [users, setUsers] = useState<Principal[]>([]);
  const [error, setError] = useState<string>();
  const [userKey, setUserKey] = useState(currentUserName());
  const inbox = useApi<RequestView[]>(me ? `/requests?view=inbox&u=${userKey}` : null, 5000);

  useEffect(() => {
    api<Me>("/me")
      .then((m) => {
        setMe(m);
        if (m.authMode === "dev") api<Principal[]>("/users").then(setUsers);
      })
      .catch((e) => setError(e.message));
  }, [userKey]);

  const switchUser = (name: string) => {
    setCurrentUserName(name);
    setUserKey(name);
  };

  if (error) return <div className="boot">Quai API unavailable: {error}</div>;
  if (!me) return <div className="boot">Loading…</div>;
  const pending = inbox.data?.length ?? 0;

  return (
    <MeContext.Provider value={me}>
      <div className="shell" key={me.name}>
        <aside className="sidebar">
          <div className="brand">
            <div className="logo">
              <Icon name="layers" size={18} />
            </div>
            <div>
              <div className="brand-name">Quai</div>
              <div className="brand-sub">self-service · GitOps</div>
            </div>
          </div>
          <nav>
            <NavLink to="/" end>
              <Icon name="grid" /> Catalog
            </NavLink>
            <NavLink to="/deployments">
              <Icon name="server" /> Deployments
            </NavLink>
            <NavLink to="/requests">
              <Icon name="list" /> My requests
            </NavLink>
            <NavLink to="/approvals">
              <Icon name="inbox" /> Approvals {pending > 0 && <span className="count">{pending}</span>}
            </NavLink>
            <div className="nav-section">Platform</div>
            <NavLink to="/designer">
              <Icon name="pen" /> Blueprint designer
            </NavLink>
            <NavLink to="/git">
              <Icon name="git" /> Git log
            </NavLink>
          </nav>
          <div className="whoami">
            {me.authMode === "dev" ? (
              <>
                <label htmlFor="user-switch" className="small muted">
                  Signed in as (dev mode)
                </label>
                <select id="user-switch" value={me.name} onChange={(e) => switchUser(e.target.value)}>
                  {users.map((u) => (
                    <option key={u.name} value={u.name}>
                      {u.displayName ?? u.name}
                    </option>
                  ))}
                </select>
              </>
            ) : (
              <div>{me.displayName ?? me.name}</div>
            )}
            <div className="groups">
              {me.groups.map((g) => (
                <span key={g} className="tag">
                  {g}
                </span>
              ))}
            </div>
          </div>
        </aside>
        <main className="content">
          <Routes>
            <Route path="/" element={<Catalog />} />
            <Route path="/request/:blueprint" element={<NewRequest />} />
            <Route path="/requests" element={<Requests view="mine" />} />
            <Route path="/approvals" element={<Requests view="inbox" onChange={inbox.reload} />} />
            <Route path="/requests/:id" element={<RequestDetail onChange={inbox.reload} />} />
            <Route path="/deployments" element={<Deployments />} />
            <Route path="/deployments/:name" element={<DeploymentDetail />} />
            <Route path="/designer" element={<Suspense fallback={<div className="page muted">Loading designer…</div>}><Designer /></Suspense>} />
            <Route path="/designer/:name" element={<Suspense fallback={<div className="page muted">Loading designer…</div>}><Designer /></Suspense>} />
            <Route path="/git" element={<GitLog />} />
          </Routes>
        </main>
      </div>
    </MeContext.Provider>
  );
}
