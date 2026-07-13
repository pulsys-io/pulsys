'use client';

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react';
import { ApiError, api, type Tenant } from '@/lib/api';
import { getStoredUser, logout as oidcLogout, type SessionUser } from '@/lib/oidc';
import { syncCSRF } from '@/lib/csrf';

type SessionState = {
  user: SessionUser | null;
  tenant: Tenant | null;
  loading: boolean;
  error: string | null;
  refresh: () => Promise<void>;
  signOut: () => Promise<void>;
};

const SessionContext = createContext<SessionState | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<SessionUser | null>(null);
  const [tenant, setTenant] = useState<Tenant | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const t = await api.tenant();
      setTenant(t);
      await syncCSRF();
      setUser(getStoredUser());
    } catch (e) {
      setTenant(null);
      setUser(null);
      if (e instanceof ApiError && e.status === 401) {
        setError(null);
      } else {
        setError(e instanceof Error ? e.message : 'Could not load session');
      }
    } finally {
      setLoading(false);
    }
  }, []);

  const signOut = useCallback(async () => {
    await oidcLogout();
    setUser(null);
    setTenant(null);
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const value = useMemo(
    () => ({ user, tenant, loading, error, refresh, signOut }),
    [user, tenant, loading, error, refresh, signOut],
  );

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionState {
  const ctx = useContext(SessionContext);
  if (!ctx) throw new Error('useSession must be used within SessionProvider');
  return ctx;
}
