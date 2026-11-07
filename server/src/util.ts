// 通用小工具：时间、JSON 响应、编码。

export const TOKEN_LEEWAY_SECONDS = 30;
export const ACCESS_TOKEN_HOURS = 2;
export const REFRESH_TOKEN_DAYS = 30;
export const ADMIN_TOKEN_HOURS = 12;
export const ADMIN_AUDIENCE = "maccy-admin";
export const PASSWORD_CODE_DIGITS = 6;
export const PASSWORD_CODE_TTL_MINUTES = 10;
export const PASSWORD_CODE_RESEND_COOLDOWN_SECONDS = 60;
export const PASSWORD_CODE_MAX_ATTEMPTS = 5;

const enc = new TextEncoder();

export function nowIso(): string {
  return new Date().toISOString();
}

export function isoAfter(ms: number, from = Date.now()): string {
  return new Date(from + ms).toISOString();
}

export function parseUtc(value: string | null | undefined): number {
  if (!value) return NaN;
  return Date.parse(value);
}

export const DAY_MS = 86_400_000;
export const HOUR_MS = 3_600_000;

export function json(data: unknown, status = 200, headers?: HeadersInit): Response {
  const h = new Headers(headers);
  h.set("Content-Type", "application/json");
  return new Response(JSON.stringify(data), { status, headers: h });
}

export function errorJson(status: number, message: string): Response {
  return json({ error: message }, status);
}

/** FastAPI 的 HTTPException 形状：{"detail": "..."} */
export function httpError(status: number): Response {
  const detail: Record<number, string> = { 401: "Unauthorized", 403: "Forbidden", 404: "Not Found", 405: "Method Not Allowed" };
  return json({ detail: detail[status] ?? "Error" }, status);
}

/** 在路由深处抛出，由入口转成响应。 */
export class HttpFailure extends Error {
  constructor(readonly response: Response) {
    super("http failure");
  }
}

export async function readJson(request: Request): Promise<Record<string, unknown> | null> {
  try {
    const value = await request.json();
    return value !== null && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

export function str(value: unknown): string {
  return value === null || value === undefined ? "" : String(value);
}

export function normalizeEmail(value: unknown): string {
  return str(value).trim().toLowerCase();
}

export function isValidEmail(email: string): boolean {
  return email.length > 0 && email.includes("@") && email.length <= 200;
}

export function asInt(value: unknown, fallback: number): number {
  const n = typeof value === "number" ? value : Number.parseInt(str(value), 10);
  return Number.isFinite(n) ? Math.trunc(n) : fallback;
}

export function clamp(value: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, value));
}

export function hex(bytes: ArrayBuffer | Uint8Array): string {
  const b = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  let out = "";
  for (const x of b) out += x.toString(16).padStart(2, "0");
  return out;
}

export function randomBytes(n: number): Uint8Array {
  return crypto.getRandomValues(new Uint8Array(n));
}

export function base64UrlEncode(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function base64UrlDecode(text: string): Uint8Array {
  const b64 = text.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((text.length + 3) % 4);
  return Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
}

export function utf8(text: string): Uint8Array {
  return enc.encode(text);
}

export async function sha256Hex(text: string): Promise<string> {
  return hex(await crypto.subtle.digest("SHA-256", utf8(text)));
}

export function normalizeBlobSha256(value: unknown): string | null {
  const text = str(value).trim().toLowerCase();
  return /^[0-9a-f]{64}$/.test(text) ? text : null;
}

export function newId(): string {
  return hex(randomBytes(16));
}

/** 和 Python 版一样：0-9 字母和 -_ 以外的字符换成 _。 */
export function safeSegment(value: string): string {
  const safe = value.replace(/[^A-Za-z0-9_-]/g, "_").replace(/^_+|_+$/g, "");
  return safe || "unknown";
}

export function sseEvent(name: string, data: unknown): string {
  return `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`;
}
