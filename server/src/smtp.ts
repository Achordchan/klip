// 最小的 SMTP 客户端：用 Workers 的 TCP socket 发验证码邮件。
// 支持 465（直接 SSL）、587（STARTTLS）以及不加密（只用于本地测试）。

import { connect } from "cloudflare:sockets";
import type { Env } from "./env";
import { envFlag } from "./env";
import { PASSWORD_CODE_TTL_MINUTES } from "./util";

export function isEmailConfigured(env: Env): boolean {
  return Boolean(env.SMTP_HOST?.trim() && Number(env.SMTP_PORT) > 0 && smtpFrom(env));
}

function smtpFrom(env: Env): string {
  return (env.SMTP_FROM?.trim() || env.SMTP_USER?.trim() || "").trim();
}

function b64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}

function encodedWord(text: string): string {
  return /^[\x20-\x7e]*$/.test(text) ? text : `=?UTF-8?B?${b64(text)}?=`;
}

function wrap76(text: string): string {
  return text.replace(/.{1,76}/g, "$&\r\n");
}

export function buildPasswordCodeMessage(env: Env, to: string, code: string, purpose: string): string {
  const action = purpose === "change" ? "修改密码" : "找回密码";
  const from = smtpFrom(env);
  const body = [
    `你正在为 Klip 云同步账号执行${action}操作。`,
    `验证码：${code}`,
    `验证码 ${PASSWORD_CODE_TTL_MINUTES} 分钟内有效，请勿转发给他人。`,
    "如果这不是你的操作，请忽略此邮件。",
  ].join("\r\n");
  const domain = from.split("@")[1] || "klip.local";
  return [
    `From: ${encodedWord(env.SMTP_FROM_NAME?.trim() || "Klip Cloud")} <${from}>`,
    `To: <${to}>`,
    `Subject: ${encodedWord(`Klip 云同步${action}验证码`)}`,
    `Date: ${new Date().toUTCString().replace("GMT", "+0000")}`,
    `Message-ID: <${crypto.randomUUID()}@${domain}>`,
    "MIME-Version: 1.0",
    "Content-Type: text/plain; charset=utf-8",
    "Content-Transfer-Encoding: base64",
    "",
    wrap76(b64(body)),
  ].join("\r\n");
}

class SmtpConnection {
  private reader!: ReadableStreamDefaultReader<Uint8Array>;
  private writer!: WritableStreamDefaultWriter<Uint8Array>;
  private buffer = "";
  private readonly decoder = new TextDecoder();
  private readonly encoder = new TextEncoder();

  constructor(private socket: Socket) {
    this.attach();
  }

  private attach(): void {
    this.reader = this.socket.readable.getReader();
    this.writer = this.socket.writable.getWriter();
  }

  /** 读一条完整的回复（多行回复以 "250 " 这样带空格的一行结束）。 */
  async reply(): Promise<{ code: number; text: string }> {
    for (;;) {
      const lines = this.buffer.split("\r\n");
      for (let i = 0; i < lines.length - 1; i++) {
        if (/^\d{3} /.test(lines[i]) || /^\d{3}$/.test(lines[i])) {
          const text = lines.slice(0, i + 1).join("\n");
          this.buffer = lines.slice(i + 1).join("\r\n");
          return { code: Number(lines[i].slice(0, 3)), text };
        }
      }
      const { value, done } = await this.reader.read();
      if (done) throw new Error(`smtp connection closed: ${this.buffer}`);
      this.buffer += this.decoder.decode(value, { stream: true });
    }
  }

  async send(line: string): Promise<void> {
    await this.writer.write(this.encoder.encode(line + "\r\n"));
  }

  async command(line: string, expect: number[]): Promise<string> {
    await this.send(line);
    const r = await this.reply();
    if (!expect.includes(r.code)) throw new Error(`smtp "${line.split(" ")[0]}" failed: ${r.text}`);
    return r.text;
  }

  async upgradeTls(): Promise<void> {
    this.reader.releaseLock();
    this.writer.releaseLock();
    this.socket = this.socket.startTls();
    this.attach();
  }

  async close(): Promise<void> {
    try {
      await this.socket.close();
    } catch {
      /* 已经关了 */
    }
  }
}

export async function sendMail(env: Env, to: string, message: string): Promise<void> {
  const ssl = envFlag(env.SMTP_SSL, false);
  const starttls = !ssl && envFlag(env.SMTP_STARTTLS, true);
  const socket = connect(
    { hostname: env.SMTP_HOST.trim(), port: Number(env.SMTP_PORT) },
    { secureTransport: ssl ? "on" : starttls ? "starttls" : "off", allowHalfOpen: false },
  );
  const conn = new SmtpConnection(socket);
  const timeout = new Promise<never>((_, reject) => setTimeout(() => reject(new Error("smtp timeout")), 15_000));
  const run = async () => {
    const greeting = await conn.reply();
    if (greeting.code !== 220) throw new Error(`smtp greeting: ${greeting.text}`);
    await conn.command("EHLO klip-sync", [250]);
    if (starttls) {
      await conn.command("STARTTLS", [220]);
      await conn.upgradeTls();
      await conn.command("EHLO klip-sync", [250]);
    }
    const user = env.SMTP_USER?.trim();
    if (user) {
      await conn.command("AUTH LOGIN", [334]);
      await conn.command(b64(user), [334]);
      await conn.command(b64(env.SMTP_PASSWORD ?? ""), [235]);
    }
    await conn.command(`MAIL FROM:<${smtpFrom(env)}>`, [250]);
    await conn.command(`RCPT TO:<${to}>`, [250, 251]);
    await conn.command("DATA", [354]);
    await conn.command(`${message}\r\n.`, [250]);
    try {
      await conn.command("QUIT", [221]);
    } catch {
      /* 邮件已经交出去了 */
    }
  };
  try {
    await Promise.race([run(), timeout]);
  } finally {
    await conn.close();
  }
}
