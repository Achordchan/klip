// JWT（HS256）与密码哈希（ASP.NET Core Identity v3 格式）。
//
// JWT 签名很便宜，放在 Worker 里做；密码哈希每次要几十毫秒 CPU，
// 超过免费版 Worker 每个请求 10ms 的上限，只在 Durable Object 里调用。

import { base64UrlDecode, base64UrlEncode, utf8 } from "./util";

export type Claims = Record<string, unknown>;

async function hmacKey(secret: string): Promise<CryptoKey> {
  return crypto.subtle.importKey("raw", utf8(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign", "verify"]);
}

export async function signJwt(claims: Claims, secret: string): Promise<string> {
  const header = base64UrlEncode(utf8(JSON.stringify({ alg: "HS256", typ: "JWT" })));
  const payload = base64UrlEncode(utf8(JSON.stringify(claims)));
  const input = `${header}.${payload}`;
  const sig = await crypto.subtle.sign("HMAC", await hmacKey(secret), utf8(input));
  return `${input}.${base64UrlEncode(new Uint8Array(sig))}`;
}

/** 校验签名、aud、iss、exp、nbf（带容差）。不通过返回 null。 */
export async function verifyJwt(
  token: string,
  secret: string,
  expect: { audience: string; issuer: string; leewaySeconds: number },
): Promise<Claims | null> {
  const parts = token.split(".");
  if (parts.length !== 3) return null;
  try {
    const header = JSON.parse(new TextDecoder().decode(base64UrlDecode(parts[0]))) as Claims;
    if (header.alg !== "HS256") return null;
    const ok = await crypto.subtle.verify("HMAC", await hmacKey(secret), base64UrlDecode(parts[2]), utf8(`${parts[0]}.${parts[1]}`));
    if (!ok) return null;
    const claims = JSON.parse(new TextDecoder().decode(base64UrlDecode(parts[1]))) as Claims;
    const now = Math.floor(Date.now() / 1000);
    const leeway = expect.leewaySeconds;
    if (typeof claims.exp === "number" && claims.exp + leeway <= now) return null;
    if (typeof claims.nbf === "number" && claims.nbf - leeway > now) return null;
    if (typeof claims.iat === "number" && claims.iat - leeway > now) return null;
    if (claims.iss !== expect.issuer) return null;
    const aud = claims.aud;
    if (Array.isArray(aud) ? !aud.includes(expect.audience) : aud !== expect.audience) return null;
    return claims;
  } catch {
    return null;
  }
}

// ---- 密码哈希 ----
// 格式：0x01 | prf(u32 BE) | iter(u32 BE) | saltLen(u32 BE) | salt | subkey，整体 base64。
// prf：0 = SHA-1，1 = SHA-256，2 = SHA-512。

const PRF_HASH: Record<number, string> = { 0: "SHA-1", 1: "SHA-256", 2: "SHA-512" };
const DEFAULT_PRF = 2;
const DEFAULT_ITERATIONS = 100_000; // workerd 的 PBKDF2 上限也是 100000
const SALT_SIZE = 16;
const SUBKEY_LENGTH = 32;

async function pbkdf2(password: string, salt: Uint8Array, iterations: number, hash: string, length: number): Promise<Uint8Array> {
  const key = await crypto.subtle.importKey("raw", utf8(password), "PBKDF2", false, ["deriveBits"]);
  const bits = await crypto.subtle.deriveBits({ name: "PBKDF2", hash, salt, iterations }, key, length * 8);
  return new Uint8Array(bits);
}

function u32(view: DataView, offset: number): number {
  return view.getUint32(offset, false);
}

export async function hashPassword(password: string): Promise<string> {
  const salt = crypto.getRandomValues(new Uint8Array(SALT_SIZE));
  const subkey = await pbkdf2(password, salt, DEFAULT_ITERATIONS, PRF_HASH[DEFAULT_PRF], SUBKEY_LENGTH);
  const out = new Uint8Array(13 + salt.length + subkey.length);
  const view = new DataView(out.buffer);
  out[0] = 0x01;
  view.setUint32(1, DEFAULT_PRF, false);
  view.setUint32(5, DEFAULT_ITERATIONS, false);
  view.setUint32(9, salt.length, false);
  out.set(salt, 13);
  out.set(subkey, 13 + salt.length);
  let s = "";
  for (const b of out) s += String.fromCharCode(b);
  return btoa(s);
}

function timingSafeEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i];
  return diff === 0;
}

export async function verifyPassword(stored: string, password: string): Promise<boolean> {
  let decoded: Uint8Array;
  try {
    decoded = Uint8Array.from(atob(stored), (c) => c.charCodeAt(0));
  } catch {
    return false;
  }
  if (decoded.length < 13 || decoded[0] !== 0x01) return false;
  const view = new DataView(decoded.buffer, decoded.byteOffset, decoded.byteLength);
  const prf = u32(view, 1);
  const iterations = u32(view, 5);
  const saltLength = u32(view, 9);
  const hash = PRF_HASH[prf];
  if (!hash || saltLength < 16 || iterations < 1 || iterations > DEFAULT_ITERATIONS) return false;
  if (decoded.length < 13 + saltLength + 16) return false;
  const salt = decoded.slice(13, 13 + saltLength);
  const expected = decoded.slice(13 + saltLength);
  const actual = await pbkdf2(password, salt, iterations, hash, expected.length);
  return timingSafeEqual(expected, actual);
}
