'use client';

import { useEffect, useMemo, useState } from 'react';
import { Button } from '@/components/ui';
import type { ModelGroup } from '@/lib/api';
import { buildDownloadCommands, proxyEndpointFromWindow } from '@/lib/download-commands';
import styles from './download-commands.module.css';

export function DownloadCommands({ group }: { group: ModelGroup }) {
  const [endpoint, setEndpoint] = useState('');
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    setEndpoint(proxyEndpointFromWindow(window.location));
  }, []);

  const code = useMemo(
    () => (endpoint ? buildDownloadCommands(group, endpoint) : ''),
    [group, endpoint],
  );

  async function copy() {
    if (!code) return;
    await navigator.clipboard.writeText(code);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 2000);
  }

  return (
    <div className={styles.codeBlock} data-testid="model-download-commands">
      <div className={styles.codeToolbar}>
        <span className={styles.codeLabel}>shell</span>
        <Button variant="ghost" size="sm" onClick={() => void copy()} disabled={!code}>
          {copied ? 'Copied' : 'Copy'}
        </Button>
      </div>
      <pre className={styles.codePre}>
        <code data-testid="model-download-command">{code || '…'}</code>
      </pre>
    </div>
  );
}
