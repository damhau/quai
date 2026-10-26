import Fastify, { type FastifyReply, type FastifyRequest } from "fastify";
import fastifyStatic from "@fastify/static";
import { existsSync } from "node:fs";
import { timingSafeEqual } from "node:crypto";
import { components, type Principal } from "@quai/core";
import type { QuaiConfig } from "./config.ts";
import { HttpError, Quai } from "./service.ts";
import { Resources, typeView } from "./resources.ts";
import type { RequestRecord } from "./store.ts";

export async function buildApp(cfg: QuaiConfig) {
  const quai = new Quai(cfg);
  await quai.init();
  const resources = new Resources(quai);
  const app = Fastify({ logger: process.env.NODE_ENV === "test" ? false : { level: "info" }, bodyLimit: 2 * 1024 * 1024 });

  app.setErrorHandler((err: any, _req, reply) => {
    if (err instanceof HttpError) return reply.status(err.status).send({ error: err.message, details: err.details });
    if (err.validation || err.statusCode === 400) return reply.status(400).send({ error: err.message });
    app.log.error(err);
    return reply.status(500).send({ error: "Internal error" });
  });

  const user = (req: FastifyRequest): Principal => quai.resolveUser(req.headers);

  const view = (r: RequestRecord, me: Principal, full = false) => {
    const outcome = quai.outcome(r);
    const { files, ...rest } = r;
    return {
      ...rest,
      changes: full ? r.changes : r.changes.map(({ path, op }) => ({ path, op })),
      ...(full ? { files } : {}),
      approval: outcome.rules.map((s) => ({ ...s.rule, min: s.rule.min ?? 1, approvals: s.approvals, satisfied: s.satisfied })),
      canReview: quai.canReviewRequest(me, r),
      canCancel: r.status === "pending" && (r.requester === me.name || quai.isAdmin(me)),
    };
  };

  // ------------------------------------------------------------ identity

  app.get("/api/me", async (req) => ({ ...user(req), admin: quai.isAdmin(user(req)), authMode: cfg.auth.mode }));
  app.get("/api/users", async () => (cfg.auth.mode === "dev" ? cfg.auth.users : []));

  // ---------------------------------------------------------- blueprints

  app.get("/api/components", async () => components.map(({ render, connectionSecret, ...c }) => ({ ...c, publishesSecret: !!connectionSecret })));

  app.get("/api/blueprints", async () => {
    const { items, errors } = await quai.blueprints();
    return { items, errors };
  });

  app.get<{ Params: { name: string } }>("/api/blueprints/:name", async (req) => quai.blueprint(req.params.name));

  app.put<{ Params: { name: string }; Body: unknown }>("/api/blueprints/:name", async (req) => quai.saveBlueprint(user(req), req.params.name, req.body));

  // --------------------------------------------------------- deployments

  app.get("/api/deployments", async () => quai.deployments());

  app.get<{ Params: { name: string } }>("/api/deployments/:name", async (req) => {
    const me = user(req);
    const d = await quai.deploymentDetail(req.params.name);
    return { ...d, requests: d.requests.map((r) => view(r, me)), canOperate: d.record.metadata.owner === me.name || quai.isAdmin(me) };
  });

  // ------------------------------------------------------------ requests

  type CreateBody = { blueprint: string; name: string; inputs?: Record<string, unknown>; reason?: string };
  type ActionBody = { inputs?: Record<string, unknown>; reason?: string };

  const planView = (p: Awaited<ReturnType<Quai["planCreate"]>>) => ({
    kind: p.kind,
    action: p.action,
    resourceType: p.resourceType,
    stack: p.stack,
    deployment: p.deployment,
    dir: p.dir,
    inputs: p.inputs,
    changes: p.changes,
    rules: p.rules,
  });

  /** Dry run: what would be committed and who must approve it. */
  app.post<{ Body: CreateBody }>("/api/plan", async (req) => planView(await quai.planCreate(user(req), req.body)));

  app.post<{ Params: { name: string; action: string }; Body: ActionBody }>("/api/deployments/:name/actions/:action/plan", async (req) =>
    planView(await quai.planAction(user(req), req.params.name, req.params.action, req.body ?? {})),
  );

  app.post<{ Body: CreateBody }>("/api/requests", async (req, reply) => {
    const me = user(req);
    const r = await quai.submit(me, await quai.planCreate(me, req.body), req.body.reason ?? "");
    return reply.status(201).send(view(r, me, true));
  });

  app.post<{ Params: { name: string; action: string }; Body: ActionBody }>("/api/deployments/:name/actions/:action", async (req, reply) => {
    const me = user(req);
    const plan = await quai.planAction(me, req.params.name, req.params.action, req.body ?? {});
    const r = await quai.submit(me, plan, req.body?.reason ?? "");
    return reply.status(201).send(view(r, me, true));
  });

  app.get<{ Querystring: { view?: "mine" | "inbox" | "all" } }>("/api/requests", async (req) => {
    const me = user(req);
    return quai.list(me, req.query.view ?? "all").map((r) => view(r, me));
  });

  app.get<{ Params: { id: string } }>("/api/requests/:id", async (req) => {
    const me = user(req);
    const r = quai.get(req.params.id);
    return { ...view(r, me, true), events: quai.store.listEvents({ requestId: r.id }) };
  });

  app.post<{ Params: { id: string }; Body: { decision: "approve" | "deny"; comment?: string } }>("/api/requests/:id/review", async (req) => {
    const me = user(req);
    if (!["approve", "deny"].includes(req.body?.decision)) throw new HttpError(400, "decision must be approve or deny");
    return view(await quai.review(me, req.params.id, req.body.decision, req.body.comment), me, true);
  });

  app.post<{ Params: { id: string } }>("/api/requests/:id/cancel", async (req) => {
    const me = user(req);
    return view(quai.cancel(me, req.params.id), me, true);
  });

  app.post<{ Params: { id: string } }>("/api/requests/:id/retry", async (req) => {
    const me = user(req);
    return view(await quai.retry(me, req.params.id), me, true);
  });

  // ------------------------------------------------- repo-defined resource types

  app.get("/api/types", async () => {
    const { types, errors } = await resources.types();
    const inventory = await resources.inventory();
    const items = [];
    for (const t of types) {
      items.push({ ...typeView(t, await resources.discover(t)), count: inventory.filter((i) => i.type === t.type.metadata.name).length });
    }
    return { items, errors };
  });

  app.get<{ Params: { type: string } }>("/api/types/:type", async (req) => {
    const t = await resources.type(req.params.type);
    return typeView(t, await resources.discover(t));
  });

  type TypedCreate = { params?: Record<string, unknown>; document?: Record<string, unknown>; reason?: string };
  type TypedAction = { document?: Record<string, unknown>; inputs?: Record<string, unknown>; reason?: string };

  app.post<{ Params: { type: string }; Body: TypedCreate }>("/api/types/:type/plan", async (req) => planView(await resources.planCreate(user(req), req.params.type, req.body ?? {})));

  app.post<{ Params: { type: string }; Body: TypedCreate }>("/api/types/:type/requests", async (req, reply) => {
    const me = user(req);
    const r = await quai.submit(me, await resources.planCreate(me, req.params.type, req.body ?? {}), req.body?.reason ?? "");
    return reply.status(201).send(view(r, me, true));
  });

  app.get<{ Params: { type: string; key: string } }>("/api/types/:type/resources/:key", async (req) => {
    const me = user(req);
    const d = await resources.detail(me, req.params.type, req.params.key);
    return { ...d, requests: d.requests.map((r) => view(r, me)) };
  });

  app.post<{ Params: { type: string; key: string; action: string }; Body: TypedAction }>("/api/types/:type/resources/:key/actions/:action/plan", async (req) =>
    planView(await resources.planAction(user(req), req.params.type, req.params.key, req.params.action, req.body ?? {})),
  );

  app.post<{ Params: { type: string; key: string; action: string }; Body: TypedAction }>("/api/types/:type/resources/:key/actions/:action", async (req, reply) => {
    const me = user(req);
    const plan = await resources.planAction(me, req.params.type, req.params.key, req.params.action, req.body ?? {});
    const r = await quai.submit(me, plan, req.body?.reason ?? "");
    return reply.status(201).send(view(r, me, true));
  });

  /** One list for everything Quai can operate: blueprint deployments and typed resources found in the repo. */
  app.get("/api/inventory", async () => {
    const open = quai.store.listRequests({ status: ["pending", "approved"] });
    const openFor = (id: string) => open.filter((r) => r.deployment === id).length;
    const { types } = await resources.types();
    const titles = new Map(types.map((t) => [t.type.metadata.name, t.type]));
    const deployments = (await quai.deployments()).map((d) => ({
      kind: "deployment" as const,
      id: d.name,
      name: d.name,
      source: d.record.spec.blueprint,
      sourceTitle: d.record.spec.blueprint,
      icon: undefined as string | undefined,
      path: d.dir,
      env: typeof d.record.spec.inputs.environment === "string" ? d.record.spec.inputs.environment : undefined,
      owner: d.record.metadata.owner,
      sync: d.sync,
      openRequests: openFor(d.name),
      href: `/deployments/${d.name}`,
    }));
    const typed = (await resources.inventory()).map((i) => ({
      kind: "resource" as const,
      id: i.id,
      name: i.params.name ?? i.key,
      source: i.type,
      sourceTitle: titles.get(i.type)?.metadata.title ?? i.type,
      icon: titles.get(i.type)?.metadata.icon,
      path: i.path,
      env: i.params.env,
      owner: i.owner,
      error: i.error,
      sync: quai.store.getSync(i.id),
      openRequests: openFor(i.id),
      href: `/types/${i.type}/resources/${encodeURIComponent(i.key)}`,
    }));
    return [...deployments, ...typed];
  });

  // ----------------------------------------------------------------- git

  app.get<{ Querystring: { path?: string; limit?: string } }>("/api/git/log", async (req) =>
    quai.git.log({ path: req.query.path, limit: Math.min(Number(req.query.limit ?? 50) || 50, 500) }),
  );
  app.get<{ Params: { sha: string } }>("/api/git/commits/:sha", async (req, reply) => {
    try {
      return reply.type("text/plain").send(await quai.git.show(req.params.sha));
    } catch {
      throw new HttpError(404, "Commit not found");
    }
  });
  /** Pull commits made outside Quai (humans, Renovate, CI) into the working copy. */
  app.post("/api/git/sync", async () => {
    await quai.git.sync();
    return { head: await quai.git.head() };
  });

  app.get("/api/events", async () => quai.store.listEvents({ limit: 100 }));

  // ------------------------------------------------------------- webhook

  app.post<{ Body: Record<string, string> }>("/api/webhooks/sync", async (req: FastifyRequest<{ Body: Record<string, string> }>, reply: FastifyReply) => {
    if (cfg.webhookToken) {
      const got = Buffer.from(String(req.headers.authorization ?? ""));
      const want = Buffer.from(`Bearer ${cfg.webhookToken}`);
      if (got.length !== want.length || !timingSafeEqual(got, want)) throw new HttpError(401, "Invalid webhook token");
    } else if (cfg.auth.mode !== "dev") {
      throw new HttpError(503, "Set webhookToken to enable the sync webhook");
    }
    quai.reportSync(req.body ?? {});
    return reply.status(204).send();
  });

  app.get("/healthz", async () => ({ ok: true }));

  if (cfg.webDist && existsSync(cfg.webDist)) {
    await app.register(fastifyStatic, { root: cfg.webDist });
    app.setNotFoundHandler((req, reply) => {
      if (req.url.startsWith("/api/")) return reply.status(404).send({ error: "Not found" });
      return reply.sendFile("index.html");
    });
  }

  if (cfg.git.pullIntervalSeconds > 0) {
    const timer = setInterval(() => quai.git.sync().catch((e) => app.log.warn(`git pull failed: ${(e as Error).message}`)), cfg.git.pullIntervalSeconds * 1000);
    timer.unref();
    app.addHook("onClose", async () => clearInterval(timer));
  }
  app.addHook("onClose", async () => quai.store.close());
  return { app, quai };
}
