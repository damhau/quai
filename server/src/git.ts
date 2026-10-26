import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { createHash } from "node:crypto";
import { existsSync } from "node:fs";
import { mkdir, readFile, readdir, rm, stat, writeFile } from "node:fs/promises";
import { dirname, join, relative, resolve, sep } from "node:path";
import type { RenderedFile } from "@quai/core";

const exec = promisify(execFile);

export interface GitConfig {
  /** Remote URL or a local path; a missing local path becomes a bare repo. */
  remote: string;
  workdir: string;
  branch: string;
  committer: { name: string; email: string };
}

export interface CommitOp {
  /** Directories or files to remove before writing (a resource is always rewritten whole). */
  removeDirs: string[];
  write: RenderedFile[];
  message: string;
  author: { name: string; email: string };
}

export interface LogEntry {
  sha: string;
  author: string;
  email: string;
  date: string;
  subject: string;
  body: string;
}

/**
 * The Git repository is the system of record: deployments and blueprints live
 * there, and Quai is just a well-behaved committer. All writes are serialized
 * and retried against the latest remote head so concurrent requests never
 * clobber each other or a human's commit.
 */
export class GitRepo {
  private queue: Promise<unknown> = Promise.resolve();

  constructor(private readonly cfg: GitConfig) {}

  get workdir() {
    return this.cfg.workdir;
  }

  private async git(args: string[], env: Record<string, string> = {}) {
    const { stdout } = await exec("git", args, {
      cwd: this.cfg.workdir,
      env: { ...process.env, GIT_TERMINAL_PROMPT: "0", ...env },
      maxBuffer: 32 * 1024 * 1024,
    });
    return stdout;
  }

  private serialize<T>(fn: () => Promise<T>): Promise<T> {
    const run = this.queue.then(fn, fn);
    this.queue = run.catch(() => undefined);
    return run;
  }

  async init(seed: (workdir: string) => Promise<boolean>) {
    const { remote, workdir, branch } = this.cfg;
    const isUrl = /^[a-z+]+:\/\/|^git@/.test(remote);
    if (!isUrl && !existsSync(remote)) {
      await mkdir(remote, { recursive: true });
      await exec("git", ["init", "--bare", "-q", "-b", branch, remote]);
    }
    if (!existsSync(join(workdir, ".git"))) {
      await mkdir(dirname(workdir), { recursive: true });
      await exec("git", ["clone", "-q", remote, workdir]);
    }
    await this.git(["config", "user.name", this.cfg.committer.name]);
    await this.git(["config", "user.email", this.cfg.committer.email]);
    await this.serialize(async () => {
      const hasRemoteBranch = await this.remoteHasBranch();
      if (hasRemoteBranch) await this.syncUnlocked();
      else await this.git(["checkout", "-q", "-B", branch]);
      if (await seed(workdir)) {
        await this.git(["add", "-A"]);
        await this.git(["commit", "-q", "-m", "quai: seed blueprint catalog"]);
        await this.git(["push", "-q", "origin", `HEAD:${branch}`]);
      }
    });
  }

  private async remoteHasBranch() {
    const out = await this.git(["ls-remote", "--heads", "origin", this.cfg.branch]);
    return out.trim().length > 0;
  }

  private async syncUnlocked() {
    await this.git(["fetch", "-q", "origin", this.cfg.branch]);
    await this.git(["checkout", "-q", "-B", this.cfg.branch, `origin/${this.cfg.branch}`]);
    await this.git(["reset", "-q", "--hard", `origin/${this.cfg.branch}`]);
    await this.git(["clean", "-q", "-fd"]);
  }

  /** Pull the latest remote state (picks up commits made outside Quai). */
  sync() {
    return this.serialize(() => this.syncUnlocked());
  }

  async head(): Promise<string> {
    return (await this.git(["rev-parse", "HEAD"])).trim();
  }

  private abs(path: string) {
    const p = resolve(this.cfg.workdir, path);
    if (p !== this.cfg.workdir && !p.startsWith(this.cfg.workdir + sep)) throw new Error(`Path escapes repository: ${path}`);
    if (relative(this.cfg.workdir, p).split(sep)[0] === ".git") throw new Error(`Refusing to touch .git: ${path}`);
    return p;
  }

  async readFile(path: string): Promise<string | undefined> {
    try {
      return await readFile(this.abs(path), "utf8");
    } catch {
      return undefined;
    }
  }

  /** All files under a directory (or the file itself), recursively, with repo-relative paths. */
  async readDir(dir: string): Promise<RenderedFile[]> {
    const root = this.abs(dir);
    if (!existsSync(root)) return [];
    if (!(await stat(root)).isDirectory()) return [{ path: dir, content: await readFile(root, "utf8") }];
    const out: RenderedFile[] = [];
    const walk = async (d: string) => {
      for (const e of await readdir(d, { withFileTypes: true })) {
        const full = join(d, e.name);
        if (e.name === ".git") continue;
        if (e.isDirectory()) await walk(full);
        else out.push({ path: relative(this.cfg.workdir, full).split(sep).join("/"), content: await readFile(full, "utf8") });
      }
    };
    await walk(root);
    return out.sort((a, b) => a.path.localeCompare(b.path));
  }

  /** Every file and directory under `prefix` (repo-relative), skipping .git. */
  async walk(prefix = ""): Promise<{ files: string[]; dirs: string[] }> {
    const files: string[] = [];
    const dirs: string[] = [];
    const root = this.abs(prefix || ".");
    if (!existsSync(root) || !(await stat(root)).isDirectory()) return { files, dirs };
    const rel = (p: string) => relative(this.cfg.workdir, p).split(sep).join("/");
    const go = async (d: string) => {
      for (const e of await readdir(d, { withFileTypes: true })) {
        if (e.name === ".git") continue;
        const full = join(d, e.name);
        if (e.isDirectory()) {
          dirs.push(rel(full));
          await go(full);
        } else files.push(rel(full));
      }
    };
    await go(root);
    return { files: files.sort(), dirs: dirs.sort() };
  }

  /** Repo-relative paths of every file with the given basename. */
  async find(basename: string, skip: string[] = []): Promise<string[]> {
    const out: string[] = [];
    const walk = async (d: string) => {
      for (const e of await readdir(d, { withFileTypes: true })) {
        if (e.name === ".git" || skip.includes(e.name)) continue;
        const full = join(d, e.name);
        if (e.isDirectory()) await walk(full);
        else if (e.name === basename) out.push(relative(this.cfg.workdir, full).split(sep).join("/"));
      }
    };
    await walk(this.cfg.workdir);
    return out.sort();
  }

  /** Content fingerprint of a directory, used to detect stale approvals. */
  async fingerprint(dir: string): Promise<string> {
    const h = createHash("sha256");
    for (const f of await this.readDir(dir)) h.update(f.path).update("\0").update(f.content).update("\0");
    return h.digest("hex");
  }

  /**
   * Applies `op` on top of the latest remote head and pushes. `precondition`
   * runs after syncing and can veto the commit (e.g. the target changed since
   * it was approved). Returns the new commit SHA, or the current head when
   * the change turned out to be a no-op.
   */
  commit(op: CommitOp, precondition?: () => Promise<void>): Promise<{ sha: string; noop: boolean }> {
    return this.serialize(async () => {
      for (let attempt = 0; ; attempt++) {
        await this.syncUnlocked();
        if (precondition) await precondition();
        for (const d of op.removeDirs) await rm(this.abs(d), { recursive: true, force: true });
        for (const f of op.write) {
          const p = this.abs(f.path);
          await mkdir(dirname(p), { recursive: true });
          await writeFile(p, f.content);
        }
        await this.git(["add", "-A"]);
        const status = await this.git(["status", "--porcelain"]);
        if (!status.trim()) return { sha: await this.head(), noop: true };
        await this.git(["commit", "-q", "-m", op.message, "--author", `${op.author.name} <${op.author.email}>`]);
        try {
          await this.git(["push", "-q", "origin", `HEAD:${this.cfg.branch}`]);
          return { sha: await this.head(), noop: false };
        } catch (e) {
          if (attempt >= 3) throw e;
        }
      }
    });
  }

  async log(opts: { path?: string; limit?: number } = {}): Promise<LogEntry[]> {
    const SEP = "\x1e";
    const args = ["log", `-n${opts.limit ?? 50}`, `--format=%H%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%b${SEP}`];
    if (opts.path) args.push("--", opts.path);
    let out: string;
    try {
      out = await this.git(args);
    } catch {
      return [];
    }
    return out
      .split(SEP)
      .map((s) => s.trim())
      .filter(Boolean)
      .map((s) => {
        const [sha, author, email, date, subject, body] = s.split("\x1f");
        return { sha, author, email, date, subject, body: body ?? "" };
      });
  }

  async show(sha: string): Promise<string> {
    if (!/^[0-9a-f]{7,40}$/.test(sha)) throw new Error("Invalid commit SHA");
    return this.git(["show", "--format=%H%n%an <%ae>%n%aI%n%n%B", sha]);
  }
}
