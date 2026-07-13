import { CSRF_HEADER, readCSRFCookie } from './csrf';
import { generatePKCE, OIDC_STATE_KEY, PKCE_VERIFIER_KEY, randomState } from './pkce';

export type OIDCConfig = {
  issuer: string;
  client_id: string;
  redirect_uri: string;
  scopes: string[];
};

export type SessionUser = {
  user_id: string;
  email: string;
  role: string;
  csrf_token?: string;
};

const USER_STORAGE_KEY = 'pulsys_user';

export function getStoredUser(): SessionUser | null {
  if (typeof window === 'undefined') return null;
  try {
    const raw = sessionStorage.getItem(USER_STORAGE_KEY);
    return raw ? (JSON.parse(raw) as SessionUser) : null;
  } catch {
    return null;
  }
}

export function storeUser(user: SessionUser): void {
  sessionStorage.setItem(USER_STORAGE_KEY, JSON.stringify(user));
}

export function clearStoredUser(): void {
  sessionStorage.removeItem(USER_STORAGE_KEY);
}

export async function fetchOIDCConfig(): Promise<OIDCConfig> {
  const res = await fetch('/auth/oidc/config', { credentials: 'include' });
  if (!res.ok) {
    throw new Error(await readError(res, 'OIDC is not configured for this deployment'));
  }
  return res.json() as Promise<OIDCConfig>;
}

export async function beginOIDCLogin(): Promise<void> {
  const cfg = await fetchOIDCConfig();
  const { verifier, challenge } = await generatePKCE();
  const state = randomState();
  sessionStorage.setItem(PKCE_VERIFIER_KEY, verifier);
  sessionStorage.setItem(OIDC_STATE_KEY, state);

  const discovery = await fetch(`${cfg.issuer.replace(/\/$/, '')}/.well-known/openid-configuration`);
  if (!discovery.ok) {
    throw new Error('Could not reach your identity provider');
  }
  const meta = (await discovery.json()) as { authorization_endpoint: string };
  const scope = cfg.scopes.join(' ');
  const params = new URLSearchParams({
    client_id: cfg.client_id,
    redirect_uri: cfg.redirect_uri,
    response_type: 'code',
    scope,
    state,
    code_challenge: challenge,
    code_challenge_method: 'S256',
  });
  window.location.assign(`${meta.authorization_endpoint}?${params}`);
}

export async function completeOIDCCallback(code: string, state: string): Promise<SessionUser> {
  const expectedState = sessionStorage.getItem(OIDC_STATE_KEY);
  const verifier = sessionStorage.getItem(PKCE_VERIFIER_KEY);
  sessionStorage.removeItem(OIDC_STATE_KEY);
  sessionStorage.removeItem(PKCE_VERIFIER_KEY);

  if (!expectedState || state !== expectedState) {
    throw new Error('Sign-in state mismatch. Try again.');
  }
  if (!verifier) {
    throw new Error('Sign-in session expired. Try again.');
  }

  const cfg = await fetchOIDCConfig();
  const discovery = await fetch(`${cfg.issuer.replace(/\/$/, '')}/.well-known/openid-configuration`);
  const meta = (await discovery.json()) as { token_endpoint: string };

  const tokenRes = await fetch(meta.token_endpoint, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({
      grant_type: 'authorization_code',
      client_id: cfg.client_id,
      code,
      redirect_uri: cfg.redirect_uri,
      code_verifier: verifier,
    }),
  });
  if (!tokenRes.ok) {
    throw new Error(await readError(tokenRes, 'Identity provider rejected the sign-in'));
  }
  const tokens = (await tokenRes.json()) as { id_token?: string };
  if (!tokens.id_token) {
    throw new Error('Identity provider did not return an ID token');
  }

  const sessionRes = await fetch('/auth/session', {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id_token: tokens.id_token }),
  });
  if (!sessionRes.ok) {
    throw new Error(await readError(sessionRes, 'Could not establish a Pulsys session'));
  }
  const user = (await sessionRes.json()) as SessionUser;
  storeUser(user);
  return user;
}

export async function logout(): Promise<void> {
  const headers: Record<string, string> = {};
  const csrf = readCSRFCookie();
  if (csrf) headers[CSRF_HEADER] = csrf;
  await fetch('/auth/logout', { method: 'POST', credentials: 'include', headers });
  clearStoredUser();
}

async function readError(res: Response, fallback: string): Promise<string> {
  try {
    const text = await res.text();
    if (text.startsWith('{')) {
      const j = JSON.parse(text) as { error?: string };
      if (j.error) return j.error;
    }
    if (text && text.length < 200) return text;
  } catch {
    /* ignore */
  }
  return fallback;
}
