'use client';

import Link from 'next/link';
import { usePathname, useRouter } from 'next/navigation';
import { type ReactNode } from 'react';
import { useSession } from '@/lib/session-context';
import styles from './shell.module.css';

const NAV = [
  { href: '/', label: 'Overview', icon: '◉' },
  { href: '/models', label: 'Models', icon: '▦' },
  { href: '/imports', label: 'Imports', icon: '↓' },
  { href: '/tokens', label: 'API tokens', icon: '⬡' },
  { href: '/audit', label: 'Audit log', icon: '☰' },
  { href: '/users', label: 'Users', icon: '◎' },
] as const;

export function ConsoleShell({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  const router = useRouter();
  const { user, tenant, loading, signOut } = useSession();

  if (loading) {
    return (
      <div className={styles.boot} role="status" aria-live="polite">
        <div className={styles.bootMark} aria-hidden />
        <p>Loading console…</p>
      </div>
    );
  }

  if (!tenant) {
    router.replace('/login');
    return null;
  }

  return (
    <div className={styles.root}>
      <aside className={styles.sidebar} aria-label="Console navigation">
        <div className={styles.brand}>
          <span className={styles.brandMark} aria-hidden />
          <div>
            <span className={styles.brandName}>pulsys</span>
            <span className={styles.brandTenant}>{tenant.display_name}</span>
          </div>
        </div>
        <nav className={styles.nav}>
          <ul className={styles.navList}>
            {NAV.map((item) => {
              const active = pathname === item.href || (item.href !== '/' && pathname.startsWith(item.href));
              return (
                <li key={item.href}>
                  <Link
                    href={item.href}
                    className={`${styles.navLink} ${active ? styles.navLinkActive : ''}`}
                    aria-current={active ? 'page' : undefined}
                  >
                    <span className={styles.navIcon} aria-hidden>
                      {item.icon}
                    </span>
                    {item.label}
                  </Link>
                </li>
              );
            })}
          </ul>
        </nav>
        <div className={styles.sidebarFoot}>
          {user ? (
            <div className={styles.userChip}>
              <span className={styles.userEmail} title={user.email}>
                {user.email}
              </span>
              <span className={styles.userRole}>{user.role}</span>
            </div>
          ) : null}
          <button
            type="button"
            className={styles.signOut}
            onClick={() => void signOut().then(() => router.replace('/login'))}
          >
            Sign out
          </button>
        </div>
      </aside>
      <main className={styles.main} id="main-content">
        <div className={styles.mainInner}>{children}</div>
      </main>
    </div>
  );
}
