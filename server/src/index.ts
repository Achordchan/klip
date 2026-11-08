// Klip 云同步后端入口。接口与旧版（nas-agent-py）保持一致，客户端无需改动。

import type { Env } from "./env";
import { SUPPORTED_TIERS, getTierPolicy, normalizeTierName } from "./env";
import { type Claims, signJwt, verifyJwt } from "./crypto";
import * as db from "./db";
import { blobKey, manifestKey, UserStore } from "./user_store";
import { isEmailConfigured } from "./smtp";
import {
  ACCESS_TOKEN_HOURS,
  ADMIN_AUDIENCE,
  ADMIN_TOKEN_HOURS,
  DAY_MS,
  HOUR_MS,
  HttpFailure,
  PASSWORD_CODE_RESEND_COOLDOWN_SECONDS,
  REFRESH_TOKEN_DAYS,
  TOKEN_LEEWAY_SECONDS,
  asInt,
  base64UrlEncode,
  clamp,
  errorJson,
  httpError,
  isValidEmail,
  json,
  newId,
  normalizeBlobSha256,
  normalizeEmail,
  nowIso,
  randomBytes,
  readJson,
  sha256Hex,
  sseEvent,
  str,
} from "./util";

export { UserStore };

const NAME_ID_CLAIM = "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/nameidentifier";
const EMAIL_CLAIM = "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress";
const EVENTS_KEEPALIVE_MS = 20_000;
// 一条推送连接最长保持多久。免费版 Worker 每个请求累计只有 10ms CPU，
// 心跳会一点点用掉它；到时间就正常结束，客户端会立刻重连（并顺带同步一次）。
const EVENTS_WINDOW_MS = 15 * 60_000;

type Ctx = ExecutionContext;
type Handler = (req: Request, env: Env, ctx: Ctx, params: string[]) => Promise<Response>;

function userStub(env: Env, userId: string) {
  return env.USER.get(env.USER.idFromName(userId));
}

// ---- 鉴权 ----

function bearer(request: Request): string | null {
  const value = (request.headers.get("Authorization") ?? "").trim();
  if (!value.toLowerCase().startsWith("bearer ")) return null;
  return value.slice(7).trim() || null;
}

async function requireUser(request: Request, env: Env): Promise<db.UserRow> {
  const token = bearer(request);
  if (!token) throw new HttpFailure(httpError(401));
  const claims = await verifyJwt(token, env.AUTH_SIGNING_KEY, { audience: env.AUTH_AUDIENCE, issuer: env.AUTH_ISSUER, leewaySeconds: TOKEN_LEEWAY_SECONDS });
  const userId = str(claims?.sub || claims?.[NAME_ID_CLAIM]).trim();
  if (!userId) throw new HttpFailure(httpError(401));
  const user = await db.getUserById(env, userId);
  if (!user) throw new HttpFailure(httpError(401));
  return user;
}

async function requireAdmin(request: Request, env: Env): Promise<{ email: string; authType: string; accessTokenExpiresAt: string | null }> {
  const token = bearer(request);
  if (!token) throw new HttpFailure(httpError(401));
  const adminEmail = normalizeEmail(env.ADMIN_EMAIL);
  if (env.ADMIN_KEY && token === env.ADMIN_KEY) return { email: adminEmail || "admin", authType: "legacy-key", accessTokenExpiresAt: null };
  const claims = await verifyJwt(token, env.AUTH_SIGNING_KEY, { audience: ADMIN_AUDIENCE, issuer: env.AUTH_ISSUER, leewaySeconds: TOKEN_LEEWAY_SECONDS });
  if (!claims || str(claims.role).trim().toLowerCase() !== "admin") throw new HttpFailure(httpError(401));
  const exp = typeof claims.exp === "number" ? new Date(claims.exp * 1000).toISOString() : null;
  return { email: normalizeEmail(claims.email) || adminEmail || "admin", authType: "password", accessTokenExpiresAt: exp };
}

async function accessTokenFor(env: Env, userId: string, email: string) {
  const now = Date.now();
  const iat = Math.floor(now / 1000);
  const accessExpires = now + ACCESS_TOKEN_HOURS * HOUR_MS;
  const claims: Claims = {
    sub: userId,
    email,
    [NAME_ID_CLAIM]: userId,
    [EMAIL_CLAIM]: email,
    iss: env.AUTH_ISSUER,
    aud: env.AUTH_AUDIENCE,
    nbf: iat,
    iat,
    exp: Math.floor(accessExpires / 1000),
  };
  return { accessToken: await signJwt(claims, env.AUTH_SIGNING_KEY), accessTokenExpiresAt: new Date(accessExpires).toISOString() };
}

function newRefreshToken() {
  return { token: base64UrlEncode(randomBytes(32)), expiresAt: new Date(Date.now() + REFRESH_TOKEN_DAYS * DAY_MS).toISOString() };
}

async function issueTokens(env: Env, userId: string, email: string) {
  const refreshToken = newRefreshToken();
  await db.insertRefreshToken(env, await sha256Hex(refreshToken.token), userId, refreshToken.expiresAt);
  const access = await accessTokenFor(env, userId, email);
  return { accessToken: access.accessToken, refreshToken: refreshToken.token, accessTokenExpiresAt: access.accessTokenExpiresAt, email, userId };
}

/** 订阅有效且分配了档位才能同步。 */
async function requireSync(request: Request, env: Env) {
  const user = await requireUser(request, env);
  if (!db.isActive(await db.getSubscription(env, user.id))) {
    throw new HttpFailure(json({ code: "subscription_expired", error: "subscription expired" }, 403));
  }
  const tier = getTierPolicy(user.tier);
  if (!tier) throw new HttpFailure(json({ code: "tier_not_assigned", error: "tier not assigned" }, 403));
  return { user, tier };
}

// ---- 首页与健康检查 ----

const index: Handler = async () =>
  new Response(
    '<!doctype html><html><body style="font-family:system-ui;padding:24px"><h1>Klip Sync</h1><p><a href="/health">health</a> | <a href="/admin">admin</a></p></body></html>',
    { headers: { "Content-Type": "text/html; charset=utf-8" } },
  );

const health: Handler = async () => json({ ok: true });

// ---- 账号 ----

const register: Handler = async (req, env) => {
  const p = await readJson(req);
  if (!p) return errorJson(400, "invalid request");
  const email = normalizeEmail(p.email);
  const password = str(p.password);
  if (!isValidEmail(email)) return errorJson(400, "invalid email");
  if (password.length < 8) return errorJson(400, "password must be at least 8 characters");
  if (await db.getUserByEmail(env, email)) return errorJson(400, "email already registered");
  const userId = newId();
  const hash = await userStub(env, userId).hashPassword(password);
  if (!(await db.insertUser(env, userId, email, hash))) return errorJson(400, "email already registered");
  return json(await issueTokens(env, userId, email));
};

const login: Handler = async (req, env) => {
  const p = await readJson(req);
  if (!p) return errorJson(400, "invalid request");
  const user = await db.getUserByEmail(env, normalizeEmail(p.email));
  if (!user) return errorJson(400, "invalid email or password");
  if (!(await userStub(env, user.id).verifyPassword(user.password_hash, str(p.password)))) return errorJson(400, "invalid email or password");
  // 刷新令牌只在密码哈希没被改过时才发（验证期间密码被重置的话，这次登录作废）。
  const refreshToken = newRefreshToken();
  if (!(await db.insertRefreshTokenIfPassword(env, await sha256Hex(refreshToken.token), user.id, refreshToken.expiresAt, user.password_hash))) {
    return errorJson(400, "invalid email or password");
  }
  const access = await accessTokenFor(env, user.id, user.email);
  return json({ accessToken: access.accessToken, refreshToken: refreshToken.token, accessTokenExpiresAt: access.accessTokenExpiresAt, email: user.email, userId: user.id });
};

const refresh: Handler = async (req, env) => {
  const p = await readJson(req);
  const token = p?.refreshToken;
  if (typeof token !== "string" || !token.trim()) return errorJson(400, "missing refresh token");
  const fresh = newRefreshToken();
  const user = await db.rotateRefreshToken(env, await sha256Hex(token.trim()), await sha256Hex(fresh.token), fresh.expiresAt);
  if (!user) return httpError(401);
  const access = await accessTokenFor(env, user.id, user.email);
  return json({ accessToken: access.accessToken, refreshToken: fresh.token, accessTokenExpiresAt: access.accessTokenExpiresAt, email: user.email, userId: user.id });
};

const me: Handler = async (req, env) => {
  const user = await requireUser(req, env);
  return json({ id: user.id, email: user.email });
};

const passwordCode: Handler = async (req, env) => {
  const p = await readJson(req);
  if (!p) return errorJson(400, "invalid request");
  const purpose = str(p.purpose).trim().toLowerCase();
  if (purpose !== "reset" && purpose !== "change") return errorJson(400, "invalid purpose");

  let user: db.UserRow | null;
  let email: string;
  if (purpose === "change") {
    user = await requireUser(req, env);
    email = normalizeEmail(user.email);
  } else {
    email = normalizeEmail(p.email);
    if (!isValidEmail(email)) return errorJson(400, "invalid email");
    user = await db.getUserByEmail(env, email);
    if (!user) return json({ success: true, cooldownSeconds: PASSWORD_CODE_RESEND_COOLDOWN_SECONDS });
  }
  if (!isEmailConfigured(env)) return errorJson(503, "email service not configured");

  try {
    const r = await userStub(env, user.id).issuePasswordCode(email, purpose);
    return json({ success: true, cooldownSeconds: r.cooldownSeconds });
  } catch (e) {
    console.error("send password code failed", String(e));
    return errorJson(503, "failed to send email");
  }
};

async function setPassword(env: Env, userId: string, password: string): Promise<void> {
  const hash = await userStub(env, userId).hashPassword(password);
  await env.DB.batch(db.updatePasswordStatements(env, userId, hash));
}

const passwordReset: Handler = async (req, env) => {
  const p = await readJson(req);
  if (!p) return errorJson(400, "invalid request");
  const email = normalizeEmail(p.email);
  const code = str(p.code).trim();
  const newPassword = str(p.newPassword);
  if (!isValidEmail(email)) return errorJson(400, "invalid email");
  if (newPassword.length < 8) return errorJson(400, "password must be at least 8 characters");
  const user = await db.getUserByEmail(env, email);
  if (!user) return errorJson(400, "invalid or expired code");
  const error = await db.verifyPasswordCode(env, email, "reset", code);
  if (error) return errorJson(400, error);
  await setPassword(env, user.id, newPassword);
  return json({ success: true });
};

const passwordChange: Handler = async (req, env) => {
  const user = await requireUser(req, env);
  const p = await readJson(req);
  if (!p) return errorJson(400, "invalid request");
  const code = str(p.code).trim();
  const newPassword = str(p.newPassword);
  if (newPassword.length < 8) return errorJson(400, "password must be at least 8 characters");
  const error = await db.verifyPasswordCode(env, normalizeEmail(user.email), "change", code);
  if (error) return errorJson(400, error);
  await setPassword(env, user.id, newPassword);
  return json({ success: true });
};

// ---- 订阅 ----

const subscriptionStatus: Handler = async (req, env) => {
  const user = await requireUser(req, env);
  const expiresAt = await db.getSubscription(env, user.id);
  const tier = getTierPolicy(user.tier);
  const storageBytes = user.storage_bytes ?? 0;
  return json({
    subscribed: db.isActive(expiresAt),
    expiresAt,
    tier: tier?.name ?? null,
    storageBytes,
    storageLimitBytes: tier?.storageLimitBytes ?? null,
    retentionDays: tier?.retentionDays ?? null,
    overLimit: tier !== null && storageBytes > tier.storageLimitBytes,
  });
};

const redeemCard: Handler = async (req, env) => {
  const user = await requireUser(req, env);
  const p = await readJson(req);
  const raw = p?.code;
  if (typeof raw !== "string" || !raw.trim()) return errorJson(400, "missing code");
  const code = raw.trim().toUpperCase();
  const expiresAt = await db.redeemCard(env, user.id, code);
  if (!expiresAt) return errorJson(400, "card not found or used");
  return json({ success: true, expiresAt });
};

// ---- 同步 ----

const syncManifest: Handler = async (req, env) => {
  const { user } = await requireSync(req, env);
  const obj = await env.BUCKET.get(manifestKey(user.id));
  if (!obj) return httpError(404);
  return new Response(obj.body, { headers: { "Content-Type": "application/json; charset=utf-8" } });
};

const syncSnapshotGet: Handler = async (req, env) => {
  const { user } = await requireSync(req, env);
  // 读到 manifest 之后、取文件之前，可能正好有新快照发布并把旧的清掉了：
  // 文件不见了就重新读 manifest 再取。
  let obj: R2ObjectBody | null = null;
  for (let attempt = 0; attempt < 3 && !obj; attempt++) {
    const key = (await env.BUCKET.head(manifestKey(user.id)))?.customMetadata?.snapshotKey;
    if (!key) return httpError(404);
    obj = await env.BUCKET.get(key);
  }
  if (!obj) return httpError(404);
  return new Response(obj.body, {
    headers: { "Content-Type": "application/octet-stream", "Content-Length": String(obj.size), "Content-Disposition": 'attachment; filename="snapshot.sqlite"' },
  });
};

function forwardUpload(req: Request, env: Env, userId: string, path: string, extra: Record<string, string> = {}): Promise<Response> {
  const headers = new Headers({ "x-klip-user": userId, ...extra });
  const encoding = req.headers.get("content-encoding");
  if (encoding) headers.set("content-encoding", encoding);
  return userStub(env, userId).fetch(new Request(`https://user-store${path}`, { method: "PUT", headers, body: req.body }));
}

const syncSnapshotPut: Handler = async (req, env) => {
  const { user, tier } = await requireSync(req, env);
  return forwardUpload(req, env, user.id, "/snapshot", { "x-klip-tier": JSON.stringify(tier) });
};

const syncBlobCheck: Handler = async (req, env) => {
  const { user } = await requireSync(req, env);
  const p = (await readJson(req)) ?? {};
  if (!Array.isArray(p.hashes)) return errorJson(400, "missing hashes");
  const hashes = [...new Set(p.hashes.map(normalizeBlobSha256).filter((h): h is string => h !== null))];
  const missing = hashes.length ? await userStub(env, user.id).missingBlobs(hashes) : [];
  return json({ missing });
};

const syncBlobPut: Handler = async (req, env, _ctx, [raw]) => {
  const { user, tier } = await requireSync(req, env);
  const sha = normalizeBlobSha256(raw);
  if (!sha) return errorJson(400, "invalid blob hash");
  return forwardUpload(req, env, user.id, `/blobs/${sha}`, { "x-klip-tier": JSON.stringify(tier) });
};

const syncBlobGet: Handler = async (req, env, _ctx, [raw]) => {
  const { user } = await requireSync(req, env);
  const sha = normalizeBlobSha256(raw);
  if (!sha) return errorJson(400, "invalid blob hash");
  const obj = await env.BUCKET.get(blobKey(user.id, sha));
  if (!obj) return httpError(404);
  return new Response(obj.body, { headers: { "Content-Type": "application/octet-stream", "Content-Length": String(obj.size) } });
};

const syncEvents: Handler = async (req, env, ctx) => {
  const { user } = await requireSync(req, env);
  const upstream = await userStub(env, user.id).fetch("https://user-store/events", { headers: { Upgrade: "websocket", "x-klip-user": user.id } });
  const ws = upstream.webSocket;
  if (!ws) return errorJson(502, "event channel unavailable");
  ws.accept();

  const { readable, writable } = new TransformStream<Uint8Array, Uint8Array>();
  const writer = writable.getWriter();
  const encoder = new TextEncoder();
  let closed = false;
  let wake: (() => void) | null = null;
  const finish = () => {
    if (closed) return;
    closed = true;
    try {
      ws.close(1000, "done");
    } catch {
      /* 已经关了 */
    }
    writer.close().catch(() => {});
    wake?.();
  };
  const send = (text: string) => {
    if (!closed) writer.write(encoder.encode(text)).catch(finish);
  };
  ws.addEventListener("message", (e) => send(typeof e.data === "string" ? e.data : ""));
  ws.addEventListener("close", finish);
  ws.addEventListener("error", finish);

  send(sseEvent("connected", { serverTime: nowIso() }));
  ctx.waitUntil(
    (async () => {
      const end = Date.now() + EVENTS_WINDOW_MS;
      while (!closed && Date.now() < end) {
        await new Promise<void>((resolve) => {
          wake = resolve;
          setTimeout(resolve, EVENTS_KEEPALIVE_MS);
        });
        send(": keepalive\n\n");
      }
      finish();
    })(),
  );

  return new Response(readable, {
    headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive", "X-Accel-Buffering": "no" },
  });
};

// ---- 后台 ----

const adminLogin: Handler = async (req, env) => {
  const p = await readJson(req);
  if (!p) return errorJson(400, "invalid request");
  const adminEmail = normalizeEmail(env.ADMIN_EMAIL);
  if (!adminEmail || !env.ADMIN_PASSWORD) return errorJson(503, "admin password login not configured");
  if (normalizeEmail(p.email) !== adminEmail || str(p.password) !== env.ADMIN_PASSWORD) return httpError(401);
  const now = Date.now();
  const iat = Math.floor(now / 1000);
  const expires = now + ADMIN_TOKEN_HOURS * HOUR_MS;
  const accessToken = await signJwt(
    { sub: "admin", email: adminEmail, role: "admin", iss: env.AUTH_ISSUER, aud: ADMIN_AUDIENCE, nbf: iat, iat, exp: Math.floor(expires / 1000) },
    env.AUTH_SIGNING_KEY,
  );
  return json({ accessToken, accessTokenExpiresAt: new Date(expires).toISOString(), email: adminEmail });
};

const adminMe: Handler = async (req, env) => json(await requireAdmin(req, env));

const adminUsers: Handler = async (req, env) => {
  await requireAdmin(req, env);
  return json({ users: await db.listUsers(env) });
};

const adminSubscriptions: Handler = async (req, env) => {
  await requireAdmin(req, env);
  return json({ subscriptions: await db.listSubscriptions(env) });
};

const adminCards: Handler = async (req, env) => {
  await requireAdmin(req, env);
  return json({ cards: await db.listCards(env) });
};

const adminGenerateCards: Handler = async (req, env) => {
  await requireAdmin(req, env);
  const p = (await readJson(req)) ?? {};
  const count = clamp(asInt(p.count, 1), 1, 100);
  const durationDays = clamp(asInt(p.durationDays, 30), 1, 3650);
  const codes = Array.from({ length: count }, db.generateCardCode);
  const now = nowIso();
  // 一条语句插入全部：免费版 D1 每次调用最多 50 条查询，batch 里的每条都算。
  await env.DB.prepare("INSERT INTO cards (code,duration_days,created_at) SELECT value, ?, ? FROM json_each(?)").bind(durationDays, now, JSON.stringify(codes)).run();
  return json({ codes, durationDays });
};

async function adminTarget(req: Request, env: Env): Promise<{ user: db.UserRow; p: Record<string, unknown> } | Response> {
  await requireAdmin(req, env);
  const p = await readJson(req);
  const key = str(p?.emailOrUserId).trim();
  if (!p || !key) return errorJson(400, "missing emailOrUserId");
  const user = await db.resolveUser(env, key);
  if (!user) return errorJson(400, "user not found");
  return { user, p };
}

const adminUpgrade: Handler = async (req, env) => {
  const t = await adminTarget(req, env);
  if (t instanceof Response) return t;
  const { user, p } = t;
  const durationDays = clamp(asInt(p.durationDays, 0), 0, 3650);
  const hasTier = p.tier !== null && p.tier !== undefined && str(p.tier).trim() !== "";
  const tier = normalizeTierName(p.tier);
  if (hasTier && tier === null) return json({ error: "invalid tier", supportedTiers: SUPPORTED_TIERS }, 400);
  if (durationDays <= 0 && !hasTier) return errorJson(400, "missing operation (durationDays or tier required)");

  const statements: D1PreparedStatement[] = [];
  if (durationDays > 0) statements.push(db.extendSubscriptionStatement(env, user.id, durationDays));
  if (hasTier) statements.push(env.DB.prepare("UPDATE users SET tier=?, updated_at=? WHERE id=?").bind(tier, nowIso(), user.id));
  statements.push(env.DB.prepare("SELECT expires_at FROM subscriptions WHERE user_id=?").bind(user.id));
  const results = await env.DB.batch(statements);
  const expiresAt = (results[results.length - 1].results[0] as { expires_at: string } | undefined)?.expires_at ?? null;
  return json({ success: true, expiresAt, email: user.email, userId: user.id, tier: tier ?? normalizeTierName(user.tier) });
};

const adminSetTier: Handler = async (req, env) => {
  const t = await adminTarget(req, env);
  if (t instanceof Response) return t;
  const { user, p } = t;
  const tier = normalizeTierName(p.tier);
  if (p.tier !== null && p.tier !== undefined && p.tier !== "" && tier === null) return json({ error: "invalid tier", supportedTiers: SUPPORTED_TIERS }, 400);
  await db.updateUserTier(env, user.id, tier);
  return json({ success: true, userId: user.id, email: user.email, tier });
};

const adminResetPassword: Handler = async (req, env) => {
  const t = await adminTarget(req, env);
  if (t instanceof Response) return t;
  const { user, p } = t;
  const newPassword = str(p.newPassword);
  if (newPassword.length < 8) return errorJson(400, "password must be at least 8 characters");
  await setPassword(env, user.id, newPassword);
  return json({ success: true, userId: user.id, email: user.email });
};

const adminResetStorage: Handler = async (req, env) => {
  const t = await adminTarget(req, env);
  if (t instanceof Response) return t;
  const { user } = t;
  const r = await userStub(env, user.id).resetStorage(user.id);
  return json({ success: true, userId: user.id, email: user.email, removedObjects: r.removedObjects });
};

// ---- 路由表 ----

const ROUTES: Array<[string, RegExp, Handler]> = [
  ["GET", /^\/$/, index],
  ["GET", /^\/health$/, health],
  ["POST", /^\/auth\/register$/, register],
  ["POST", /^\/auth\/login$/, login],
  ["POST", /^\/auth\/refresh$/, refresh],
  ["GET", /^\/auth\/me$/, me],
  ["POST", /^\/auth\/password\/code$/, passwordCode],
  ["POST", /^\/auth\/password\/reset$/, passwordReset],
  ["POST", /^\/auth\/password\/change$/, passwordChange],
  ["GET", /^\/subscription\/status$/, subscriptionStatus],
  ["POST", /^\/card\/redeem$/, redeemCard],
  ["GET", /^\/sync\/manifest$/, syncManifest],
  ["GET", /^\/sync\/snapshot$/, syncSnapshotGet],
  ["PUT", /^\/sync\/snapshot$/, syncSnapshotPut],
  ["GET", /^\/sync\/events$/, syncEvents],
  ["POST", /^\/sync\/blobs\/check$/, syncBlobCheck],
  ["PUT", /^\/sync\/blobs\/([^/]+)$/, syncBlobPut],
  ["GET", /^\/sync\/blobs\/([^/]+)$/, syncBlobGet],
  ["POST", /^\/admin\/auth\/login$/, adminLogin],
  ["GET", /^\/admin\/auth\/me$/, adminMe],
  ["GET", /^\/admin\/users\/list$/, adminUsers],
  ["GET", /^\/admin\/subscriptions\/list$/, adminSubscriptions],
  ["GET", /^\/admin\/cards\/list$/, adminCards],
  ["POST", /^\/admin\/card\/generate$/, adminGenerateCards],
  ["POST", /^\/admin\/user\/upgrade$/, adminUpgrade],
  ["POST", /^\/admin\/user\/tier$/, adminSetTier],
  ["POST", /^\/admin\/user\/reset-password$/, adminResetPassword],
  ["POST", /^\/admin\/user\/reset-storage$/, adminResetStorage],
];

export default {
  async fetch(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
    const path = new URL(request.url).pathname;
    let pathMatched = false;
    for (const [method, pattern, handler] of ROUTES) {
      const m = path.match(pattern);
      if (!m) continue;
      pathMatched = true;
      if (method !== request.method && !(method === "GET" && request.method === "HEAD")) continue;
      try {
        return await handler(request, env, ctx, m.slice(1).map(decodeURIComponent));
      } catch (e) {
        if (e instanceof HttpFailure) return e.response;
        console.error(request.method, path, e instanceof Error ? e.stack : String(e));
        return errorJson(500, "internal error");
      }
    }
    return httpError(pathMatched ? 405 : 404);
  },
} satisfies ExportedHandler<Env>;
