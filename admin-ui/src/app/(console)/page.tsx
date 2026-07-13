'use client';

import { useRouter } from 'next/navigation';
import { useEffect, useState } from 'react';
import { ConsoleShell } from '@/components/shell';
import { Alert, Badge, Card, DataTable, EmptyState, PageHeader, Skeleton, UsageBar } from '@/components/ui';
import { api, type AuditEntry, type CacheStats } from '@/lib/api';
import { useSession } from '@/lib/session-context';
import styles from './dashboard.module.css';

function formatWhen(iso: string): string {
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(iso));
}

function formatBytes(n?: number): string {
  if (n == null) return '—';
  if (n < 1024) return `${n} B`;
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(1)} KiB`;
  if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(1)} MiB`;
  return `${(n / 1024 ** 3).toFixed(2)} GiB`;
}

function outcomeTone(outcome: string): 'success' | 'warning' | 'critical' | 'neutral' {
  if (outcome === 'success') return 'success';
  if (outcome === 'denied') return 'warning';
  if (outcome === 'failure') return 'critical';
  return 'neutral';
}

export default function DashboardPage() {
  const { tenant, user } = useSession();
  const router = useRouter();
  const [audit, setAudit] = useState<AuditEntry[] | null>(null);
  const [modelCount, setModelCount] = useState<number | null>(null);
  const [cacheStats, setCacheStats] = useState<CacheStats | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    void (async () => {
      try {
        const [auditRes, modelsRes, statsRes] = await Promise.all([
          api.audit(8),
          api.models(500),
          api.cacheStats(),
        ]);
        setAudit(auditRes.items);
        setModelCount(modelsRes.items.length);
        setCacheStats(statsRes);
      } catch (e) {
        setError(e instanceof Error ? e.message : 'Could not load overview');
      }
    })();
  }, []);

  return (
    <ConsoleShell>
      <div className="animate-in">
        <PageHeader
          title={`Welcome${user?.email ? `, ${user.email.split('@')[0]}` : ''}`}
          subtitle="Monitor cache activity, credentials, and configuration for your deployment."
        />

        {error ? <Alert tone="critical">{error}</Alert> : null}

        <div className={styles.stats}>
          <Card className={styles.statCard}>
            <span className={styles.statLabel}>Tenant</span>
            <span className={styles.statValue}>{tenant?.display_name ?? '—'}</span>
            <span className={styles.statMeta}>Slug: {tenant?.name}</span>
          </Card>
          <Card className={styles.statCard}>
            <span className={styles.statLabel}>Cache usage</span>
            <span className={`${styles.statValue} tabular`}>
              {cacheStats === null
                ? '…'
                : cacheStats.quota_bytes > 0
                  ? `${formatBytes(cacheStats.used_bytes)} / ${formatBytes(cacheStats.quota_bytes)}`
                  : formatBytes(cacheStats.used_bytes)}
            </span>
            {cacheStats !== null && cacheStats.quota_bytes > 0 ? (
              <UsageBar fullWidth percent={(cacheStats.used_bytes / cacheStats.quota_bytes) * 100} />
            ) : null}
            <span className={styles.statMeta}>
              {modelCount === null || cacheStats === null
                ? 'From disk cache index'
                : `${modelCount} models · ${cacheStats.entry_count} cached objects`}
            </span>
          </Card>
          <Card className={styles.statCard}>
            <span className={styles.statLabel}>Your role</span>
            <span className={styles.statValue}>{user?.role ?? '—'}</span>
            <span className={styles.statMeta}>From identity provider groups</span>
          </Card>
        </div>

        <section className={styles.section} aria-labelledby="recent-audit">
          <div className={styles.sectionHead}>
            <h2 id="recent-audit" className={styles.sectionTitle}>
              Recent activity
            </h2>
            <button type="button" className={styles.linkBtn} onClick={() => router.push('/audit')}>
              View all
            </button>
          </div>

          {audit === null ? (
            <Card>
              <Skeleton height={120} />
            </Card>
          ) : audit.length === 0 ? (
            <EmptyState
              title="No activity yet"
              description="Mutations such as token creation and sign-ins will appear here."
            />
          ) : (
            <DataTable>
              <thead>
                <tr>
                  <th scope="col">When</th>
                  <th scope="col">Action</th>
                  <th scope="col">Resource</th>
                  <th scope="col">Outcome</th>
                </tr>
              </thead>
              <tbody>
                {audit.map((row) => (
                  <tr key={row.id}>
                    <td className="tabular">{formatWhen(row.occurred_at)}</td>
                    <td>{row.action}</td>
                    <td className={styles.mono}>{row.resource ?? '—'}</td>
                    <td>
                      <Badge tone={outcomeTone(row.outcome)}>{row.outcome}</Badge>
                    </td>
                  </tr>
                ))}
              </tbody>
            </DataTable>
          )}
        </section>
      </div>
    </ConsoleShell>
  );
}
