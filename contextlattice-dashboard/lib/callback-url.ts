const SAFE_CALLBACK_URL_FALLBACK = "/billing";
const SAFE_BASE_URL = "http://127.0.0.1";
const DECODE_MAX_ITERATIONS = 3;

const ENCODED_SEPARATOR_RX = /%(2e|2f|5c)/i;

function isObfuscatedPath(raw: string): boolean {
  return (
    raw.includes("\\") ||
    raw.includes("\u0000") ||
    raw.includes("\r") ||
    raw.includes("\n") ||
    raw.includes("..") ||
    ENCODED_SEPARATOR_RX.test(raw)
  );
}

function containsInjectedControlChars(value: string): boolean {
  return value.includes("\u0000") || value.includes("\r") || value.includes("\n");
}

function normalizeAndDecode(raw: string): string {
  let value = raw;
  for (let i = 0; i < DECODE_MAX_ITERATIONS; i += 1) {
    if (!value.includes("%")) {
      return value;
    }
    const decoded = decodeURIComponent(value);
    if (decoded === value) {
      return value;
    }
    if (containsInjectedControlChars(decoded) || isObfuscatedPath(decoded)) {
      return value;
    }
    value = decoded;
  }
  return value;
}

function isSafeRelativeUrl(rawValue: string): boolean {
  if (rawValue.length === 0) {
    return false;
  }

  if (rawValue === "/") {
    return true;
  }

  if (
    rawValue.startsWith("http://") ||
    rawValue.startsWith("https://") ||
    rawValue.startsWith("//") ||
    rawValue.includes("\\")
  ) {
    return false;
  }

  if (!rawValue.startsWith("/")) {
    return false;
  }

  if (rawValue.includes("..") || ENCODED_SEPARATOR_RX.test(rawValue)) {
    return false;
  }

  const decoded = normalizeAndDecode(rawValue);
  if (
    decoded.includes("..") ||
    decoded.includes("\\") ||
    decoded.includes("\u0000") ||
    decoded.includes("\r") ||
    decoded.includes("\n") ||
    ENCODED_SEPARATOR_RX.test(decoded)
  ) {
    return false;
  }

  return true;
}

export function sanitizeCallbackUrl(
  raw: string | null | undefined,
  fallback = SAFE_CALLBACK_URL_FALLBACK,
): string {
  if (!raw) {
    return fallback;
  }

  const rawValue = raw.trim();
  if (!rawValue || containsInjectedControlChars(rawValue) || isObfuscatedPath(rawValue)) {
    return fallback;
  }

  if (!isSafeRelativeUrl(rawValue)) {
    return fallback;
  }

  try {
    const parsed = new URL(rawValue, SAFE_BASE_URL);
    const parsedPath = parsed.pathname;
    const parsedQuery = parsed.search;
    const parsedHash = parsed.hash;

    if (
      parsed.origin !== SAFE_BASE_URL ||
      !parsedPath.startsWith("/") ||
      parsedPath.includes("..") ||
      containsInjectedControlChars(parsedPath) ||
      isObfuscatedPath(parsedPath) ||
      ENCODED_SEPARATOR_RX.test(parsedPath) ||
      containsInjectedControlChars(parsedQuery) ||
      containsInjectedControlChars(parsedHash)
    ) {
      return fallback;
    }

    return `${parsedPath}${parsedQuery}${parsedHash}`;
  } catch {
    return fallback;
  }
}
