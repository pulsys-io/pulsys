'use client';

import { useSearchParams } from 'next/navigation';
import { Suspense, useEffect, useState } from 'react';
import { completeOIDCCallback } from '@/lib/oidc';
import styles from '../../../login/login.module.css';

function CallbackInner() {
  const params = useSearchParams();
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const code = params.get('code');
    const state = params.get('state');
    const err = params.get('error_description') ?? params.get('error');

    if (err) {
      setError(err);
      return;
    }
    if (!code || !state) {
      setError('Missing authorization response from identity provider.');
      return;
    }

    void completeOIDCCallback(code, state)
      .then(() => {
        window.location.assign('/');
      })
      .catch((e) => setError(e instanceof Error ? e.message : 'Sign-in failed'));
  }, [params]);

  if (error) {
    return (
      <div className={styles.page}>
        <div className={styles.panel}>
          <h1 className={styles.title}>Sign-in failed</h1>
          <p className={styles.error} role="alert">
            {error}
          </p>
          <a href="/login">Try again</a>
        </div>
      </div>
    );
  }

  return (
    <div className={styles.page}>
      <div className={styles.panel} role="status" aria-live="polite">
        <div className={styles.mark} aria-hidden />
        <h1 className={styles.title}>Completing sign-in</h1>
        <p className={styles.lead}>Verifying your identity and starting a secure session…</p>
      </div>
    </div>
  );
}

export default function OIDCCallbackPage() {
  return (
    <Suspense
      fallback={
        <div className={styles.page}>
          <div className={styles.panel} role="status">
            <p className={styles.lead}>Loading…</p>
          </div>
        </div>
      }
    >
      <CallbackInner />
    </Suspense>
  );
}
