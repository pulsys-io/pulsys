'use client';

import { useCallback, useEffect, useState } from 'react';
import { ConsoleShell } from '@/components/shell';
import {
  Alert,
  Badge,
  Button,
  DataTable,
  EmptyState,
  Field,
  Input,
  Modal,
  PageHeader,
  Skeleton,
} from '@/components/ui';
import { api, type Token, type TokenCreateResult } from '@/lib/api';

function formatWhen(iso?: string): string {
  if (!iso) return '—';
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(iso),
  );
}

export default function TokensPage() {
  const [items, setItems] = useState<Token[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [name, setName] = useState('');
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<TokenCreateResult | null>(null);
  const [revoking, setRevoking] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const r = await api.tokens();
      setItems(r.items);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load tokens');
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function onCreate(e: React.FormEvent) {
    e.preventDefault();
    if (!name.trim()) return;
    setCreating(true);
    setError(null);
    try {
      const res = await api.createToken({ name: name.trim(), scopes: ['admin:read'] });
      setCreated(res);
      setShowCreate(false);
      setName('');
      await load();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not create token');
    } finally {
      setCreating(false);
    }
  }

  async function onRevoke(id: string) {
    if (!confirm('Revoke this token? Applications using it will lose access immediately.')) return;
    setRevoking(id);
    try {
      await api.revokeToken(id);
      await load();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not revoke token');
    } finally {
      setRevoking(null);
    }
  }

  return (
    <ConsoleShell>
      <div className="animate-in">
        <PageHeader
          title="API tokens"
          subtitle="Machine credentials for automation. Each token is shown once at creation."
          actions={<Button onClick={() => setShowCreate(true)}>Create token</Button>}
        />

        {error ? <Alert tone="critical">{error}</Alert> : null}

        {created ? (
          <Alert tone="success">
            <strong>Token created.</strong> Copy it now — it will not be shown again.
            <pre
              style={{
                marginTop: 12,
                padding: 12,
                borderRadius: 8,
                background: 'var(--fill-secondary)',
                fontFamily: 'var(--font-mono)',
                fontSize: 13,
                overflow: 'auto',
              }}
            >
              {created.secret}
            </pre>
            <Button variant="secondary" onClick={() => setCreated(null)}>
              Dismiss
            </Button>
          </Alert>
        ) : null}

        {items === null ? (
          <Skeleton height={160} />
        ) : items.length === 0 ? (
          <EmptyState
            title="No API tokens"
            description="Create a token for CI pipelines, scripts, or integrations that call the admin API."
            action={<Button onClick={() => setShowCreate(true)}>Create token</Button>}
          />
        ) : (
          <DataTable>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Prefix</th>
                <th scope="col">Created</th>
                <th scope="col">Status</th>
                <th scope="col" />
              </tr>
            </thead>
            <tbody>
              {items.map((t) => (
                <tr key={t.id}>
                  <td>{t.name}</td>
                  <td className="tabular" style={{ fontFamily: 'var(--font-mono)', fontSize: 13 }}>
                    pulsys_{t.prefix}_…
                  </td>
                  <td className="tabular">{formatWhen(t.created_at)}</td>
                  <td>
                    <Badge tone={t.revoked_at ? 'critical' : 'success'}>
                      {t.revoked_at ? 'Revoked' : 'Active'}
                    </Badge>
                  </td>
                  <td>
                    {!t.revoked_at ? (
                      <Button
                        variant="ghost"
                        loading={revoking === t.id}
                        onClick={() => void onRevoke(t.id)}
                      >
                        Revoke
                      </Button>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </DataTable>
        )}

        {showCreate ? (
          <Modal
            title="Create API token"
            onClose={() => setShowCreate(false)}
            footer={
              <>
                <Button variant="ghost" onClick={() => setShowCreate(false)}>
                  Cancel
                </Button>
                <Button type="submit" form="create-token" loading={creating}>
                  Create
                </Button>
              </>
            }
          >
            <form id="create-token" onSubmit={(e) => void onCreate(e)}>
              <Field label="Name" hint="A label to identify this token in audit logs.">
                <Input
                  id="token-name"
                  name="name"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="CI deploy"
                  autoFocus
                  required
                />
              </Field>
            </form>
          </Modal>
        ) : null}
      </div>
    </ConsoleShell>
  );
}
