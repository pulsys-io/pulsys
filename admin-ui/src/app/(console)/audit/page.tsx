'use client';

import { useEffect, useState } from 'react';
import { ConsoleShell } from '@/components/shell';
import { Alert, Badge, DataTable, EmptyState, PageHeader, Skeleton } from '@/components/ui';
import { api, type AuditEntry } from '@/lib/api';

function formatWhen(iso: string): string {
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'medium' }).format(
    new Date(iso),
  );
}

function outcomeTone(outcome: string): 'success' | 'warning' | 'critical' | 'neutral' {
  if (outcome === 'success') return 'success';
  if (outcome === 'denied') return 'warning';
  if (outcome === 'failure') return 'critical';
  return 'neutral';
}

export default function AuditPage() {
  const [items, setItems] = useState<AuditEntry[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    void api.audit(200).then(
      (r) => setItems(r.items),
      (e) => setError(e instanceof Error ? e.message : 'Failed to load audit log'),
    );
  }, []);

  return (
    <ConsoleShell>
      <div className="animate-in">
        <PageHeader
          title="Audit log"
          subtitle="Append-only record of sign-ins, token changes, and configuration updates."
        />
        {error ? <Alert tone="critical">{error}</Alert> : null}
        {items === null ? (
          <Skeleton height={200} />
        ) : items.length === 0 ? (
          <EmptyState
            title="No events recorded"
            description="Activity will appear here as users sign in and make changes."
          />
        ) : (
          <DataTable>
            <thead>
              <tr>
                <th scope="col">When</th>
                <th scope="col">Actor</th>
                <th scope="col">Action</th>
                <th scope="col">Resource</th>
                <th scope="col">Outcome</th>
              </tr>
            </thead>
            <tbody>
              {items.map((row) => (
                <tr key={row.id}>
                  <td className="tabular">{formatWhen(row.occurred_at)}</td>
                  <td>
                    {row.actor_type}
                    {row.actor_id ? (
                      <span style={{ color: 'var(--label-tertiary)', fontSize: 12 }}> · {row.actor_id.slice(0, 8)}…</span>
                    ) : null}
                  </td>
                  <td>{row.action}</td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{row.resource ?? '—'}</td>
                  <td>
                    <Badge tone={outcomeTone(row.outcome)}>{row.outcome}</Badge>
                  </td>
                </tr>
              ))}
            </tbody>
          </DataTable>
        )}
      </div>
    </ConsoleShell>
  );
}
