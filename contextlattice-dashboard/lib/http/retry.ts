type RetryOptions = {
  retries?: number;
  minDelayMs?: number;
  maxDelayMs?: number;
  retryOn?: (response: Response) => boolean;
};

function sleep(ms: number) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

const defaultRetryOn = (response: Response) =>
  response.status === 408 ||
  response.status === 425 ||
  response.status === 429 ||
  response.status === 500 ||
  response.status === 502 ||
  response.status === 503 ||
  response.status === 504;

export async function fetchWithRetry(
  input: RequestInfo | URL,
  init: RequestInit,
  options: RetryOptions = {},
) {
  const retries = options.retries ?? 3;
  const minDelayMs = options.minDelayMs ?? 250;
  const maxDelayMs = options.maxDelayMs ?? 2000;
  const shouldRetry = options.retryOn ?? defaultRetryOn;

  let attempt = 0;
  while (true) {
    attempt += 1;
    const response = await fetch(input, init);
    if (!shouldRetry(response) || attempt > retries) {
      return response;
    }
    const jitter = Math.random() * 0.3 + 0.85;
    const baseDelay = Math.min(maxDelayMs, minDelayMs * 2 ** (attempt - 1));
    await sleep(Math.round(baseDelay * jitter));
  }
}
