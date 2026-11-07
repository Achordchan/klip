import type { UserStore } from "./user_store";

export interface Env {
  DB: D1Database;
  BUCKET: R2Bucket;
  USER: DurableObjectNamespace<UserStore>;
  ASSETS: Fetcher;

  AUTH_ISSUER: string;
  AUTH_AUDIENCE: string;
  AUTH_SIGNING_KEY: string;
  ADMIN_KEY: string;
  ADMIN_EMAIL: string;
  ADMIN_PASSWORD: string;

  SMTP_HOST: string;
  SMTP_PORT: string;
  SMTP_USER: string;
  SMTP_PASSWORD: string;
  SMTP_FROM: string;
  SMTP_FROM_NAME: string;
  SMTP_SSL: string;
  SMTP_STARTTLS: string;
}

export interface TierPolicy {
  name: string;
  retentionDays: number;
  storageLimitBytes: number;
}

const MB = 1024 * 1024;
const GB = 1024 * MB;

const TIERS: Record<string, TierPolicy> = {
  mini: { name: "Mini", retentionDays: 30, storageLimitBytes: 100 * MB },
  plus: { name: "Plus", retentionDays: 90, storageLimitBytes: 500 * MB },
  pro: { name: "Pro", retentionDays: 365, storageLimitBytes: 2 * GB },
  ultra: { name: "Ultra", retentionDays: 730, storageLimitBytes: 10 * GB },
};

export const SUPPORTED_TIERS = ["Mini", "Plus", "Pro", "Ultra"];

export function normalizeTierName(value: unknown): string | null {
  if (value === null || value === undefined) return null;
  return TIERS[String(value).trim().toLowerCase()]?.name ?? null;
}

export function getTierPolicy(value: unknown): TierPolicy | null {
  if (value === null || value === undefined) return null;
  return TIERS[String(value).trim().toLowerCase()] ?? null;
}

export function envFlag(value: string | undefined, fallback: boolean): boolean {
  if (value === undefined || value === "") return fallback;
  return ["1", "true", "yes", "on"].includes(value.trim().toLowerCase());
}
