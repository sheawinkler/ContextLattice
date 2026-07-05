import crypto from "crypto";

export type PremiumDownloadAssetId = "macos" | "windows" | "linux";

export type PremiumDownloadAsset = {
  id: PremiumDownloadAssetId;
  label: string;
  fileName: string;
  url: string;
};

type TokenPayload = {
  a: PremiumDownloadAssetId;
  exp: number;
  uid?: string;
  p?: string;
};

const DOWNLOAD_ASSET_SPECS: Array<{
  id: PremiumDownloadAssetId;
  label: string;
  urlEnv: string;
  fileNameEnv: string;
  fallbackFileName: string;
}> = [
  {
    id: "macos",
    label: "macOS DMG",
    urlEnv: "PREMIUM_DOWNLOAD_MACOS_URL",
    fileNameEnv: "PREMIUM_DOWNLOAD_MACOS_FILENAME",
    fallbackFileName: "ContextLattice-premium-macOS.dmg",
  },
  {
    id: "windows",
    label: "Windows MSI",
    urlEnv: "PREMIUM_DOWNLOAD_WINDOWS_URL",
    fileNameEnv: "PREMIUM_DOWNLOAD_WINDOWS_FILENAME",
    fallbackFileName: "ContextLattice-premium-windows-x64.msi",
  },
  {
    id: "linux",
    label: "Linux Bundle",
    urlEnv: "PREMIUM_DOWNLOAD_LINUX_URL",
    fileNameEnv: "PREMIUM_DOWNLOAD_LINUX_FILENAME",
    fallbackFileName: "ContextLattice-premium-linux.tar.gz",
  },
];

function b64url(input: string | Buffer): string {
  return Buffer.from(input).toString("base64url");
}

function unb64url(input: string): string {
  return Buffer.from(input, "base64url").toString("utf8");
}

function hmacSignature(payloadB64: string, secret: string): string {
  return crypto.createHmac("sha256", secret).update(payloadB64).digest("base64url");
}

function secureEqual(a: string, b: string): boolean {
  if (!a || !b || a.length !== b.length) {
    return false;
  }
  let diff = 0;
  for (let i = 0; i < a.length; i += 1) {
    diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return diff === 0;
}

export function configuredPremiumDownloads(): PremiumDownloadAsset[] {
  return DOWNLOAD_ASSET_SPECS.map((spec) => {
    const url = String(process.env[spec.urlEnv] || "").trim();
    const fileName =
      String(process.env[spec.fileNameEnv] || "").trim() || spec.fallbackFileName;
    return {
      id: spec.id,
      label: spec.label,
      fileName,
      url,
    };
  }).filter((asset) => Boolean(asset.url));
}

export function premiumDownloadById(
  assetId: string,
): PremiumDownloadAsset | null {
  const normalized = String(assetId || "").trim().toLowerCase();
  return (
    configuredPremiumDownloads().find((asset) => asset.id === normalized) || null
  );
}

function downloadTokenSecret(): string {
  return (
    process.env.CONTEXTLATTICE_DOWNLOAD_TOKEN_SECRET ||
    process.env.CONTEXTLATTICE_ADMIN_API_KEY ||
    process.env.GO_PAID_ENTITLEMENT_KEY ||
    process.env.NEXTAUTH_SECRET ||
    ""
  ).trim();
}

export function defaultDownloadTokenTtlMinutes(): number {
  const raw = Number(process.env.PREMIUM_DOWNLOAD_TOKEN_TTL_MINUTES || 120);
  if (!Number.isFinite(raw)) {
    return 120;
  }
  return Math.min(24 * 60, Math.max(5, Math.floor(raw)));
}

export function issueDownloadToken(
  assetId: PremiumDownloadAssetId,
  options?: { ttlMinutes?: number; userId?: string; planId?: string },
): string {
  const secret = downloadTokenSecret();
  if (!secret) {
    throw new Error("Download token secret is not configured");
  }
  const ttlMinutes = Math.min(
    24 * 60,
    Math.max(5, Math.floor(options?.ttlMinutes || defaultDownloadTokenTtlMinutes())),
  );
  const payload: TokenPayload = {
    a: assetId,
    exp: Date.now() + ttlMinutes * 60_000,
    uid: options?.userId,
    p: options?.planId,
  };
  const payloadB64 = b64url(JSON.stringify(payload));
  const sig = hmacSignature(payloadB64, secret);
  return `${payloadB64}.${sig}`;
}

export function verifyDownloadToken(token: string): TokenPayload | null {
  const secret = downloadTokenSecret();
  if (!secret) {
    return null;
  }
  const [payloadB64, sig] = String(token || "").split(".");
  if (!payloadB64 || !sig) {
    return null;
  }
  const expected = hmacSignature(payloadB64, secret);
  if (!secureEqual(sig, expected)) {
    return null;
  }
  try {
    const parsed = JSON.parse(unb64url(payloadB64)) as TokenPayload;
    if (!parsed?.a || !parsed?.exp) {
      return null;
    }
    if (Date.now() > parsed.exp) {
      return null;
    }
    return parsed;
  } catch {
    return null;
  }
}

export function appOrigin(): string {
  return (
    String(process.env.APP_URL || "").trim() ||
    String(process.env.NEXTAUTH_URL || "").trim() ||
    "http://localhost:3000"
  );
}
