// D1 上的账号、令牌、卡密、订阅与验证码。

import type { Env } from "./env";
import { getTierPolicy, normalizeTierName } from "./env";
import { DAY_MS, PASSWORD_CODE_DIGITS, PASSWORD_CODE_MAX_ATTEMPTS, PASSWORD_CODE_RESEND_COOLDOWN_SECONDS, PASSWORD_CODE_TTL_MINUTES, newId, normalizeEmail, nowIso, parseUtc, sha256Hex } from "./util";

export interface UserRow {
  id: string;
  email: string;
  password_hash: string;
  tier: string | null;
  storage_bytes: number;
}

const USER_COLUMNS = "id,email,password_hash,tier,storage_bytes";

export async function getUserByEmail(env: Env, email: string): Promise<UserRow | null> {
  return env.DB.prepare(`SELECT ${USER_COLUMNS} FROM users WHERE email=?`).bind(email).first<UserRow>();
}

export async function getUserById(env: Env, id: string): Promise<UserRow | null> {
  return env.DB.prepare(`SELECT ${USER_COLUMNS} FROM users WHERE id=?`).bind(id).first<UserRow>();
}

export async function resolveUser(env: Env, emailOrUserId: string): Promise<UserRow | null> {
  return (await getUserByEmail(env, normalizeEmail(emailOrUserId))) ?? (await getUserById(env, emailOrUserId));
}

export async function insertUser(env: Env, id: string, email: string, passwordHash: string): Promise<boolean> {
  const now = nowIso();
  try {
    await env.DB.prepare("INSERT INTO users (id,email,password_hash,tier,storage_bytes,created_at,updated_at) VALUES (?,?,?,NULL,0,?,?)")
      .bind(id, email, passwordHash, now, now)
      .run();
    return true;
  } catch (e) {
    if (String(e).includes("UNIQUE")) return false;
    throw e;
  }
}

export function updatePasswordStatements(env: Env, userId: string, passwordHash: string): D1PreparedStatement[] {
  const now = nowIso();
  return [
    env.DB.prepare("UPDATE users SET password_hash=?, updated_at=? WHERE id=?").bind(passwordHash, now, userId),
    env.DB.prepare("UPDATE refresh_tokens SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL").bind(now, userId),
  ];
}

export async function updateUserTier(env: Env, userId: string, tier: string | null): Promise<void> {
  await env.DB.prepare("UPDATE users SET tier=?, updated_at=? WHERE id=?").bind(tier, nowIso(), userId).run();
}

export async function setStorageBytes(env: Env, userId: string, bytes: number): Promise<void> {
  await env.DB.prepare("UPDATE users SET storage_bytes=? WHERE id=?").bind(bytes, userId).run();
}

// ---- 刷新令牌 ----

export async function insertRefreshToken(env: Env, tokenHash: string, userId: string, expiresAt: string): Promise<void> {
  await env.DB.prepare("INSERT INTO refresh_tokens (token_hash,user_id,expires_at,created_at,revoked_at) VALUES (?,?,?,?,NULL)")
    .bind(tokenHash, userId, expiresAt, nowIso())
    .run();
}

/** 把一个有效的刷新令牌作废并返回它的用户；无效就返回 null。 */
export async function consumeRefreshToken(env: Env, tokenHash: string): Promise<string | null> {
  const now = nowIso();
  const row = await env.DB.prepare(
    "UPDATE refresh_tokens SET revoked_at=? WHERE token_hash=? AND revoked_at IS NULL AND expires_at > ? RETURNING user_id",
  )
    .bind(now, tokenHash, now)
    .first<{ user_id: string }>();
  return row?.user_id ?? null;
}

// ---- 订阅与卡密 ----

export async function getSubscription(env: Env, userId: string): Promise<string | null> {
  const row = await env.DB.prepare("SELECT expires_at FROM subscriptions WHERE user_id=?").bind(userId).first<{ expires_at: string }>();
  return row?.expires_at ?? null;
}

export function isActive(expiresAt: string | null): boolean {
  return expiresAt !== null && parseUtc(expiresAt) > Date.now();
}

export function extendFrom(expiresAt: string | null, days: number): string {
  const base = isActive(expiresAt) ? parseUtc(expiresAt) : Date.now();
  return new Date(base + days * DAY_MS).toISOString();
}

export function upsertSubscriptionStatement(env: Env, userId: string, expiresAt: string): D1PreparedStatement {
  const now = nowIso();
  return env.DB.prepare(
    "INSERT INTO subscriptions (user_id,expires_at,created_at,updated_at) VALUES (?,?,?,?) " +
      "ON CONFLICT(user_id) DO UPDATE SET expires_at=excluded.expires_at, updated_at=excluded.updated_at",
  ).bind(userId, expiresAt, now, now);
}

export interface CardRow {
  code: string;
  duration_days: number;
  used_by: string | null;
}

export async function getCard(env: Env, code: string): Promise<CardRow | null> {
  return env.DB.prepare("SELECT code,duration_days,used_by FROM cards WHERE code=?").bind(code).first<CardRow>();
}

export function generateCardCode(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(8));
  const code = [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("").toUpperCase();
  return `${code.slice(0, 4)}-${code.slice(4, 8)}-${code.slice(8, 12)}-${code.slice(12, 16)}`;
}

// ---- 后台列表 ----

function usageFields(tier: string | null, storageBytes: number) {
  const policy = getTierPolicy(tier);
  return {
    storage_bytes: storageBytes,
    storage_limit_bytes: policy?.storageLimitBytes ?? null,
    retention_days: policy?.retentionDays ?? null,
    over_limit: policy !== null && storageBytes > policy.storageLimitBytes,
  };
}

export async function listUsers(env: Env) {
  const { results } = await env.DB.prepare(
    "SELECT u.id,u.email,u.tier,u.storage_bytes,u.created_at,u.updated_at,s.expires_at " +
      "FROM users u LEFT JOIN subscriptions s ON s.user_id=u.id ORDER BY u.created_at DESC",
  ).all<{ id: string; email: string; tier: string | null; storage_bytes: number; created_at: string; updated_at: string; expires_at: string | null }>();
  return results.map((r) => {
    const tier = normalizeTierName(r.tier);
    return { id: r.id, email: r.email, tier, created_at: r.created_at, updated_at: r.updated_at, expires_at: r.expires_at, ...usageFields(tier, r.storage_bytes) };
  });
}

export async function listSubscriptions(env: Env) {
  const { results } = await env.DB.prepare(
    "SELECT s.user_id,u.email,u.tier,u.storage_bytes,s.expires_at,s.created_at,s.updated_at " +
      "FROM subscriptions s LEFT JOIN users u ON u.id=s.user_id ORDER BY s.updated_at DESC",
  ).all<{ user_id: string; email: string | null; tier: string | null; storage_bytes: number | null; expires_at: string; created_at: string; updated_at: string }>();
  return results.map((r) => {
    const tier = normalizeTierName(r.tier);
    return {
      user_id: r.user_id,
      email: r.email,
      tier,
      expires_at: r.expires_at,
      created_at: r.created_at,
      updated_at: r.updated_at,
      ...usageFields(tier, r.storage_bytes ?? 0),
    };
  });
}

export async function listCards(env: Env) {
  const { results } = await env.DB.prepare("SELECT code,duration_days,used_by,used_at,created_at FROM cards ORDER BY created_at DESC").all();
  return results;
}

// ---- 改密码 / 找回密码的验证码 ----

export function hashPasswordCode(email: string, purpose: string, code: string): Promise<string> {
  return sha256Hex(`${normalizeEmail(email)}:${purpose}:${code.trim()}`);
}

export function generatePasswordCode(): string {
  const n = crypto.getRandomValues(new Uint32Array(1))[0] % 10 ** PASSWORD_CODE_DIGITS;
  return String(n).padStart(PASSWORD_CODE_DIGITS, "0");
}

export async function passwordCodeCooldown(env: Env, email: string, purpose: string): Promise<number> {
  const row = await env.DB.prepare(
    "SELECT created_at FROM password_verification_codes WHERE email=? AND purpose=? ORDER BY created_at DESC LIMIT 1",
  )
    .bind(normalizeEmail(email), purpose)
    .first<{ created_at: string }>();
  if (!row) return 0;
  const remaining = PASSWORD_CODE_RESEND_COOLDOWN_SECONDS - (Date.now() - parseUtc(row.created_at)) / 1000;
  return remaining <= 0 ? 0 : Math.max(1, Math.ceil(remaining));
}

export async function createPasswordCode(env: Env, email: string, purpose: string, code: string): Promise<string> {
  const id = newId();
  const now = Date.now();
  await env.DB.prepare(
    "INSERT INTO password_verification_codes (id,email,purpose,code_hash,expires_at,attempts,used_at,created_at) VALUES (?,?,?,?,?,0,NULL,?)",
  )
    .bind(id, normalizeEmail(email), purpose, await hashPasswordCode(email, purpose, code), new Date(now + PASSWORD_CODE_TTL_MINUTES * 60_000).toISOString(), new Date(now).toISOString())
    .run();
  return id;
}

export async function deletePasswordCode(env: Env, id: string): Promise<void> {
  await env.DB.prepare("DELETE FROM password_verification_codes WHERE id=?").bind(id).run();
}

/** 校验验证码；成功或作废都会把它标为已用。返回错误文字，成功为 null。 */
export async function verifyPasswordCode(env: Env, email: string, purpose: string, code: string): Promise<string | null> {
  const normalizedEmail = normalizeEmail(email);
  const normalizedCode = code.trim();
  const row = await env.DB.prepare(
    "SELECT id,code_hash,expires_at,attempts FROM password_verification_codes " +
      "WHERE email=? AND purpose=? AND used_at IS NULL ORDER BY created_at DESC LIMIT 1",
  )
    .bind(normalizedEmail, purpose)
    .first<{ id: string; code_hash: string; expires_at: string; attempts: number }>();
  if (!row) return "invalid or expired code";

  const markUsed = () => env.DB.prepare("UPDATE password_verification_codes SET used_at=? WHERE id=? AND used_at IS NULL").bind(nowIso(), row.id).run();
  const attempts = row.attempts ?? 0;
  if (attempts >= PASSWORD_CODE_MAX_ATTEMPTS) {
    await markUsed();
    return "too many code attempts";
  }
  if (parseUtc(row.expires_at) <= Date.now()) {
    await markUsed();
    return "invalid or expired code";
  }
  const wellFormed = new RegExp(`^\\d{${PASSWORD_CODE_DIGITS}}$`).test(normalizedCode);
  if (!wellFormed || (await hashPasswordCode(normalizedEmail, purpose, normalizedCode)) !== row.code_hash) {
    await env.DB.prepare("UPDATE password_verification_codes SET attempts=? WHERE id=?").bind(attempts + 1, row.id).run();
    if (attempts + 1 >= PASSWORD_CODE_MAX_ATTEMPTS) {
      await markUsed();
      return "too many code attempts";
    }
    return "invalid or expired code";
  }
  await markUsed();
  return null;
}
