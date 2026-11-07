// 每个用户一个 Durable Object（按用户 id 命名）。
//
// 为什么要它：免费版 Worker 每个请求只有 10ms CPU，密码哈希、
// gzip 解压和 sha256 都会超；DO 每个请求有 30 秒 CPU。所以这些重活都在这里做。
// 它同时是这个用户同步数据的唯一写入方：快照一次只写一个，账目（快照版本、
// blob、用量）放在自己的 SQLite 里，R2 只放文件本身。
// 推送：在线设备通过 Worker 挂一条 WebSocket 到这里（Hibernation API，
// 空闲时 DO 休眠不计费），有新快照就广播。

import { DurableObject } from "cloudflare:workers";
import type { Env, TierPolicy } from "./env";
import { hashPassword, verifyPassword } from "./crypto";
import { setStorageBytes } from "./db";
import { buildPasswordCodeMessage, sendMail } from "./smtp";
import { DAY_MS, errorJson, hex, json, nowIso, safeSegment, sseEvent } from "./util";

const PART_SIZE = 8 * 1024 * 1024; // R2 分片上传：除最后一片外每片一样大

export function userPrefix(userId: string): string {
  return `users/${safeSegment(userId)}/`;
}

export function manifestKey(userId: string): string {
  return `${userPrefix(userId)}manifest.json`;
}

export function blobKey(userId: string, sha: string): string {
  return `${userPrefix(userId)}blobs/${sha.slice(0, 2)}/${sha}.blob`;
}

function snapshotKey(userId: string, version: string): string {
  return `${userPrefix(userId)}snapshots/${version}.sqlite`;
}

class GzipError extends Error {}

interface StoreResult {
  size: number;
  sha256: string;
  stored: boolean; // 被 admit 拒绝或校验不符时为 false，R2 里什么也不留
}

const R2_DELETE_BATCH = 1000; // R2 一次 delete 最多 1000 个 key

/**
 * 把（可能是 gzip 的）请求体解压后写进 R2，同时算 sha256 和大小。
 * 请求体长度未知（客户端边压缩边发），所以大文件走分片上传。
 */
async function streamToR2(
  bucket: R2Bucket,
  key: string,
  body: ReadableStream<Uint8Array> | null,
  gzip: boolean,
  // admit(已收到的字节数)：每收到一块问一次还能不能存；返回 false 就放弃写入，
  // 但继续读完请求体，好报出完整大小。
  opts: { admit?: (size: number) => boolean; expectedSha256?: string },
): Promise<StoreResult> {
  let stream: ReadableStream<Uint8Array> = body ?? new ReadableStream({ start: (c) => c.close() });
  if (gzip) stream = stream.pipeThrough(new DecompressionStream("gzip"));
  const reader = stream.getReader();
  const digest = new crypto.DigestStream("SHA-256");
  const digestWriter = digest.getWriter();

  let size = 0;
  let storing = true;
  let pending: Uint8Array[] = [];
  let pendingBytes = 0;
  let upload: R2MultipartUpload | null = null;
  const parts: R2UploadedPart[] = [];

  const takePart = (n: number): Uint8Array => {
    const out = new Uint8Array(n);
    let off = 0;
    while (off < n) {
      const head = pending[0];
      const need = n - off;
      if (head.byteLength <= need) {
        out.set(head, off);
        off += head.byteLength;
        pending.shift();
      } else {
        out.set(head.subarray(0, need), off);
        pending[0] = head.subarray(need);
        off += need;
      }
    }
    pendingBytes -= n;
    return out;
  };

  const abort = async () => {
    storing = false;
    pending = [];
    pendingBytes = 0;
    if (upload) {
      await upload.abort().catch(() => {});
      upload = null;
    }
  };

  try {
    for (;;) {
      let chunk: ReadableStreamReadResult<Uint8Array>;
      try {
        chunk = await reader.read();
      } catch (e) {
        if (gzip) throw new GzipError(String(e));
        throw e;
      }
      if (chunk.done) break;
      const data = chunk.value;
      if (!data || data.byteLength === 0) continue;
      size += data.byteLength;
      await digestWriter.write(data);
      if (!storing) continue;
      if (opts.admit && !opts.admit(size)) {
        await abort();
        continue;
      }
      pending.push(data);
      pendingBytes += data.byteLength;
      while (pendingBytes >= PART_SIZE) {
        upload ??= await bucket.createMultipartUpload(key);
        parts.push(await upload.uploadPart(parts.length + 1, takePart(PART_SIZE)));
      }
    }
  } catch (e) {
    await abort();
    await digestWriter.abort().catch(() => {});
    throw e;
  }

  await digestWriter.close();
  const sha256 = hex(await digest.digest);
  if (opts.expectedSha256 !== undefined && sha256 !== opts.expectedSha256) await abort();
  if (!storing) return { size, sha256, stored: false };

  try {
    const rest = takePart(pendingBytes);
    if (upload) {
      if (rest.byteLength > 0) parts.push(await upload.uploadPart(parts.length + 1, rest));
      await upload.complete(parts);
    } else {
      await bucket.put(key, rest);
    }
  } catch (e) {
    await abort(); // 分片上传没完成就中止，不留下没人记账的分片
    throw e;
  }
  return { size, sha256, stored: true };
}

type SnapshotRow = {
  version: string;
  created_ms: number;
  size: number;
  current: number;
  r2_key: string | null;
};

// snapshots.current：0 = 备份，1 = 当前，2 = 正在发布（R2 里的 manifest 还没指向它）。
const BACKUP = 0;
const CURRENT = 1;
const PENDING = 2;

export class UserStore extends DurableObject<Env> {
  private sql: SqlStorage;
  // 一次只上传一个快照。
  private snapshotLock: Promise<unknown> = Promise.resolve();
  // 所有“提交”（改账目、清理备份、写 D1 用量）按顺序一个个来：
  // 账目不会交错，D1 里的用量也不会被较早算出的旧值覆盖。
  private commitChain: Promise<unknown> = Promise.resolve();
  // 正在进行的上传各自已经收到的字节数：并发上传一起占额度。
  private inflight = new Map<symbol, number>();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;
    this.sql.exec(`
      CREATE TABLE IF NOT EXISTS snapshots (
        version TEXT PRIMARY KEY,
        created_ms INTEGER NOT NULL,
        size INTEGER NOT NULL,
        sha256 TEXT NOT NULL,
        current INTEGER NOT NULL DEFAULT 0,
        r2_key TEXT
      );
      CREATE TABLE IF NOT EXISTS blobs (
        sha256 TEXT PRIMARY KEY,
        size INTEGER NOT NULL
      );
      CREATE TABLE IF NOT EXISTS meta (
        k TEXT PRIMARY KEY,
        v TEXT NOT NULL
      );
    `);
    const cols = this.sql.exec<{ name: string }>("PRAGMA table_info(snapshots)").toArray();
    if (!cols.some((c) => c.name === "r2_key")) this.sql.exec("ALTER TABLE snapshots ADD COLUMN r2_key TEXT");
    ctx.blockConcurrencyWhile(() => this.recoverPublication());
  }

  /**
   * 上次发布到一半就中断了（DO 重启）：看 R2 里的 manifest 指向谁。
   * 指向待发布的那份就把它补成当前快照；否则撤销它，删掉 R2 里的文件。
   */
  private async recoverPublication(): Promise<void> {
    const pending = this.sql.exec<SnapshotRow>("SELECT version,created_ms,size,current,r2_key FROM snapshots WHERE current=?", PENDING).toArray();
    if (pending.length === 0) return;
    const userId = this.sql.exec<{ v: string }>("SELECT v FROM meta WHERE k='user'").toArray()[0]?.v;
    const published = userId ? (await this.env.BUCKET.head(manifestKey(userId)))?.customMetadata?.snapshotKey : undefined;
    for (const row of pending) {
      if (row.r2_key && row.r2_key === published) {
        this.promote(row.version);
      } else {
        if (row.r2_key) await this.env.BUCKET.delete(row.r2_key).catch(() => {});
        this.sql.exec("DELETE FROM snapshots WHERE version=?", row.version);
      }
    }
  }

  /** 把一份待发布的快照变成当前快照，原来的当前快照变成备份。 */
  private promote(version: string): void {
    this.ctx.storage.transactionSync(() => {
      this.sql.exec("UPDATE snapshots SET current=? WHERE current=?", BACKUP, CURRENT);
      this.sql.exec("UPDATE snapshots SET current=? WHERE version=?", CURRENT, version);
    });
  }

  private withCommit<T>(fn: () => Promise<T>): Promise<T> {
    const run = this.commitChain.then(fn);
    this.commitChain = run.catch(() => {});
    return run;
  }

  // ---- 密码：CPU 重，放在这里 ----

  async hashPassword(password: string): Promise<string> {
    return hashPassword(password);
  }

  async verifyPassword(stored: string, password: string): Promise<boolean> {
    return verifyPassword(stored, password);
  }

  /** 发验证码邮件。TLS 握手也耗 CPU，一并放在这里。 */
  async sendPasswordCode(email: string, code: string, purpose: string): Promise<void> {
    await sendMail(this.env, email, buildPasswordCodeMessage(this.env, email, code, purpose));
  }

  // ---- 账目 ----

  private usageBytes(): number {
    const s = this.sql.exec<{ n: number }>("SELECT COALESCE(SUM(size),0) AS n FROM snapshots").one().n;
    return s + this.blobBytes();
  }

  private blobBytes(): number {
    return this.sql.exec<{ n: number }>("SELECT COALESCE(SUM(size),0) AS n FROM blobs").one().n;
  }

  /** 当前快照占的空间。发布过程中按新旧两份里较大的算，宁可多算。 */
  private currentSnapshotBytes(): number {
    return this.sql.exec<{ n: number }>("SELECT COALESCE(MAX(size),0) AS n FROM snapshots WHERE current IN (?,?)", CURRENT, PENDING).one().n;
  }

  private inflightBytes(except: symbol): number {
    let n = 0;
    for (const [k, v] of this.inflight) if (k !== except) n += v;
    return n;
  }

  /**
   * 给一次上传做额度检查。备份不算占用：每次提交后都会从最旧的开始清理，
   * 所以“能不能放下”只看当前快照、blob 和其他正在进行的上传。
   * withCurrentSnapshot 为 false 时（上传的就是新快照，会替换当前的）不算当前快照。
   */
  private quota(tier: TierPolicy, withCurrentSnapshot: boolean) {
    const id = Symbol("upload");
    this.inflight.set(id, 0);
    return {
      admit: (size: number): boolean => {
        const fixed = this.blobBytes() + (withCurrentSnapshot ? this.currentSnapshotBytes() : 0);
        const ok = fixed + this.inflightBytes(id) + size <= tier.storageLimitBytes;
        this.inflight.set(id, ok ? size : 0);
        return ok;
      },
      release: () => this.inflight.delete(id),
    };
  }

  private limitExceeded(tier: TierPolicy): Response {
    return json(
      { code: "storage_limit_exceeded", error: "storage usage exceeds tier limit", tier: tier.name, storageBytes: this.usageBytes(), storageLimitBytes: tier.storageLimitBytes },
      409,
    );
  }

  /** 提交的收尾：清理备份（失败只记日志，下次提交会再清），再把用量写到 D1。只在 withCommit 里调用。 */
  private async settle(userId: string, tier: TierPolicy): Promise<{ pruned: { count: number; bytes: number }; storageBytes: number }> {
    let pruned = { count: 0, bytes: 0 };
    try {
      pruned = await this.pruneBackups(userId, tier);
    } catch (e) {
      console.error("prune backups failed", String(e));
    }
    const storageBytes = this.usageBytes();
    try {
      await setStorageBytes(this.env, userId, storageBytes);
    } catch (e) {
      console.error("update storage_bytes failed", String(e));
    }
    return { pruned, storageBytes };
  }

  async missingBlobs(hashes: string[]): Promise<string[]> {
    const missing: string[] = [];
    for (const h of hashes) {
      if (this.sql.exec("SELECT 1 FROM blobs WHERE sha256=?", h).toArray().length === 0) missing.push(h);
    }
    return missing;
  }

  async storageBytes(): Promise<number> {
    return this.usageBytes();
  }

  // ---- HTTP：上传与推送（Worker 鉴权后转过来） ----

  async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const userId = request.headers.get("x-klip-user") ?? "";
    if (!userId) return errorJson(400, "missing user");
    this.sql.exec("INSERT OR IGNORE INTO meta (k,v) VALUES ('user',?)", userId);

    if (url.pathname === "/events") return this.acceptEvents(request);
    const tier: TierPolicy | null = JSON.parse(request.headers.get("x-klip-tier") ?? "null");
    if (request.method === "PUT" && !tier) return errorJson(400, "missing tier");
    if (request.method === "PUT" && tier && url.pathname === "/snapshot") {
      const run = this.snapshotLock.then(() => this.uploadSnapshot(request, userId, tier));
      this.snapshotLock = run.catch(() => {});
      return run;
    }
    const blob = url.pathname.match(/^\/blobs\/([0-9a-f]{64})$/);
    if (request.method === "PUT" && tier && blob) return this.uploadBlob(request, userId, blob[1], tier);
    return errorJson(404, "not found");
  }

  private isGzip(request: Request): boolean {
    return (request.headers.get("content-encoding") ?? "").toLowerCase().includes("gzip");
  }

  private async uploadBlob(request: Request, userId: string, sha: string, tier: TierPolicy): Promise<Response> {
    const existing = this.sql.exec<{ size: number }>("SELECT size FROM blobs WHERE sha256=?", sha).toArray()[0];
    if (existing) {
      await request.body?.cancel();
      return json({ sha256: sha, size: existing.size, existing: true });
    }
    const quota = this.quota(tier, true);
    let admitted = true;
    let result: StoreResult;
    try {
      result = await streamToR2(this.env.BUCKET, blobKey(userId, sha), request.body, this.isGzip(request), {
        expectedSha256: sha,
        admit: (size) => (admitted = quota.admit(size)),
      });
    } catch (e) {
      if (e instanceof GzipError) return errorJson(400, "invalid gzip payload");
      throw e;
    } finally {
      quota.release();
    }
    if (!admitted) return this.limitExceeded(tier);
    if (!result.stored) return errorJson(400, "blob sha256 mismatch");
    await this.withCommit(async () => {
      this.sql.exec("INSERT OR IGNORE INTO blobs (sha256,size) VALUES (?,?)", sha, result.size);
      // blob 占的空间不算备份，所以备份可能因此超额：这里也清理。
      await this.settle(userId, tier);
    });
    return json({ sha256: sha, size: result.size, existing: false });
  }

  private nextVersion(): string {
    const last = this.sql.exec<{ v: string | null }>("SELECT MAX(CAST(version AS INTEGER)) AS v FROM snapshots").one().v;
    return String(Math.max(Date.now(), last === null ? 0 : Number(last) + 1));
  }

  /**
   * 上传快照：先登记成“正在发布”，写进 R2，确定放得下，写 manifest，最后才切成当前快照。
   * 任何一步失败都撤销这份待发布的快照，当前快照、manifest、备份都不动；
   * 写完 manifest 后 DO 若被重启，recoverPublication 会把它补完。
   */
  private async uploadSnapshot(request: Request, userId: string, tier: TierPolicy): Promise<Response> {
    const version = this.nextVersion();
    const key = snapshotKey(userId, version);
    this.sql.exec("INSERT INTO snapshots (version,created_ms,size,sha256,current,r2_key) VALUES (?,?,0,'',?,?)", version, Number(version), PENDING, key);
    const discard = async () => {
      this.sql.exec("DELETE FROM snapshots WHERE version=? AND current=?", version, PENDING);
      await this.env.BUCKET.delete(key).catch(() => {});
    };

    const quota = this.quota(tier, false);
    let result: StoreResult;
    try {
      result = await streamToR2(this.env.BUCKET, key, request.body, this.isGzip(request), { admit: quota.admit });
    } catch (e) {
      await discard();
      if (e instanceof GzipError) return errorJson(400, "invalid gzip payload");
      throw e;
    } finally {
      quota.release();
    }
    if (!result.stored) {
      await discard();
      if (result.size > tier.storageLimitBytes) {
        return json(
          { code: "snapshot_too_large", error: "snapshot size exceeds tier limit", tier: tier.name, snapshotBytes: result.size, storageLimitBytes: tier.storageLimitBytes },
          413,
        );
      }
      return this.limitExceeded(tier);
    }
    this.sql.exec("UPDATE snapshots SET size=?, sha256=? WHERE version=?", result.size, result.sha256, version);

    const updatedAt = nowIso();
    const settled = await this.withCommit(async () => {
      const manifest = {
        schema: 1,
        issuer: this.env.AUTH_ISSUER,
        subject: userId,
        latest: {
          version,
          updatedAt,
          deviceId: "",
          createdFromVersion: "",
          snapshot: { type: "sqlite", fileName: "snapshot.sqlite", sha256: result.sha256, size: result.size },
        },
      };
      try {
        await this.env.BUCKET.put(manifestKey(userId), JSON.stringify(manifest), {
          httpMetadata: { contentType: "application/json; charset=utf-8" },
          customMetadata: { snapshotKey: key },
        });
      } catch (e) {
        await discard();
        throw e;
      }
      this.promote(version);
      return this.settle(userId, tier);
    });

    this.broadcast(sseEvent("sync-updated", { version, updatedAt, sha256: result.sha256, size: result.size }));

    return json({
      version,
      sha256: result.sha256,
      size: result.size,
      tier: tier.name,
      storageBytes: settled.storageBytes,
      storageLimitBytes: tier.storageLimitBytes,
      removedBackupCount: settled.pruned.count,
      removedBackupBytes: settled.pruned.bytes,
    });
  }

  /** 先删超过保留天数的备份，还超额就从最旧的开始删。当前快照和正在发布的不动。只在 withCommit 里调用。 */
  private async pruneBackups(userId: string, tier: TierPolicy): Promise<{ count: number; bytes: number }> {
    const backups = this.sql.exec<SnapshotRow>("SELECT version,created_ms,size,current,r2_key FROM snapshots WHERE current=? ORDER BY created_ms ASC", BACKUP).toArray();
    const cutoff = Date.now() - Math.abs(tier.retentionDays) * DAY_MS;
    const doomed: SnapshotRow[] = backups.filter((b) => b.created_ms < cutoff);
    let usage = this.usageBytes() - doomed.reduce((n, b) => n + b.size, 0);
    for (const b of backups) {
      if (usage <= tier.storageLimitBytes) break;
      if (doomed.includes(b)) continue;
      doomed.push(b);
      usage -= b.size;
    }
    // 分批删：R2 一次最多 1000 个；每批删成功才从账目里去掉这一批。
    const removed = { count: 0, bytes: 0 };
    for (let i = 0; i < doomed.length; i += R2_DELETE_BATCH) {
      const batch = doomed.slice(i, i + R2_DELETE_BATCH);
      await this.env.BUCKET.delete(batch.map((b) => b.r2_key ?? snapshotKey(userId, b.version)));
      for (const b of batch) {
        this.sql.exec("DELETE FROM snapshots WHERE version=?", b.version);
        removed.count++;
        removed.bytes += b.size;
      }
    }
    return removed;
  }

  // ---- 推送 ----

  private acceptEvents(request: Request): Response {
    if (request.headers.get("Upgrade")?.toLowerCase() !== "websocket") return errorJson(426, "expected websocket");
    const pair = new WebSocketPair();
    this.ctx.acceptWebSocket(pair[1]);
    return new Response(null, { status: 101, webSocket: pair[0] });
  }

  private broadcast(text: string): void {
    for (const ws of this.ctx.getWebSockets()) {
      try {
        ws.send(text);
      } catch {
        /* 对方已断开 */
      }
    }
  }

  async webSocketMessage(): Promise<void> {
    /* 客户端不发消息 */
  }

  async webSocketClose(ws: WebSocket, code: number): Promise<void> {
    try {
      ws.close(code === 1005 || code === 1006 ? 1000 : code, "bye");
    } catch {
      /* 已经关了 */
    }
  }
}
