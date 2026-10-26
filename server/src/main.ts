import { loadConfig } from "./config.ts";
import { buildApp } from "./app.ts";

const cfg = loadConfig();
const { app } = await buildApp(cfg);
await app.listen({ port: cfg.port, host: process.env.HOST ?? "0.0.0.0" });
app.log.info(`Quai is committing to ${cfg.git.remote} (${cfg.git.branch}), auth mode: ${cfg.auth.mode}`);
if (cfg.auth.mode === "dev") app.log.warn("Auth is in dev mode: anyone can act as any user. Use auth.mode=proxy behind an authenticating proxy in production.");
