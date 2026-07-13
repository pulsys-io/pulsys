/** CSRF cookie name — must match backend auth.CSRFCookieName */
export const CSRF_COOKIE = 'pulsys_csrf';
export const CSRF_HEADER = 'X-Pulsys-CSRF';

/** Read the double-submit CSRF cookie set by the backend. */
export function readCSRFCookie(): string {
  if (typeof document === 'undefined') return '';
  const parts = document.cookie.split(';');
  for (const part of parts) {
    const [rawKey, ...rest] = part.trim().split('=');
    if (rawKey === CSRF_COOKIE) {
      return decodeURIComponent(rest.join('='));
    }
  }
  return '';
}

/** Ensure CSRF cookie is synced for an existing session. */
export async function syncCSRF(): Promise<string> {
  const res = await fetch('/auth/csrf', { credentials: 'include' });
  if (!res.ok) return readCSRFCookie();
  const body = (await res.json()) as { csrf_token?: string };
  return body.csrf_token ?? readCSRFCookie();
}

/** Headers for mutating API calls (double-submit). */
export function csrfHeaders(): Record<string, string> {
  const token = readCSRFCookie();
  if (!token) return {};
  return { [CSRF_HEADER]: token };
}

const MUTATING = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

export function isMutatingMethod(method?: string): boolean {
  return MUTATING.has((method ?? 'GET').toUpperCase());
}
