'use client';

import { useEffect, useState } from 'react';
import { ConsoleShell } from '@/components/shell';
import { Alert, Badge, DataTable, EmptyState, PageHeader, Skeleton } from '@/components/ui';
import { ApiError, api, type User } from '@/lib/api';

function formatWhen(iso: string): string {
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(new Date(iso));
}

export default function UsersPage() {
  const [items, setItems] = useState<User[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [forbidden, setForbidden] = useState(false);

  useEffect(() => {
    void api.users().then(
      (r) => setItems(r.items),
      (e) => {
        if (e instanceof ApiError && e.status === 403) {
          setForbidden(true);
          setItems([]);
        } else {
          setError(e instanceof Error ? e.message : 'Failed to load users');
        }
      },
    );
  }, []);

  return (
    <ConsoleShell>
      <div className="animate-in">
        <PageHeader
          title="Users"
          subtitle="Human accounts provisioned via OIDC. Roles map from your identity provider groups."
        />
        {error ? <Alert tone="critical">{error}</Alert> : null}
        {forbidden ? (
          <Alert tone="info">
            You need the <strong>admin</strong> role to view the user directory.
          </Alert>
        ) : null}
        {items === null ? (
          <Skeleton height={160} />
        ) : items.length === 0 && !forbidden ? (
          <EmptyState
            title="No users yet"
            description="Users appear after their first successful sign-in through your IdP."
          />
        ) : items.length > 0 ? (
          <DataTable>
            <thead>
              <tr>
                <th scope="col">Email</th>
                <th scope="col">Display name</th>
                <th scope="col">Role</th>
                <th scope="col">Status</th>
                <th scope="col">Joined</th>
              </tr>
            </thead>
            <tbody>
              {items.map((u) => (
                <tr key={u.id}>
                  <td>{u.email}</td>
                  <td>{u.display_name}</td>
                  <td>
                    <Badge tone="accent">{u.role}</Badge>
                  </td>
                  <td>
                    <Badge tone={u.is_active ? 'success' : 'critical'}>
                      {u.is_active ? 'Active' : 'Inactive'}
                    </Badge>
                  </td>
                  <td className="tabular">{formatWhen(u.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </DataTable>
        ) : null}
      </div>
    </ConsoleShell>
  );
}
