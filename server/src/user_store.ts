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
import { getTierPolicy } from "./env";
import { hashPassword, verifyPassword } from "./crypto";
import { createPasswordCode, deletePasswordCode, generatePasswordCode, passwordCodeCooldown, setStorageBytes } from "./db";
import { buildPasswordCodeMessage, sendMail } from "./smtp";
import { DAY_MS, PASSWORD_CODE_RESEND_COOLDOWN_SECONDS, errorJson, hex, json, nowIso, safeSegment, sseEvent } from "./util";

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
  // admit(已收到的字节数)：每收到一块问一次还能不能存；返回 false 就放弃写入。
  // 放弃之后，没给 readLimit 就立刻停止解压；给了就只再解压到超过 readLimit 为止
  // （用来判断是不是单个文件就超出上限）。之后剩下的原始请求体只读不解压。
  // 停下时返回的 size 是已解压的字节数。
  opts: { admit?: (size: number) => boolean; readLimit?: number; expectedSha256?: string },
): Promise<StoreResult> {
  // 放弃之后要把请求体读完（提前回响应、不读完的话，客户端还在发就会被重置连接，收不到错误码），
  // 但不再解压：gate 把之后的原始数据直接丢掉，解压那一层什么也收不到。原始请求体最大
  // 也就 Cloudflare 的上传上限，读掉它很便宜；解压才贵（高压缩比的 gzip 能膨胀到几十 GB）。
  let draining = false;
  const gate = new TransformStream<Uint8Array, Uint8Array>({
    transform(chunk, ctl) {
      if (!draining) ctl.enqueue(chunk);
    },
  });
  let stream: ReadableStream<Uint8Array> = (body ?? new ReadableStream({ start: (c) => c.close() })).pipeThrough(gate);
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
      if (storing && opts.admit && !opts.admit(size)) await abort();
      if (!storing && (opts.readLimit === undefined || size > opts.readLimit)) {
        // 不再存了，也不必再解压：只把剩下的原始请求体读掉。
        draining = true;
        await digestWriter.abort().catch(() => {});
        for (;;) {
          try {
            if ((await reader.read()).done) break;
          } catch {
            break; // gzip 被截断会在这里报错，正常
          }
        }
        return { size, sha256: "", stored: false };
      }
      if (!storing) continue;
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
  // 正在从 R2 删除的 blob（按哈希）：同一个哈希的新上传要等删除结束再开始，
  // 免得删除落在新文件写入之后。
  private blobDeletions = new Map<string, Promise<void>>();
  // 发验证码按用途排队。
  private codeLocks = new Map<string, Promise<unknown>>();

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
      CREATE TABLE IF NOT EXISTS cleanup (
        r2_key TEXT PRIMARY KEY,
        blob_sha TEXT
      );
      CREATE TABLE IF NOT EXISTS blob_uploads (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        sha256 TEXT NOT NULL,
        r2_key TEXT NOT NULL
      );
    `);
    const cols = this.sql.exec<{ name: string }>("PRAGMA table_info(snapshots)").toArray();
    if (!cols.some((c) => c.name === "r2_key")) this.sql.exec("ALTER TABLE snapshots ADD COLUMN r2_key TEXT");
    this.sql.exec("CREATE INDEX IF NOT EXISTS idx_snapshots_current ON snapshots (current)");
    const cleanupCols = this.sql.exec<{ name: string }>("PRAGMA table_info(cleanup)").toArray();
    if (!cleanupCols.some((c) => c.name === "blob_sha")) this.sql.exec("ALTER TABLE cleanup ADD COLUMN blob_sha TEXT");
    ctx.blockConcurrencyWhile(() => this.recoverPublication());
  }

  /**
   * DO 启动时收拾上次中断留下的东西：
   * - 发布到一半的快照：看 R2 里的 manifest 指向谁，指向它就补成当前快照，否则撤销。
   * - 上传到一半的 blob（有上传意图、没有记账）：R2 里的文件交给待清理表删掉。
   * - 没写成功的 D1 用量：再写一次。
   */
  private async recoverPublication(): Promise<void> {
    const pending = this.sql.exec<SnapshotRow>("SELECT version,created_ms,size,current,r2_key FROM snapshots WHERE current=?", PENDING).toArray();
    const hasCleanup = this.sql.exec("SELECT 1 FROM cleanup LIMIT 1").toArray().length > 0;
    const hasBlobIntents = this.sql.exec("SELECT 1 FROM blob_uploads LIMIT 1").toArray().length > 0;
    const usageDirty = this.sql.exec("SELECT 1 FROM meta WHERE k='usage_dirty'").toArray().length > 0;
    if (pending.length === 0 && !hasCleanup && !hasBlobIntents && !usageDirty) return;
    const userId = this.sql.exec<{ v: string }>("SELECT v FROM meta WHERE k='user'").toArray()[0]?.v;
    if (hasBlobIntents) {
      // 启动时没有正在进行的上传：留下的意图都是中断的。已经记账的 blob 不用动。
      this.ctx.storage.transactionSync(() => {
        this.sql.exec(
          "INSERT OR IGNORE INTO cleanup (r2_key, blob_sha) SELECT r2_key, sha256 FROM blob_uploads u WHERE NOT EXISTS (SELECT 1 FROM blobs b WHERE b.sha256 = u.sha256)",
        );
        this.sql.exec("DELETE FROM blob_uploads");
      });
    }
    if (pending.length > 0 && userId) {
      try {
        await this.reconcilePending(userId);
      } catch (e) {
        console.error("reconcile pending snapshot failed, will retry on next upload", String(e));
      }
    }
    await this.drainCleanup();
    // 恢复改了账目：按用户现在的档位补做一次收尾（清理备份、更新 D1 用量）。
    if (userId) {
      const row = await this.env.DB.prepare("SELECT tier FROM users WHERE id=?").bind(userId).first<{ tier: string | null }>();
      const tier = getTierPolicy(row?.tier);
      if (tier) await this.settle(userId, tier);
      await this.flushUsage(userId); // 没有档位时也要把待写入的用量写掉
    }
  }

  /**
   * 处理遗留的待发布快照（发布中断，或写 manifest 时结果不明）：按 R2 里的 manifest 实际指向决定。
   * 指向它就补成当前快照，否则撤销。读不到 manifest 就抛出，什么都不改，留到下次再判断。
   * 调用时不能有正在进行的快照上传（DO 启动时，或拿着 snapshotLock 时）。
   */
  private async reconcilePending(userId: string): Promise<void> {
    const pending = this.sql.exec<SnapshotRow>("SELECT version,created_ms,size,current,r2_key FROM snapshots WHERE current=? ORDER BY created_ms ASC", PENDING).toArray();
    if (pending.length === 0) return;
    const head = await this.env.BUCKET.head(manifestKey(userId)); // 失败就抛出：不在不确定时删东西
    const published = head?.customMetadata?.snapshotKey;
    for (const row of pending) {
      if (row.r2_key && row.r2_key === published) this.promote(row.version);
      else this.dropPending(row.version, row.r2_key);
    }
  }

  /** 撤销一份待发布的快照：先把它的 R2 文件记进待清理表（和删记录在同一个事务里），再去删文件。 */
  private dropPending(version: string, r2Key: string | null): void {
    this.ctx.storage.transactionSync(() => {
      if (r2Key) this.sql.exec("INSERT OR IGNORE INTO cleanup (r2_key) VALUES (?)", r2Key);
      this.sql.exec("DELETE FROM snapshots WHERE version=? AND current=?", version, PENDING);
    });
  }

  /**
   * 删掉待清理表里的 R2 文件；删成功才去掉记录，失败的留到下次（每次提交和 DO 启动时都会再试）。
   * blob 的 key 由哈希决定，同一个 blob 可能已经被重新传上来：已经记账的不删（只去掉记录），
   * 正在上传的先跳过，免得删掉刚写进去的文件。
   */
  // 只能在提交锁（withCommit）里或 DO 启动恢复（blockConcurrencyWhile）时调用：
  // 两次清理不能重叠，否则同一个哈希的删除登记会互相覆盖。
  private async drainCleanup(): Promise<void> {
    this.sql.exec("DELETE FROM cleanup WHERE blob_sha IS NOT NULL AND EXISTS (SELECT 1 FROM blobs b WHERE b.sha256 = cleanup.blob_sha)");
    // 快照文件同理：还被某条快照记录（当前、备份或待发布）用着的 key 不删。
    this.sql.exec("DELETE FROM cleanup WHERE blob_sha IS NULL AND EXISTS (SELECT 1 FROM snapshots s WHERE s.r2_key = cleanup.r2_key)");
    const rows = this.sql
      .exec<{ r2_key: string; blob_sha: string | null }>(
        "SELECT r2_key, blob_sha FROM cleanup c WHERE c.blob_sha IS NULL OR NOT EXISTS (SELECT 1 FROM blob_uploads u WHERE u.sha256 = c.blob_sha) LIMIT ?",
        R2_DELETE_BATCH,
      )
      .toArray();
    if (rows.length === 0) return;
    const keys = rows.map((r) => r.r2_key);
    // 查询和登记之间没有 await：登记之后，同一哈希的新上传都会等这次删除结束。
    const deletion = this.env.BUCKET.delete(keys);
    const settled = deletion.then(() => {}, () => {});
    const shas = rows.map((r) => r.blob_sha).filter((x): x is string => x !== null);
    for (const sha of shas) this.blobDeletions.set(sha, settled);
    try {
      await deletion;
      for (const k of keys) this.sql.exec("DELETE FROM cleanup WHERE r2_key=?", k);
      // 一次最多 1000 个：还有剩的（或刚才因为正在上传跳过的）就再安排一次。
      if (this.sql.exec("SELECT 1 FROM cleanup LIMIT 1").toArray().length > 0) await this.scheduleMaintenance(60_000);
    } catch (e) {
      console.error("r2 cleanup failed, will retry", String(e));
      await this.scheduleMaintenance(60_000);
    } finally {
      for (const sha of shas) if (this.blobDeletions.get(sha) === settled) this.blobDeletions.delete(sha);
    }
  }

  /** 账目变了、D1 里的用量需要重写。和改账目放在同一个事务里调用，重启也不会丢。 */
  private markUsageDirty(): void {
    this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('usage_dirty','1')");
  }

  /** 把一份待发布的快照变成当前快照，原来的当前快照变成备份。 */
  private promote(version: string): void {
    this.ctx.storage.transactionSync(() => {
      this.markUsageDirty();
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

  /**
   * 发验证码：冷却检查、生成、发信在这里按用途一个个来（每个用户一个 DO），
   * 所以同时来的两个请求不会都通过冷却检查，也不会出现后生成的码先送达。
   * 发信失败就删掉这个码并抛出。TLS 握手也耗 CPU，放在 DO 里正好。
   */
  async issuePasswordCode(email: string, purpose: string): Promise<{ sent: boolean; cooldownSeconds: number }> {
    const run = (this.codeLocks.get(purpose) ?? Promise.resolve()).then(async () => {
      const cooldown = await passwordCodeCooldown(this.env, email, purpose);
      if (cooldown > 0) return { sent: false, cooldownSeconds: cooldown };
      const code = generatePasswordCode();
      const id = await createPasswordCode(this.env, email, purpose, code);
      try {
        await sendMail(this.env, email, buildPasswordCodeMessage(this.env, email, code, purpose));
      } catch (e) {
        await deletePasswordCode(this.env, id);
        throw e;
      }
      return { sent: true, cooldownSeconds: PASSWORD_CODE_RESEND_COOLDOWN_SECONDS };
    });
    this.codeLocks.set(purpose, run.catch(() => {}));
    return run;
  }

  // ---- 账目 ----

  /** 已提交的用量：当前快照、备份和 blob。正在发布的快照不算，它还可能被撤销。 */
  private usageBytes(): number {
    const s = this.sql.exec<{ n: number }>("SELECT COALESCE(SUM(size),0) AS n FROM snapshots WHERE current IN (?,?)", BACKUP, CURRENT).one().n;
    return s + this.blobBytes();
  }

  /**
   * 注意：blob 记上账之后不会被自动删除（见 resetStorage 的说明）。
   * blob 总字节数：存在 meta 里的计数，和记 blob 在同一个事务里更新。
   * 上传时每收到一块数据都要查一次，不能每次都对整张 blobs 表求和。
   */
  private blobBytes(): number {
    const row = this.sql.exec<{ v: string }>("SELECT v FROM meta WHERE k='blob_bytes'").toArray()[0];
    if (row) return Number(row.v);
    // 第一次用（或旧数据）：算一次存起来。
    const n = this.sql.exec<{ n: number }>("SELECT COALESCE(SUM(size),0) AS n FROM blobs").one().n;
    this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('blob_bytes',?)", String(n));
    return n;
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
    await this.drainCleanup();
    let pruned = { count: 0, bytes: 0 };
    try {
      pruned = await this.pruneBackups(userId, tier);
    } catch (e) {
      console.error("prune backups failed", String(e));
    }
    await this.flushUsage(userId);
    return { pruned, storageBytes: this.usageBytes() };
  }

  /**
   * 把用量写到 D1。先记下“待写入”，写成功才清掉；失败就用 alarm 一分钟后再试，
   * DO 重启时也会再试，所以一次失败不会让 D1 里的用量一直停在旧值。只在提交里调用。
   */
  private async flushUsage(userId: string): Promise<void> {
    if (this.sql.exec("SELECT 1 FROM meta WHERE k='usage_dirty'").toArray().length === 0) return;
    try {
      await setStorageBytes(this.env, userId, this.usageBytes());
      this.sql.exec("DELETE FROM meta WHERE k='usage_dirty'");
    } catch (e) {
      console.error("update storage_bytes failed, will retry", String(e));
      await this.scheduleMaintenance(60_000);
    }
  }

  /** 过一会儿（默认一秒）用 alarm 在提交锁里做一次清理和用量补写（已经有更早的 alarm 就不动）。 */
  private async scheduleMaintenance(delayMs = 1_000): Promise<void> {
    const at = Date.now() + delayMs;
    const current = await this.ctx.storage.getAlarm();
    if (current === null || current > at) await this.ctx.storage.setAlarm(at);
  }

  async alarm(): Promise<void> {
    const userId = this.sql.exec<{ v: string }>("SELECT v FROM meta WHERE k='user'").toArray()[0]?.v;
    if (!userId) return;
    await this.withCommit(async () => {
      await this.drainCleanup();
      await this.flushUsage(userId);
    });
  }

  /**
   * 清空这个用户的云端数据（后台操作）：删掉 R2 里他名下的全部文件、清空账目、D1 用量写成 0，
   * 再推送一次更新。客户端下次同步时看到 manifest 不存在，会把本地的快照和 blob 全部重新上传。
   *
   * 为什么需要它：blob 记上账之后不会被自动回收（服务端看不懂快照内容，不知道哪些 blob 还被引用），
   * 删了历史也不会释放空间，占满档位后就同步不了。这里是给管理员的补救办法。
   * 拿着快照锁和提交锁执行；正在上传的 blob 的文件不删，它们的提交会照常记账。
   */
  async resetStorage(userId: string): Promise<{ removedObjects: number }> {
    const run = this.snapshotLock.then(() =>
      this.withCommit(async () => {
        let removedObjects = 0;
        let cursor: string | undefined;
        do {
          const page = await this.env.BUCKET.list({ prefix: userPrefix(userId), cursor, limit: R2_DELETE_BATCH });
          const inFlight = new Set(this.sql.exec<{ r2_key: string }>("SELECT r2_key FROM blob_uploads").toArray().map((r) => r.r2_key));
          const keys = page.objects.map((o) => o.key).filter((k) => !inFlight.has(k));
          if (keys.length > 0) {
            await this.env.BUCKET.delete(keys);
            removedObjects += keys.length;
          }
          cursor = page.truncated ? page.cursor : undefined;
        } while (cursor);
        this.ctx.storage.transactionSync(() => {
          this.sql.exec("DELETE FROM snapshots");
          this.sql.exec("DELETE FROM blobs");
          this.sql.exec("DELETE FROM cleanup");
          this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('blob_bytes','0')");
          this.markUsageDirty();
        });
        await this.flushUsage(userId);
        this.broadcast(sseEvent("sync-updated", { reset: true, updatedAt: nowIso() }));
        return { removedObjects };
      }),
    );
    this.snapshotLock = run.catch(() => {});
    return run;
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
    // 同一个哈希的文件正在被清理就先等删完；等完之后到登记上传意图之间没有 await，
    // 之后的清理会因为这条意图而跳过它。
    for (let pending = this.blobDeletions.get(sha); pending; pending = this.blobDeletions.get(sha)) await pending;
    const existing = this.sql.exec<{ size: number }>("SELECT size FROM blobs WHERE sha256=?", sha).toArray()[0];
    if (existing) {
      await request.body?.cancel();
      return json({ sha256: sha, size: existing.size, existing: true });
    }
    // 额度一直占着，直到 blob 在提交里记进账目才释放：排队等提交的这段时间里，
    // 别的上传也看得到这部分空间已经被占用。
    // 写 R2 之前先记下上传意图：上传中途 DO 重启的话，恢复时能找到并删掉没记账的文件。
    const key = blobKey(userId, sha);
    const intent = this.sql.exec<{ id: number }>("INSERT INTO blob_uploads (sha256, r2_key) VALUES (?,?) RETURNING id", sha, key).one().id;
    const quota = this.quota(tier, true);
    let committed = false;
    try {
      let admitted = true;
      let result: StoreResult;
      try {
        result = await streamToR2(this.env.BUCKET, key, request.body, this.isGzip(request), {
          expectedSha256: sha,
          admit: (size) => (admitted = quota.admit(size)),
        });
      } catch (e) {
        if (e instanceof GzipError) return errorJson(400, "invalid gzip payload");
        throw e;
      }
      if (!admitted) return this.limitExceeded(tier);
      if (!result.stored) return errorJson(400, "blob sha256 mismatch");
      await this.withCommit(async () => {
        this.ctx.storage.transactionSync(() => {
          const before = this.blobBytes();
          const inserted = this.sql.exec("INSERT OR IGNORE INTO blobs (sha256,size) VALUES (?,?) RETURNING size", sha, result.size).toArray().length > 0;
          if (inserted) this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('blob_bytes',?)", String(before + result.size));
          this.markUsageDirty();
          this.sql.exec("DELETE FROM blob_uploads WHERE id=?", intent);
          this.sql.exec("DELETE FROM cleanup WHERE r2_key=?", key);
        });
        committed = true;
        quota.release();
        // blob 占的空间不算备份，所以备份可能因此超额：这里也清理。
        await this.settle(userId, tier);
      });
      return json({ sha256: sha, size: result.size, existing: false });
    } finally {
      quota.release();
      if (!committed) {
        // 没记上账：R2 里可能有文件也可能没有（写入失败时也可能其实写进去了），
        // 一律交给待清理表（和删意图在同一个事务里）。删一个不存在的 key 没有副作用；
        // 同一个哈希已经记账或正在上传的，drainCleanup 不会删它。
        this.ctx.storage.transactionSync(() => {
          this.sql.exec("INSERT OR IGNORE INTO cleanup (r2_key, blob_sha) VALUES (?,?)", key, sha);
          this.sql.exec("DELETE FROM blob_uploads WHERE id=?", intent);
        });
        // 不依赖下一次请求：用 alarm 安排清理。
        await this.scheduleMaintenance();
      }
    }
  }

  /**
   * 分配快照版本号（也是 R2 key 的一部分）。最后分配过的号存在 meta 里，失败撤销的也算，
   * 所以版本号只增不减，新上传不会用到一个还在等清理的旧 key。
   */
  private nextVersion(): string {
    const maxRow = this.sql.exec<{ v: number | null }>("SELECT MAX(CAST(version AS INTEGER)) AS v FROM snapshots").one().v ?? 0;
    const lastAllocated = Number(this.sql.exec<{ v: string }>("SELECT v FROM meta WHERE k='last_version'").toArray()[0]?.v ?? 0);
    const version = Math.max(Date.now(), maxRow + 1, lastAllocated + 1);
    this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('last_version',?)", String(version));
    return String(version);
  }

  /**
   * 上传快照：先登记成“正在发布”，写进 R2，确定放得下，写 manifest，最后才切成当前快照。
   * 任何一步失败都撤销这份待发布的快照，当前快照、manifest、备份都不动；
   * 写 manifest 的结果不明时先回读核对，读不到就保留待发布状态，留给下次上传或 DO 重启时的 reconcilePending。
   */
  private async uploadSnapshot(request: Request, userId: string, tier: TierPolicy): Promise<Response> {
    // 先处理上次留下的、结果不明的发布（拿着 snapshotLock，没有别的快照在上传）。
    await this.withCommit(() => this.reconcilePending(userId));
    const version = this.nextVersion();
    const key = snapshotKey(userId, version);
    this.sql.exec("INSERT INTO snapshots (version,created_ms,size,sha256,current,r2_key) VALUES (?,?,0,'',?,?)", version, Number(version), PENDING, key);
    // 撤销只登记待清理（同步、和删记录同一事务），真正删 R2 留给提交锁里的 drainCleanup：
    // 所有清理都排在同一条队列上，同一个哈希不会有两次删除同时在进行。
    const discard = async () => {
      this.dropPending(version, key);
      await this.scheduleMaintenance();
    };

    const quota = this.quota(tier, false);
    let result: StoreResult;
    try {
      // 放不下就不再写；再读到超过档位上限为止，用来区分“单个快照就太大”（413）和“总量超额”（409）。
      result = await streamToR2(this.env.BUCKET, key, request.body, this.isGzip(request), { admit: quota.admit, readLimit: tier.storageLimitBytes });
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
        // 写入失败也可能其实已经写进去了（只是确认没回来）：回读 manifest 核对。
        let published: string | undefined;
        try {
          published = (await this.env.BUCKET.head(manifestKey(userId)))?.customMetadata?.snapshotKey;
        } catch {
          // 结果不明：保留这份待发布的快照，下次上传或 DO 重启时再按 manifest 判断。
          throw e;
        }
        if (published !== key) {
          await discard();
          throw e;
        }
        // manifest 已经指向它：当作发布成功，继续。
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
      this.ctx.storage.transactionSync(() => {
        for (const b of batch) this.sql.exec("DELETE FROM snapshots WHERE version=?", b.version);
        this.markUsageDirty();
      });
      for (const b of batch) {
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
