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
  stored: boolean; // 超出大小上限或校验不符时为 false，R2 里什么也不留
}

/**
 * 把（可能是 gzip 的）请求体解压后写进 R2，同时算 sha256 和大小。
 * 请求体长度未知（客户端边压缩边发），所以大文件走分片上传。
 */
async function streamToR2(
  bucket: R2Bucket,
  key: string,
  body: ReadableStream<Uint8Array> | null,
  gzip: boolean,
  opts: { maxBytes?: number; expectedSha256?: string },
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
      if (opts.maxBytes !== undefined && size > opts.maxBytes) {
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

  const rest = takePart(pendingBytes);
  if (upload) {
    if (rest.byteLength > 0) parts.push(await upload.uploadPart(parts.length + 1, rest));
    await upload.complete(parts);
  } else {
    await bucket.put(key, rest);
  }
  return { size, sha256, stored: true };
}

type SnapshotRow = {
  version: string;
  created_ms: number;
  size: number;
  current: number;
};

export class UserStore extends DurableObject<Env> {
  private sql: SqlStorage;
  private snapshotLock: Promise<unknown> = Promise.resolve();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;
    this.sql.exec(`
      CREATE TABLE IF NOT EXISTS snapshots (
        version TEXT PRIMARY KEY,
        created_ms INTEGER NOT NULL,
        size INTEGER NOT NULL,
        sha256 TEXT NOT NULL,
        current INTEGER NOT NULL DEFAULT 0
      );
      CREATE TABLE IF NOT EXISTS blobs (
        sha256 TEXT PRIMARY KEY,
        size INTEGER NOT NULL
      );
    `);
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
    const b = this.sql.exec<{ n: number }>("SELECT COALESCE(SUM(size),0) AS n FROM blobs").one().n;
    return s + b;
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

    if (url.pathname === "/events") return this.acceptEvents(request);
    if (request.method === "PUT" && url.pathname === "/snapshot") {
      const tier: TierPolicy = JSON.parse(request.headers.get("x-klip-tier") ?? "null");
      const run = this.snapshotLock.then(() => this.uploadSnapshot(request, userId, tier));
      this.snapshotLock = run.catch(() => {});
      return run;
    }
    const blob = url.pathname.match(/^\/blobs\/([0-9a-f]{64})$/);
    if (request.method === "PUT" && blob) return this.uploadBlob(request, userId, blob[1]);
    return errorJson(404, "not found");
  }

  private isGzip(request: Request): boolean {
    return (request.headers.get("content-encoding") ?? "").toLowerCase().includes("gzip");
  }

  private async uploadBlob(request: Request, userId: string, sha: string): Promise<Response> {
    const existing = this.sql.exec<{ size: number }>("SELECT size FROM blobs WHERE sha256=?", sha).toArray()[0];
    if (existing) {
      await request.body?.cancel();
      return json({ sha256: sha, size: existing.size, existing: true });
    }
    let result: StoreResult;
    try {
      result = await streamToR2(this.env.BUCKET, blobKey(userId, sha), request.body, this.isGzip(request), { expectedSha256: sha });
    } catch (e) {
      if (e instanceof GzipError) return errorJson(400, "invalid gzip payload");
      throw e;
    }
    if (!result.stored) return errorJson(400, "blob sha256 mismatch");
    this.sql.exec("INSERT OR IGNORE INTO blobs (sha256,size) VALUES (?,?)", sha, result.size);
    await setStorageBytes(this.env, userId, this.usageBytes());
    return json({ sha256: sha, size: result.size, existing: false });
  }

  private nextVersion(): string {
    const last = this.sql.exec<{ v: string | null }>("SELECT MAX(CAST(version AS INTEGER)) AS v FROM snapshots").one().v;
    return String(Math.max(Date.now(), last === null ? 0 : Number(last) + 1));
  }

  private async uploadSnapshot(request: Request, userId: string, tier: TierPolicy): Promise<Response> {
    const version = this.nextVersion();
    const key = snapshotKey(userId, version);
    let result: StoreResult;
    try {
      result = await streamToR2(this.env.BUCKET, key, request.body, this.isGzip(request), { maxBytes: tier.storageLimitBytes });
    } catch (e) {
      if (e instanceof GzipError) return errorJson(400, "invalid gzip payload");
      throw e;
    }
    if (!result.stored) {
      return json(
        { code: "snapshot_too_large", error: "snapshot size exceeds tier limit", tier: tier.name, snapshotBytes: result.size, storageLimitBytes: tier.storageLimitBytes },
        413,
      );
    }

    const updatedAt = nowIso();
    this.sql.exec("UPDATE snapshots SET current=0 WHERE current=1");
    this.sql.exec("INSERT INTO snapshots (version,created_ms,size,sha256,current) VALUES (?,?,?,?,1)", version, Number(version), result.size, result.sha256);

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
    await this.env.BUCKET.put(manifestKey(userId), JSON.stringify(manifest), {
      httpMetadata: { contentType: "application/json; charset=utf-8" },
      customMetadata: { snapshotKey: key },
    });

    const pruned = await this.pruneBackups(userId, tier);
    const storageBytes = this.usageBytes();
    await setStorageBytes(this.env, userId, storageBytes);

    if (storageBytes > tier.storageLimitBytes) {
      return json(
        { code: "storage_limit_exceeded", error: "storage usage exceeds tier limit after pruning", tier: tier.name, storageBytes, storageLimitBytes: tier.storageLimitBytes },
        409,
      );
    }

    this.broadcast(sseEvent("sync-updated", { version, updatedAt, sha256: result.sha256, size: result.size }));

    return json({
      version,
      sha256: result.sha256,
      size: result.size,
      tier: tier.name,
      storageBytes,
      storageLimitBytes: tier.storageLimitBytes,
      removedBackupCount: pruned.count,
      removedBackupBytes: pruned.bytes,
    });
  }

  /** 先删超过保留天数的备份，还超额就从最旧的开始删。当前快照不动。 */
  private async pruneBackups(userId: string, tier: TierPolicy): Promise<{ count: number; bytes: number }> {
    const backups = this.sql.exec<SnapshotRow>("SELECT version,created_ms,size,current FROM snapshots WHERE current=0 ORDER BY created_ms ASC").toArray();
    const cutoff = Date.now() - Math.abs(tier.retentionDays) * DAY_MS;
    const doomed: SnapshotRow[] = backups.filter((b) => b.created_ms < cutoff);
    let usage = this.usageBytes() - doomed.reduce((n, b) => n + b.size, 0);
    for (const b of backups) {
      if (usage <= tier.storageLimitBytes) break;
      if (doomed.includes(b)) continue;
      doomed.push(b);
      usage -= b.size;
    }
    if (doomed.length === 0) return { count: 0, bytes: 0 };
    await this.env.BUCKET.delete(doomed.map((b) => snapshotKey(userId, b.version)));
    for (const b of doomed) this.sql.exec("DELETE FROM snapshots WHERE version=?", b.version);
    return { count: doomed.length, bytes: doomed.reduce((n, b) => n + b.size, 0) };
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
