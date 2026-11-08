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
import { ACCESS_TOKEN_HOURS, DAY_MS, PASSWORD_CODE_RESEND_COOLDOWN_SECONDS, errorJson, hex, json, nowIso, safeSegment, sseEvent } from "./util";

const PART_SIZE = 8 * 1024 * 1024; // R2 分片上传：除最后一片外每片一样大
// R2 对同一个 key 每秒最多写一次。manifest 写入之间至少隔这么久，失败后按它的倍数退避。
const MANIFEST_WRITE_INTERVAL_MS = 1100;
const MANIFEST_WRITE_ATTEMPTS = 4;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

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

// ---- blob 回收 ----
// 客户端查缺失 blob（POST /sync/blobs/check）时交来的哈希列表，就是它接下来要上传的快照引用的全部 blob。
// 服务端把这份列表记成一条“引用声明”（ref_claims），把 token 返回给客户端；客户端上传快照时
// 用请求头 x-klip-blob-refs 带上这个 token（快照里没有 blob 时带 "none"），服务端就知道这份快照引用了哪些 blob。
// 不带这个头的快照（旧客户端）“引用未知”：只要还有这样的快照被保留着，就一个 blob 也不回收。
//
// 哈希列表按内容存一份（ref_sets：排好序的 32 字节哈希拼在一起，id 是它的 sha256），
// 快照和声明只记 id：每次同步只写一两行，内容没变的快照共用同一份。免费版 DO 每天写入行数有限。

/** 声明的有效期：查完缺失到上传完快照必须在这之内，过期了上传快照会被拒绝（409 blob_refs_expired），客户端重来一遍。 */
const REF_CLAIM_TTL_MS = DAY_MS;
/** 新上传的 blob 至少保留这么久才可能被回收：旧客户端先传 blob、后传快照，中间没有声明保护。 */
const DEFAULT_BLOB_GC_GRACE_MS = DAY_MS;
/** 引用变少之后过这么久才回收：把一段时间里的多次变化攒成一次（每次回收要扫一遍 blobs 表）。 */
const DEFAULT_BLOB_GC_DELAY_MS = 10 * 60_000;
/** 快照记录删掉之后，它独占的 blob 再保留这么久：给正在下载那份快照的设备留时间取完 blob。 */
const DEFAULT_BLOB_GC_DROP_HOLD_MS = DAY_MS;
/** 一个账号同时保留的声明最多这么多条、这么多字节（声明自己的哈希列表，不含快照也在用的）。 */
const MAX_REF_CLAIMS = 200;
const MAX_REF_CLAIM_BYTES = 8 * 1024 * 1024;
/** 一次查缺失最多这么多个哈希（8 MiB 的声明上限能装下）。Worker 据此拒绝更长的列表。 */
export const MAX_CHECK_HASHES = 250_000;
/**
 * 一份声明最多这么多个哈希（ref_sets 一行最多 2MB）。超过就不收成一份，只标记为“超长”：token 照给，
 * 上传快照时照样核对存储代数，但这份快照按“引用未知”记账（分批记下的列表照样保护这些 blob 到声明用掉或过期）。
 */
export const MAX_DECLARED_REFS = 60_000;
/**
 * caller_checks 保留多久：比访问令牌的有效期长一点。令牌过期后，这个调用方的上传过不了鉴权；
 * 旧客户端刷新令牌后会重新查缺失，那是一个新的调用方。
 */
const CALLER_CHECK_KEEP_MS = (ACCESS_TOKEN_HOURS + 1) * 3_600_000;
/** 快照“引用已知、没有 blob”时 snapshots.refs 的值。 */
const NO_REFS = "";

function encodeRefs(hashes: string[]): Uint8Array {
  const sorted = [...new Set(hashes)].sort();
  const out = new Uint8Array(sorted.length * 32);
  sorted.forEach((h, i) => {
    for (let j = 0; j < 32; j++) out[i * 32 + j] = parseInt(h.slice(j * 2, j * 2 + 2), 16);
  });
  return out;
}

function decodeRefs(buf: ArrayBuffer, into: Set<string>): void {
  const bytes = new Uint8Array(buf);
  for (let off = 0; off + 32 <= bytes.length; off += 32) into.add(hex(bytes.subarray(off, off + 32)));
}

/** 把请求体读完（不解析），再回错误：提前回响应的话，还在发送的客户端会被重置连接、收不到错误码。 */
async function drainBody(request: Request): Promise<void> {
  const reader = request.body?.getReader();
  if (!reader) return;
  try {
    while (!(await reader.read()).done) {
      /* 丢掉 */
    }
  } catch {
    /* 对方断开也无所谓 */
  }
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
  refs: string | null; // 引用的 blob：ref_sets 的 id；NO_REFS = 没有 blob；null = 未知（旧客户端）
};

const SNAPSHOT_COLS = "version,created_ms,size,current,r2_key,refs";

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
  // 正在上传的 blob（按哈希）：同一个哈希的上传排队，见 uploadBlob。
  private blobUploadLocks = new Map<string, Promise<unknown>>();
  // 发验证码按用途排队。
  private codeLocks = new Map<string, Promise<unknown>>();
  // 上次写 manifest 的时间：R2 同一个 key 每秒最多写一次，连续发布时先等够间隔。
  private lastManifestWrite = 0;

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
      CREATE TABLE IF NOT EXISTS ref_sets (
        id TEXT PRIMARY KEY,
        hashes BLOB NOT NULL
      );
      CREATE TABLE IF NOT EXISTS ref_claims (
        token TEXT PRIMARY KEY,
        set_id TEXT,
        created_ms INTEGER NOT NULL,
        generation INTEGER NOT NULL,
        oversized INTEGER NOT NULL DEFAULT 0,
        caller TEXT NOT NULL DEFAULT ''
      );
      CREATE TABLE IF NOT EXISTS caller_checks (
        caller TEXT PRIMARY KEY,
        generation INTEGER NOT NULL,
        checked_ms INTEGER NOT NULL,
        invalidated INTEGER NOT NULL DEFAULT 0
      );
      CREATE TABLE IF NOT EXISTS ref_claim_chunks (
        token TEXT NOT NULL,
        seq INTEGER NOT NULL,
        hashes BLOB NOT NULL,
        PRIMARY KEY (token, seq)
      );
    `);
    const cols = this.sql.exec<{ name: string }>("PRAGMA table_info(snapshots)").toArray();
    if (!cols.some((c) => c.name === "r2_key")) this.sql.exec("ALTER TABLE snapshots ADD COLUMN r2_key TEXT");
    // 加这一列之前的快照都是 NULL，即“引用未知”。
    if (!cols.some((c) => c.name === "refs")) this.sql.exec("ALTER TABLE snapshots ADD COLUMN refs TEXT");
    // 加这一列之前记账的 blob 是 NULL：按 blob_epoch_ms（第一次跑到这里的时间）算它的上传时间，宽限期从那时算起。
    const blobCols = this.sql.exec<{ name: string }>("PRAGMA table_info(blobs)").toArray();
    if (!blobCols.some((c) => c.name === "created_ms")) {
      this.sql.exec("ALTER TABLE blobs ADD COLUMN created_ms INTEGER");
      // 存量 blob 里可能有从来没被快照用到的（上传失败留下的）：宽限期过后安排一次回收，不然没有别的事会触发它。
      if (this.sql.exec("SELECT 1 FROM blobs LIMIT 1").toArray().length > 0) this.markGcDue(Date.now() + this.gcGraceMs());
    }
    this.sql.exec("INSERT OR IGNORE INTO meta (k,v) VALUES ('blob_epoch_ms',?)", String(Date.now()));
    // 从这时起每台设备的查缺失都记在 caller_checks 里（见 legacyUploadAllowed）。
    this.sql.exec("INSERT OR IGNORE INTO meta (k,v) VALUES ('caller_tracking_ms',?)", String(Date.now()));
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
   * - 做到一半的清空（见 resetStorage）：先把它做完，再处理别的。
   */
  private async recoverPublication(): Promise<void> {
    await this.scheduleGc(); // 迁移时安排的回收、或者上次没来得及安排的
    if (this.sql.exec("SELECT 1 FROM meta WHERE k='reset_pending'").toArray().length > 0) {
      const resetUser = this.sql.exec<{ v: string }>("SELECT v FROM meta WHERE k='user'").toArray()[0]?.v;
      try {
        if (resetUser) await this.finishReset(resetUser);
      } catch (e) {
        // 留到下一次快照上传开始前（uploadSnapshot 先调用 finishReset）。
        console.error("finish storage reset failed, will retry before the next upload", String(e));
      }
    }
    const pending = this.sql.exec<SnapshotRow>(`SELECT ${SNAPSHOT_COLS} FROM snapshots WHERE current=?`, PENDING).toArray();
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
    const pending = this.sql.exec<SnapshotRow>(`SELECT ${SNAPSHOT_COLS} FROM snapshots WHERE current=? ORDER BY created_ms ASC`, PENDING).toArray();
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
      const row = this.sql.exec<{ refs: string | null }>("DELETE FROM snapshots WHERE version=? AND current=? RETURNING refs", version, PENDING).toArray()[0];
      if (row) this.refsDropped(row.refs);
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

  // ---- blob 回收 ----

  private envMs(
    name: "BLOB_GC_GRACE_SECONDS" | "BLOB_GC_DELAY_SECONDS" | "BLOB_GC_DROP_HOLD_SECONDS" | "BACKUP_RETENTION_SECONDS",
    fallback: number,
  ): number {
    const raw = this.env[name];
    if (raw === undefined || raw.trim() === "") return fallback;
    const n = Number(raw);
    return Number.isFinite(n) && n >= 0 ? n * 1000 : fallback;
  }

  private gcGraceMs(): number {
    return this.envMs("BLOB_GC_GRACE_SECONDS", DEFAULT_BLOB_GC_GRACE_MS);
  }

  /**
   * 存储代数：每次清空云端数据、每次回收已记账的 blob 都加一（和删除在同一个事务里）。
   * 查缺失时的声明记下当时的代数；上传快照时代数已经变了，说明查完之后有 blob 被删过，
   * 那次“已经有了”的结论可能不再成立，拒绝上传（409 storage_generation_stale），客户端重查、补传再传。
   * 不带声明的上传（旧客户端）不看代数，见 legacyUploadAllowed。
   */
  private storageGeneration(): number {
    return Number(this.sql.exec<{ v: string }>("SELECT v FROM meta WHERE k='storage_generation'").toArray()[0]?.v ?? 0);
  }

  /** 代数加一。在删除已记账的 blob（或清空）的同一个事务里、删除之前调用。 */
  private bumpGeneration(): void {
    this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('storage_generation',?)", String(this.storageGeneration() + 1));
  }

  /** 下一次该检查回收的时间（存在 meta 里，重启不丢）；null = 不需要。 */
  private gcDue(): number | null {
    const v = this.sql.exec<{ v: string }>("SELECT v FROM meta WHERE k='gc_due_ms'").toArray()[0]?.v;
    return v === undefined ? null : Number(v);
  }

  /** 安排在 at 之前检查一次回收（已经安排了更早的就不动）。 */
  private markGcDue(at: number): void {
    const due = this.gcDue();
    if (due === null || at < due) this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('gc_due_ms',?)", String(at));
  }

  /**
   * 一条快照记录删掉之后调用（和删除在同一个事务里）：它引用的 blob 如果别的快照都不再引用，
   * 就安排一次回收。删掉的是“引用未知”的快照、而且这是最后一份时，回收从此可以进行，也安排一次。
   */
  private refsDropped(refs: string | null): void {
    if (refs === NO_REFS) return;
    const now = Date.now();
    // 刚删掉的快照可能还有设备在下载（先下快照，再一个个取 blob）：它的 blob 再留一阵（DROP_HOLD）。
    const hold = this.envMs("BLOB_GC_DROP_HOLD_SECONDS", DEFAULT_BLOB_GC_DROP_HOLD_MS);
    if (refs === null) {
      if (this.sql.exec("SELECT 1 FROM snapshots WHERE refs IS NULL LIMIT 1").toArray().length > 0) return;
      // 引用未知的快照不知道用了哪些 blob：整体推迟回收。
      if (hold > 0) this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('gc_hold_until_ms',?)", String(now + hold));
    } else {
      // 还在声明里的不算：声明会过期，回收时会按声明的到期时间再安排。
      if (this.sql.exec("SELECT 1 FROM snapshots WHERE refs=? LIMIT 1", refs).toArray().length > 0) return;
      // 记成一条“保留”声明（不发给任何客户端），到期时间是 now + hold。
      if (hold > 0) {
        this.sql.exec(
          "INSERT INTO ref_claims (token, set_id, created_ms, generation) VALUES (?,?,?,?)",
          "hold:" + hex(crypto.getRandomValues(new Uint8Array(16))),
          refs,
          now + Math.min(hold, REF_CLAIM_TTL_MS) - REF_CLAIM_TTL_MS,
          this.storageGeneration(),
        );
      }
    }
    this.markGcDue(now + this.envMs("BLOB_GC_DELAY_SECONDS", DEFAULT_BLOB_GC_DELAY_MS));
  }

  /** 删掉过期的声明（连同还没收齐的分批列表），以及不再被任何快照或声明用到的哈希列表。在事务里调用。 */
  private pruneRefClaims(now: number): void {
    this.sql.exec("DELETE FROM ref_claims WHERE created_ms <= ?", now - REF_CLAIM_TTL_MS);
    this.dropOrphanRefs();
  }

  private dropOrphanRefs(): void {
    this.sql.exec("DELETE FROM ref_claim_chunks WHERE NOT EXISTS (SELECT 1 FROM ref_claims c WHERE c.token = ref_claim_chunks.token)");
    this.sql.exec(
      "DELETE FROM ref_sets WHERE NOT EXISTS (SELECT 1 FROM snapshots s WHERE s.refs = ref_sets.id) AND NOT EXISTS (SELECT 1 FROM ref_claims c WHERE c.set_id = ref_sets.id)",
    );
  }

  /**
   * 给声明占的空间设上限（条数、字节数）：声明不计入档位容量，不设限的话反复查一堆不存在的哈希就能
   * 无限写入元数据。超出时从最旧的声明开始作废，keep（正在记的这一条）不动。在事务里调用。
   * 作废的声明不再保护它的 blob；还在用它的新客户端上传快照时会收到 blob_refs_expired，重查一遍；
   * 旧客户端（不带声明上传）的检查记录标成作废，它的上传会被拒一次（见 legacyUploadAllowed）。
   * 为下载保留的声明（hold:）不算在内、也不作废：它们随快照记录的删除产生，数量跟着快照走。
   */
  private boundRefClaims(keep: string): void {
    const rows = this.sql
      .exec<{ token: string; set_id: string | null; chunk_bytes: number; set_bytes: number; caller: string; created_ms: number }>(
        `SELECT c.token AS token, c.set_id AS set_id, c.caller AS caller, c.created_ms AS created_ms,
           COALESCE((SELECT SUM(length(k.hashes)) FROM ref_claim_chunks k WHERE k.token = c.token), 0) AS chunk_bytes,
           CASE WHEN c.set_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM snapshots s WHERE s.refs = c.set_id)
             THEN COALESCE((SELECT length(r.hashes) FROM ref_sets r WHERE r.id = c.set_id), 0) ELSE 0 END AS set_bytes
         FROM ref_claims c WHERE c.token NOT LIKE 'hold:%' ORDER BY (c.token = ?) DESC, c.created_ms DESC`,
        keep,
      )
      .toArray();
    let count = 0;
    let bytes = 0;
    const counted = new Set<string>();
    const evict: { token: string; caller: string; created_ms: number }[] = [];
    for (const r of rows) {
      const size = r.chunk_bytes + (r.set_id && !counted.has(r.set_id) ? r.set_bytes : 0);
      if (r.token !== keep && (count + 1 > MAX_REF_CLAIMS || bytes + size > MAX_REF_CLAIM_BYTES)) {
        evict.push(r);
        continue;
      }
      count++;
      bytes += size;
      if (r.set_id) counted.add(r.set_id);
    }
    if (evict.length === 0) return;
    for (const e of evict) {
      this.sql.exec("DELETE FROM ref_claims WHERE token=?", e.token);
      // 这是那台设备最近一次检查的话，它的检查不再有保护：记下作废。
      if (e.caller) this.sql.exec("UPDATE caller_checks SET invalidated=1 WHERE caller=? AND checked_ms <= ?", e.caller, e.created_ms);
    }
    this.dropOrphanRefs();
    console.warn("ref claims over limit, evicted the oldest", { evicted: evict.length });
  }

  /**
   * 回收不再被引用的 blob：只是把它们从账目里去掉、记进待清理表（同一个事务里减掉 blob_bytes、标记用量要重写），
   * R2 里的文件由随后的 drainCleanup 删。到了 gc_due_ms 才做；同步执行，中间没有 await。
   * 只能在提交锁（withCommit）里或 DO 启动恢复时调用，和 drainCleanup 一样。
   *
   * 一个 blob 被回收需要同时满足：
   * - 有当前快照，并且没有任何“引用未知”的快照（当前、备份、正在发布的都算）；否则整体不回收，等以后再看；
   * - 不被任何保留的快照引用，也不被未过期的声明引用（正在同步的设备查完缺失、还没传完快照）；
   * - 没有正在进行的上传；
   * - 上传已经超过宽限期。
   */
  private collectGarbage(userId: string): void {
    const now = Date.now();
    const due = this.gcDue();
    if (due === null || due > now) return;
    // 不满足前提就保持“到期”，之后每次提交再看（只是几次很便宜的查询）；能回收时一定会有快照记录被删，也会重新安排
    // （整体推迟的，到时间由 scheduleGc 安排的 alarm 来做）。
    if (this.gcBlocked()) return;

    const kept = new Set<string>();
    const wanted = this.sql.exec<{ n: number }>("SELECT COUNT(DISTINCT refs) AS n FROM snapshots WHERE refs != ?", NO_REFS).one().n;
    let found = 0;
    for (const r of this.sql.exec<{ hashes: ArrayBuffer }>("SELECT hashes FROM ref_sets WHERE id IN (SELECT refs FROM snapshots)")) {
      decodeRefs(r.hashes, kept);
      found++;
    }
    if (found !== wanted) {
      // 有快照的哈希列表找不到：不该发生。宁可不回收，一小时后再看（不让 alarm 空转）。
      console.error("blob gc: a snapshot's ref set is missing, skipping", { wanted, found });
      this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('gc_due_ms',?)", String(now + 3_600_000));
      return;
    }
    const claimed = new Set<string>();
    let claimExpiry = Infinity;
    for (const r of this.sql.exec<{ hashes: ArrayBuffer; created_ms: number }>(
      "SELECT s.hashes AS hashes, MIN(c.created_ms) AS created_ms FROM ref_claims c JOIN ref_sets s ON s.id = c.set_id WHERE c.created_ms > ? GROUP BY c.set_id",
      now - REF_CLAIM_TTL_MS,
    )) {
      decodeRefs(r.hashes, claimed);
      claimExpiry = Math.min(claimExpiry, r.created_ms + REF_CLAIM_TTL_MS);
    }
    // 还在分批记录中的声明（查缺失的请求还没结束）。
    for (const r of this.sql.exec<{ hashes: ArrayBuffer; created_ms: number }>(
      "SELECT k.hashes AS hashes, c.created_ms AS created_ms FROM ref_claim_chunks k JOIN ref_claims c ON c.token = k.token WHERE c.created_ms > ?",
      now - REF_CLAIM_TTL_MS,
    )) {
      decodeRefs(r.hashes, claimed);
      claimExpiry = Math.min(claimExpiry, r.created_ms + REF_CLAIM_TTL_MS);
    }

    const grace = this.gcGraceMs();
    const epoch = Number(this.sql.exec<{ v: string }>("SELECT v FROM meta WHERE k='blob_epoch_ms'").one().v);
    let next = Infinity;
    const doomed: { sha256: string; size: number }[] = [];
    for (const b of this.sql.exec<{ sha256: string; size: number; created_ms: number }>(
      "SELECT sha256, size, COALESCE(created_ms, ?) AS created_ms FROM blobs b WHERE NOT EXISTS (SELECT 1 FROM blob_uploads u WHERE u.sha256 = b.sha256)",
      epoch,
    )) {
      if (kept.has(b.sha256)) continue;
      if (claimed.has(b.sha256)) {
        next = Math.min(next, claimExpiry);
        continue;
      }
      if (b.created_ms > now - grace) {
        next = Math.min(next, b.created_ms + grace);
        continue;
      }
      doomed.push({ sha256: b.sha256, size: b.size });
    }

    this.ctx.storage.transactionSync(() => {
      this.pruneRefClaims(now);
      if (doomed.length > 0) {
        this.bumpGeneration(); // 先加代数：之前查过缺失、还没传快照的设备会被要求重查
        let freed = 0;
        for (const b of doomed) {
          this.sql.exec("DELETE FROM blobs WHERE sha256=?", b.sha256);
          this.sql.exec("INSERT OR IGNORE INTO cleanup (r2_key, blob_sha) VALUES (?,?)", blobKey(userId, b.sha256), b.sha256);
          freed += b.size;
        }
        this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('blob_bytes',?)", String(Math.max(0, this.blobBytes() - freed)));
        this.markUsageDirty();
      }
      if (Number.isFinite(next)) this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('gc_due_ms',?)", String(next));
      else this.sql.exec("DELETE FROM meta WHERE k='gc_due_ms'");
    });
    if (doomed.length > 0) console.log("blob gc", { removed: doomed.length, bytes: doomed.reduce((n, b) => n + b.size, 0) });
  }

  /** 回收的前提不满足：没有当前快照，或者还保留着“引用未知”的快照。这时到期了也不回收，等快照记录被删时再看。 */
  private gcBlocked(): boolean {
    if (this.sql.exec("SELECT 1 FROM snapshots WHERE current=? LIMIT 1", CURRENT).toArray().length === 0) return true;
    if (this.gcHoldUntil() > Date.now()) return true;
    return this.sql.exec("SELECT 1 FROM snapshots WHERE refs IS NULL LIMIT 1").toArray().length > 0;
  }

  /** 删掉最后一份引用未知的快照之后，回收整体推迟到这个时间（见 refsDropped）。 */
  private gcHoldUntil(): number {
    return Number(this.sql.exec<{ v: string }>("SELECT v FROM meta WHERE k='gc_hold_until_ms'").toArray()[0]?.v ?? 0);
  }

  /**
   * 用 alarm 安排下一次回收（alarm 只有一个，跑完一次之后要重新安排）。在提交的最后调用：
   * 提交过程中到期时间可能刚好过去（回收那一步时还没到），这时马上安排；前提不满足的不安排，免得 alarm 空转。
   */
  private async scheduleGc(): Promise<void> {
    const due = this.gcDue();
    if (due === null) return;
    const wait = Math.max(due, this.gcHoldUntil()) - Date.now();
    if (wait > 0) await this.scheduleMaintenance(wait);
    else if (!this.gcBlocked()) await this.scheduleMaintenance();
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
    let pruned = { count: 0, bytes: 0 };
    try {
      pruned = await this.pruneBackups(userId, tier);
    } catch (e) {
      console.error("prune backups failed", String(e));
    }
    // 备份删掉之后才不再被引用的 blob，这次就能回收（到了回收时间的话）。
    this.collectGarbage(userId);
    await this.drainCleanup();
    await this.flushUsage(userId);
    await this.scheduleGc();
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
      this.collectGarbage(userId);
      await this.drainCleanup();
      await this.flushUsage(userId);
      await this.scheduleGc();
    });
  }

  /**
   * 清空这个用户的云端数据（后台操作）：先删 manifest，再在一个事务里把账目里的全部文件（快照、blob）
   * 记进待清理表、清空账目、存储代数加一，最后由 drainCleanup 删 R2 里的文件。删到一半失败或 DO 重启，
   * 剩下的留在待清理表里，alarm 和启动恢复会接着删。然后推送一次更新：客户端看到 manifest 不存在，
   * 会把本地的快照和 blob 重新上传，只留下现在还用得到的那部分。
   *
   * 为什么需要它：旧客户端上传的快照不声明引用了哪些 blob，只要还保留着这样的快照，blob 就不会被自动回收，
   * 删了历史也不会释放空间，占满档位后就同步不了。这里是给管理员的补救办法。
   * 和正在同步的设备之间：清空前查过缺失的设备，上传快照时代数已经变了，会被拒绝并重查（见 storageGeneration；
   * 不带声明的旧客户端见 legacyUploadAllowed）；
   * 正在上传的 blob 不在账目里，不受影响，它们的提交照常记账。
   * 拿着快照锁和提交锁执行，期间没有快照在上传或发布。
   * 删 manifest 之前先记下“清空进行中”（meta reset_pending）：删完 manifest、事务还没提交时 DO 重启，
   * 启动恢复会接着把清空做完；恢复也失败的话，下一次快照上传开始前先做完它（见 finishReset）。
   */
  async resetStorage(userId: string): Promise<{ removedObjects: number }> {
    const run = this.snapshotLock.then(() =>
      this.withCommit(async () => {
        this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('reset_pending','1')");
        return { removedObjects: await this.finishReset(userId, this.env.TEST_INTERRUPT_RESET === "1") };
      }),
    );
    this.snapshotLock = run.catch(() => {});
    return run;
  }

  /**
   * 做完记下的清空（见 resetStorage），没有就什么都不做，返回 0。调用时不能有快照在上传或发布：
   * 拿着快照锁和提交锁，或者在 DO 启动恢复里。失败就抛出，reset_pending 留着（manifest 可能已经删掉了），下次再做：管理员看到失败，清空也会在下次快照上传前或 DO 重启时做完。
   */
  private async finishReset(userId: string, interruptForTest = false): Promise<number> {
    if (this.sql.exec("SELECT 1 FROM meta WHERE k='reset_pending'").toArray().length === 0) return 0;
    // manifest 不进待清理表：清理晚于新快照发布的话，会删掉新的 manifest。失败就抛出，账目没动。
    await this.env.BUCKET.delete(manifestKey(userId));
    if (interruptForTest) throw new Error("storage reset interrupted (TEST_INTERRUPT_RESET)");
    const queued = this.ctx.storage.transactionSync(() => {
      this.sql.exec("DELETE FROM meta WHERE k='reset_pending'");
      this.bumpGeneration();
      let n = 0;
      for (const r of this.sql.exec<{ version: string; r2_key: string | null }>("SELECT version, r2_key FROM snapshots").toArray()) {
        this.sql.exec("INSERT OR IGNORE INTO cleanup (r2_key) VALUES (?)", r.r2_key ?? snapshotKey(userId, r.version));
        n++;
      }
      for (const b of this.sql.exec<{ sha256: string }>("SELECT sha256 FROM blobs").toArray()) {
        this.sql.exec("INSERT OR IGNORE INTO cleanup (r2_key, blob_sha) VALUES (?,?)", blobKey(userId, b.sha256), b.sha256);
        n++;
      }
      this.sql.exec("DELETE FROM snapshots");
      this.sql.exec("DELETE FROM blobs");
      this.sql.exec("DELETE FROM ref_sets WHERE NOT EXISTS (SELECT 1 FROM ref_claims c WHERE c.set_id = ref_sets.id)");
      this.sql.exec("DELETE FROM meta WHERE k='gc_due_ms'");
      this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('blob_bytes','0')");
      // 清空前查过缺失、清空后才上传的旧客户端据此拒绝（见 legacyUploadAllowed）。
      this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('reset_generation',?)", String(this.storageGeneration()));
      this.markUsageDirty();
      return n;
    });
    await this.drainCleanup();
    await this.flushUsage(userId);
    this.broadcast(sseEvent("sync-updated", { reset: true, updatedAt: nowIso() }));
    return queued + 1;
  }

  /**
   * 查哪些 blob 服务端还没有（hashes 已经规范化、去重），同时把这批哈希记进一条引用声明：
   * Worker 按批调用（RPC 参数有大小限制），第一批 token 为 null，新开一条声明；之后各批带上返回的 token
   * 追加进去；final 那一批把整份列表收成一个 ref_sets（按内容去重），collect 为 false（列表太长）时
   * 只标记成“超长”，分批记下的列表照样保护这些 blob。
   * 声明在有效期内保护列表里的 blob 不被回收：查完说“已经有了”的 blob，不会在客户端传快照之前被删掉。
   * 返回的 refsToken 由客户端在上传快照时带上；旧客户端不认识它，声明到期自然作废。
   * 带来的 token 已经不存在或已经收尾（不该发生）时 refsToken 为 null，Worker 让整次检查失败。
   */
  async checkBlobs(
    hashes: string[],
    declare: { token: string | null; final: boolean; collect: boolean; caller: string },
  ): Promise<{ missing: string[]; refsToken: string | null }> {
    // 从这里到查完缺失没有 await：记这批声明和查缺失是同一刻，回收（也是同步执行的）只能在这之前或之后。
    const now = Date.now();
    let token: string | null = null;
    {
      const t = declare.token ?? hex(crypto.getRandomValues(new Uint8Array(16)));
      token = this.ctx.storage.transactionSync(() => {
        if (declare.token === null) {
          this.pruneRefClaims(now);
          this.sql.exec(
            "INSERT INTO ref_claims (token, set_id, created_ms, generation, caller) VALUES (?,NULL,?,?,?)",
            t,
            now,
            this.storageGeneration(),
            declare.caller,
          );
          // 记下这台设备最近一次查缺失时的代数（见 legacyUploadAllowed）。令牌过期之后它就传不上来了，
          // 刷新令牌后旧客户端会重新查一遍、换一个调用方标识，所以更早的记录可以删掉。
          this.sql.exec("DELETE FROM caller_checks WHERE checked_ms < ?", now - CALLER_CHECK_KEEP_MS);
          this.sql.exec(
            "INSERT OR REPLACE INTO caller_checks (caller, generation, checked_ms, invalidated) VALUES (?,?,?,0)",
            declare.caller,
            this.storageGeneration(),
            now,
          );
        } else if (this.sql.exec("SELECT 1 FROM ref_claims WHERE token=? AND set_id IS NULL AND created_ms > ?", t, now - REF_CLAIM_TTL_MS).toArray().length === 0) {
          return null;
        }
        const seq = this.sql.exec<{ n: number }>("SELECT COUNT(*) AS n FROM ref_claim_chunks WHERE token=?", t).one().n;
        this.sql.exec("INSERT INTO ref_claim_chunks (token, seq, hashes) VALUES (?,?,?)", t, seq, encodeRefs(hashes).buffer as ArrayBuffer);
        this.boundRefClaims(t);
        return t;
      });
    }
    const missing: string[] = [];
    for (const h of hashes) {
      if (this.sql.exec("SELECT 1 FROM blobs WHERE sha256=?", h).toArray().length === 0) missing.push(h);
    }
    if (token && declare.final && !declare.collect) {
      const t = token;
      this.sql.exec("UPDATE ref_claims SET oversized=1 WHERE token=? AND set_id IS NULL", t);
    } else if (token && declare.final) {
      // 收齐了：合成一份排好序的列表。算哈希要 await，这期间分批记下的列表照样保护着这些 blob。
      const all = new Set<string>();
      for (const r of this.sql.exec<{ hashes: ArrayBuffer }>("SELECT hashes FROM ref_claim_chunks WHERE token=?", token)) decodeRefs(r.hashes, all);
      const bytes = encodeRefs([...all]);
      const setId = hex(await crypto.subtle.digest("SHA-256", bytes));
      const t = token;
      token = this.ctx.storage.transactionSync(() => {
        if (this.sql.exec("SELECT 1 FROM ref_claims WHERE token=? AND set_id IS NULL", t).toArray().length === 0) return null; // 刚被清空
        this.sql.exec("INSERT OR IGNORE INTO ref_sets (id, hashes) VALUES (?,?)", setId, bytes.buffer as ArrayBuffer);
        this.sql.exec("UPDATE ref_claims SET set_id=? WHERE token=?", setId, t);
        this.sql.exec("DELETE FROM ref_claim_chunks WHERE token=?", t);
        return t;
      });
    }
    return { missing, refsToken: token };
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

  /**
   * 同一个哈希的上传排队一个个来：两台设备同时传同一个附件时，后到的等前一个结束，
   * 再查一次账目，前一个已经记账就直接返回 existing，不会重复占额度，也不会对同一个 key 连续写。
   */
  private async uploadBlob(request: Request, userId: string, sha: string, tier: TierPolicy): Promise<Response> {
    const run = (this.blobUploadLocks.get(sha) ?? Promise.resolve()).then(() => this.uploadBlobLocked(request, userId, sha, tier));
    const tail = run.catch(() => {});
    this.blobUploadLocks.set(sha, tail);
    try {
      return await run;
    } finally {
      if (this.blobUploadLocks.get(sha) === tail) this.blobUploadLocks.delete(sha);
    }
  }

  private async uploadBlobLocked(request: Request, userId: string, sha: string, tier: TierPolicy): Promise<Response> {
    // 同一个哈希的文件正在被清理就先等删完；等完之后到登记上传意图之间没有 await，
    // 之后的清理会因为这条意图而跳过它。
    for (let pending = this.blobDeletions.get(sha); pending; pending = this.blobDeletions.get(sha)) await pending;
    const existing = this.sql.exec<{ size: number }>("SELECT size FROM blobs WHERE sha256=?", sha).toArray()[0];
    if (existing) {
      // 把客户端还在发的请求体读完再回复（不解压不计算），否则客户端会在发送途中被断开（EPIPE）。
      // 排在同一个哈希后面的上传通常就走到这里：它等锁时请求体一点都没读。
      await request.body?.pipeTo(new WritableStream());
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
          const now = Date.now();
          const inserted =
            this.sql.exec("INSERT OR IGNORE INTO blobs (sha256,size,created_ms) VALUES (?,?,?) RETURNING size", sha, result.size, now).toArray().length > 0;
          if (inserted) {
            this.sql.exec("INSERT OR REPLACE INTO meta (k,v) VALUES ('blob_bytes',?)", String(before + result.size));
            // 宽限期过后看一眼：上传了却没有快照用到它（快照没传成）的 blob 那时回收。
            this.markGcDue(now + this.gcGraceMs());
          }
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
   * 不带声明的上传（还没用上引用声明的 Go 客户端；已发布的 C# 客户端连的是旧服务器，到不了这里）能不能接受。
   * - 回收不影响它：它查缺失时同样会记下声明，声明有效期内不会回收它查到的 blob。
   * - 清空会：它可能是在清空前查的缺失（结论是“都在”），清空后才上传。按调用方（访问令牌）对上：
   *   这台设备最近一次查缺失（caller_checks）在最近一次清空之前，就拒绝，并删掉这条记录。它的同步就此失败，
   *   下次同步会重新导出、有 blob 就重新查，那时就能通过；只有文字的历史从来不查，也就不会被拦。
   *   旧客户端刷新令牌后会重新查一遍缺失，所以令牌变了不会让清空前的检查混过去。
   * 在 snapshotRefs 里同步调用。
   */
  private legacyUploadAllowed(caller: string, issuedMs: number): boolean {
    // 旧客户端查缺失和上传快照用同一个令牌（遇到 401 刷新令牌后会重新查一遍），所以它的检查不早于令牌签发；
    // 那次检查的声明保护它查到的 blob 24 小时。令牌签发不到 24 小时，回收就碰不到那些 blob。
    // 访问令牌只有两小时有效，下面这条正常走不到，留着把这个前提写明：超过了就不再担保。
    if (this.storageGeneration() > 0 && Date.now() - issuedMs >= REF_CLAIM_TTL_MS) return this.refuseLegacy(caller);
    // caller_checks 不随声明过期：只要这个令牌还能用，它最近那次检查就还记着。
    const last = this.sql
      .exec<{ generation: number; invalidated: number }>("SELECT generation, invalidated FROM caller_checks WHERE caller=?", caller)
      .toArray()[0];
    // 它最近那次检查的声明因为超出上限被作废了：那些 blob 不再受保护，不再担保（见 boundRefClaims）。
    if (last?.invalidated) return this.refuseLegacy(caller);
    // 用代数而不是时间排先后：清空和紧接着的一次检查可能落在同一毫秒。
    const resetGeneration = Number(this.sql.exec<{ v: string }>("SELECT v FROM meta WHERE k='reset_generation'").toArray()[0]?.v ?? 0);
    if (resetGeneration === 0) return true;
    if (last) return last.generation >= resetGeneration || this.refuseLegacy(caller);
    // 没有记录：令牌是开始记录之后签发的，就说明它从没查过缺失（只有文字），放行。
    // 签发得更早的，可能在开始记录之前查过、没留下记录：保守拒绝一次。
    const trackingSince = Number(this.sql.exec<{ v: string }>("SELECT v FROM meta WHERE k='caller_tracking_ms'").one().v);
    return issuedMs >= trackingSince || this.refuseLegacy(caller);
  }

  /**
   * 拒绝一次不带声明的上传，并记下“已经拦过”（当前代数、未作废）：这台设备的这次同步就此失败，
   * 下次同步会重新导出，有 blob 就重新查（记录随之更新），只有文字就不查，凭这条记录放行。
   */
  private refuseLegacy(caller: string): false {
    this.sql.exec(
      "INSERT OR REPLACE INTO caller_checks (caller, generation, checked_ms, invalidated) VALUES (?,?,?,0)",
      caller,
      this.storageGeneration(),
      Date.now(),
    );
    return false;
  }

  /**
   * 读上传快照的 x-klip-blob-refs 头：没有这个头 = 引用未知（旧客户端）；"none" = 没有 blob；
   * 否则是查缺失时拿到的声明 token。声明不存在或过期了（blob_refs_expired）、查完之后有 blob 被删过
   * （storage_generation_stale，见 storageGeneration）、或者声明里有服务端还没有的 blob（blob_refs_missing），
   * 都返回 409，客户端重新查缺失、补传 blob 之后再传。
   */
  private snapshotRefs(
    header: string | null,
    caller: { id: string; issuedMs: number; unique: boolean },
  ): { refs: string | null; token: string | null } | Response {
    const stale = () =>
      json({ code: "storage_generation_stale", error: "blobs were deleted after the blob check; check again", storageGeneration: this.storageGeneration() }, 409);
    if (header === null) {
      // 不带声明的上传靠调用方（令牌）把检查和上传对上；这一版之前签发的令牌可能几台设备共用一个，
      // 对不上。让它先刷新令牌：旧客户端遇到 401 会刷新，并用新令牌重新查一遍缺失再上传。
      if (!caller.unique) return errorJson(401, "access token predates blob reference tracking; refresh it");
      return this.legacyUploadAllowed(caller.id, caller.issuedMs) ? { refs: null, token: null } : stale();
    }
    const value = header.trim();
    // 没有 blob 的快照不引用任何 blob，不会因为 blob 被删而坏掉，不看代数。
    if (value === "none") return { refs: NO_REFS, token: null };
    const claim = this.sql
      .exec<{ set_id: string | null; generation: number; oversized: number }>(
        "SELECT set_id, generation, oversized FROM ref_claims WHERE token=? AND created_ms > ?",
        value,
        Date.now() - REF_CLAIM_TTL_MS,
      )
      .toArray()[0];
    if (claim?.oversized) {
      // 列表太长没收成一份：只核对代数，快照按“引用未知”记账。
      return claim.generation === this.storageGeneration() ? { refs: null, token: value } : stale();
    }
    const set = claim?.set_id ? this.sql.exec<{ hashes: ArrayBuffer }>("SELECT hashes FROM ref_sets WHERE id=?", claim.set_id).toArray()[0] : undefined;
    if (!claim || !claim.set_id || !set) return json({ code: "blob_refs_expired", error: "blob reference declaration not found or expired" }, 409);
    if (claim.generation !== this.storageGeneration()) return stale();
    const hashes = new Set<string>();
    decodeRefs(set.hashes, hashes);
    const missing: string[] = [];
    for (const h of hashes) if (this.sql.exec("SELECT 1 FROM blobs WHERE sha256=?", h).toArray().length === 0) missing.push(h);
    if (missing.length > 0) return json({ code: "blob_refs_missing", error: "snapshot refers to blobs the server does not have", missing: missing.slice(0, 100) }, 409);
    return { refs: claim.set_id as string, token: value };
  }

  /**
   * 上传快照：先登记成“正在发布”，写进 R2，确定放得下，写 manifest，最后才切成当前快照。
   * 任何一步失败都撤销这份待发布的快照，当前快照、manifest、备份都不动；
   * 写 manifest 的结果不明时先回读核对，读不到就保留待发布状态，留给下次上传或 DO 重启时的 reconcilePending。
   */
  private async uploadSnapshot(request: Request, userId: string, tier: TierPolicy): Promise<Response> {
    // 先做完没做完的清空，再处理上次留下的、结果不明的发布（拿着 snapshotLock，没有别的快照在上传）。
    // 清空做不完就让这次上传失败：不能在 manifest 已删、账目还在的状态下发布新快照。
    await this.withCommit(async () => {
      await this.finishReset(userId);
      await this.reconcilePending(userId);
    });
    // 这份快照引用了哪些 blob（见 checkBlobs）。从这里到登记待发布快照之间没有 await：
    // 核对过的 blob 不会在登记之前被回收；登记之后，这份快照的引用就保护着它们。
    const refs = this.snapshotRefs(request.headers.get("x-klip-blob-refs"), {
      id: request.headers.get("x-klip-caller") ?? "",
      issuedMs: Number(request.headers.get("x-klip-caller-issued-ms") ?? 0),
      unique: request.headers.get("x-klip-caller-unique") === "1",
    });
    if (refs instanceof Response) {
      await drainBody(request);
      return refs;
    }
    const version = this.nextVersion();
    const key = snapshotKey(userId, version);
    this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        "INSERT INTO snapshots (version,created_ms,size,sha256,current,r2_key,refs) VALUES (?,?,0,'',?,?,?)",
        version,
        Number(version),
        PENDING,
        key,
        refs.refs,
      );
      // 声明用掉了：引用由这份快照接着保护。上传失败的话快照记录撤销，客户端下次同步会重新查一遍。
      if (refs.token) this.sql.exec("DELETE FROM ref_claims WHERE token=?", refs.token);
    });
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
      // R2 同一个 key 每秒最多写一次，超了会被限流：先等够间隔，失败了再退避重试几次。
      // 内容和 snapshotKey 每次都一样，重复写是安全的。
      for (let attempt = 0; ; attempt++) {
        const wait = this.lastManifestWrite + MANIFEST_WRITE_INTERVAL_MS - Date.now();
        if (wait > 0) await sleep(wait);
        try {
          this.lastManifestWrite = Date.now();
          await this.env.BUCKET.put(manifestKey(userId), JSON.stringify(manifest), {
            httpMetadata: { contentType: "application/json; charset=utf-8" },
            customMetadata: { snapshotKey: key },
          });
          break;
        } catch (e) {
          // 写入失败也可能其实已经写进去了（只是确认没回来）：回读 manifest 核对。
          let published: string | undefined;
          try {
            published = (await this.env.BUCKET.head(manifestKey(userId)))?.customMetadata?.snapshotKey;
          } catch {
            // 结果不明：保留这份待发布的快照，下次上传或 DO 重启时再按 manifest 判断。
            throw e;
          }
          // manifest 已经指向它：当作发布成功，继续。
          if (published === key) break;
          if (attempt + 1 >= MANIFEST_WRITE_ATTEMPTS) {
            await discard();
            throw e;
          }
          await sleep(MANIFEST_WRITE_INTERVAL_MS * 2 ** attempt);
        }
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
    const backups = this.sql.exec<SnapshotRow>(`SELECT ${SNAPSHOT_COLS} FROM snapshots WHERE current=? ORDER BY created_ms ASC`, BACKUP).toArray();
    // BACKUP_RETENTION_SECONDS 只给测试用：让备份很快过期，好观察 blob 回收。
    const cutoff = Date.now() - this.envMs("BACKUP_RETENTION_SECONDS", Math.abs(tier.retentionDays) * DAY_MS);
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
        for (const b of batch) this.refsDropped(b.refs);
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
