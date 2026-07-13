'use client';

import { useRouter } from 'next/navigation';
import { useEffect, useState } from 'react';
import { Button } from '@/components/ui';
import { beginOIDCLogin } from '@/lib/oidc';
import { useSession } from '@/lib/session-context';
import styles from './login.module.css';

export default function LoginPage() {
  const { tenant, loading } = useSession();
  const router = useRouter();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!loading && tenant) {
      router.replace('/');
    }
  }, [loading, tenant, router]);

  async function onSignIn() {
    setBusy(true);
    setError(null);
    try {
      await beginOIDCLogin();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Sign-in failed');
      setBusy(false);
    }
  }

  return (
    <div className={styles.page}>
      <div className={styles.panel}>
        <div className={styles.mark} aria-hidden />
        <h1 className={styles.title}>Pulsys Console</h1>
        <p className={styles.lead}>
          Sign in with your organization&apos;s identity provider. Pulsys never stores passwords —
          authentication is handled entirely by your IdP.
        </p>

        {error ? (
          <p className={styles.error} role="alert">
            {error}
          </p>
        ) : null}

        <Button size="lg" loading={busy} onClick={() => void onSignIn()} className={styles.cta}>
          Continue with SSO
        </Button>

        <p className={styles.footnote}>
          API tokens for automation are managed after sign-in under{' '}
          <span className={styles.footnoteEm}>API tokens</span>.
        </p>
      </div>
    </div>
  );
}
