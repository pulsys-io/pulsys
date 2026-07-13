'use client';

import Link from 'next/link';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { ConsoleShell } from '@/components/shell';
import {
  Alert,
  Button,
  Card,
  DataTable,
  Disclosure,
  EmptyState,
  Input,
  Modal,
  PageHeader,
  Skeleton,
  UsageBar,
} from '@/components/ui';
import { api, type CacheStats, type GroupedListing, type ModelGroup } from '@/lib/api';
import { DownloadCommands } from '@/components/download-commands';
// Import-creation lives on /imports.  We keep the Link import here
// to render a "Queue import" affordance in the PageHeader so users
// arriving on /models with intent-to-add still have a one-click
// path to the form, without duplicating the form itself.
import styles from './models.module.css';

const CHART_COLORS = [
  'var(--accent)',
  '#5856d6',
  '#34c759',
  '#ff9500',
  '#ff2d55',
  '#64d2ff',
  '#bf5af2',
  '#ffd60a',
  'var(--label-tertiary)',
];

function formatBytes(n?: number): string {
  if (n == null) return '—';
  if (n < 1024) return `${n} B`;
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(1)} KiB`;
  if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(1)} MiB`;
  return `${(n / 1024 ** 3).toFixed(2)} GiB`;
}

function formatExactBytes(n: number): string {
  return `${new Intl.NumberFormat().format(n)} bytes`;
}

function formatPercent(n: number): string {
  return `${Math.round(n)}%`;
}

function modelLabel(g: ModelGroup): string {
  return `${g.org}/${g.name}`;
}

function shortRevision(rev: string): string {
  if (rev.length <= 12) return rev;
  return rev.slice(0, 8);
}

function RevisionSummary({ revisions }: { revisions?: string[] | null }) {
  const safeRevisions = revisions ?? [];
  const visible = safeRevisions.slice(0, 2);
  const hidden = Math.max(0, safeRevisions.length - visible.length);

  return (
    <div className={styles.revisionSummary}>
      <span className={styles.revisionCount}>
        {safeRevisions.length} {safeRevisions.length === 1 ? 'revision' : 'revisions'}
      </span>
      <span className={styles.revisionChips}>
        {visible.map((rev) => (
          <span key={rev} className={styles.revisionChip} title={rev}>
            {shortRevision(rev)}
          </span>
        ))}
        {hidden > 0 ? <span className={styles.revisionMore}>+{hidden}</span> : null}
      </span>
    </div>
  );
}

function RevisionDetails({ revisions }: { revisions?: string[] | null }) {
  const safeRevisions = revisions ?? [];
  if (safeRevisions.length === 0) return null;

  return (
    <section className={styles.detailBlock} aria-label="Cached revisions">
      <div className={styles.detailTitle}>Cached revisions</div>
      <div className={styles.fullRevisionList}>
        {safeRevisions.map((rev) => (
          <code key={rev} className={styles.fullRevision}>
            {rev}
          </code>
        ))}
      </div>
    </section>
  );
}

type SortKey = 'size-desc' | 'size-asc' | 'name-asc' | 'files-desc';
type MinSizeKey = 'none' | '10mb' | '100mb' | '1gb';

const MIN_SIZE_BYTES: Record<MinSizeKey, number> = {
  none: 0,
  '10mb': 10 * 1024 ** 2,
  '100mb': 100 * 1024 ** 2,
  '1gb': 1024 ** 3,
};

type StorageSegment = { label: string; bytes: number; pct: number };

function StorageDonut({ segments, total }: { segments: StorageSegment[]; total: number }) {
  // Donut math: a single ring stroked with stroke-dasharray to cut
  // each segment's arc. We rotate -90° so the first slice starts at
  // 12 o'clock, matching Apple's Storage / Time Machine breakdowns.
  const size = 184;
  const stroke = 22;
  const radius = (size - stroke) / 2;
  const circumference = 2 * Math.PI * radius;
  // Small visual gap (px along the arc) between adjacent slices.
  const gap = 1.5;
  let offset = 0;

  return (
    <div className={styles.donutWrap}>
      <svg
        className={styles.donut}
        viewBox={`0 0 ${size} ${size}`}
        width={size}
        height={size}
        role="img"
        aria-label="Share of total cache storage by model"
      >
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke="var(--fill-secondary)"
          strokeWidth={stroke}
        />
        <g transform={`rotate(-90 ${size / 2} ${size / 2})`}>
          {segments.map((seg, i) => {
            const arc = (seg.pct / 100) * circumference;
            // Always render at least a hair-thin slice for non-zero
            // segments, otherwise they vanish into the gap.
            const drawn = seg.pct > 0 ? Math.max(arc - gap, 0.75) : 0;
            const dash = `${drawn} ${circumference}`;
            const el = (
              <circle
                key={seg.label}
                cx={size / 2}
                cy={size / 2}
                r={radius}
                fill="none"
                stroke={CHART_COLORS[i % CHART_COLORS.length]}
                strokeWidth={stroke}
                strokeDasharray={dash}
                strokeDashoffset={-offset}
              >
                <title>{`${seg.label}: ${formatBytes(seg.bytes)} (${seg.pct.toFixed(1)}%)`}</title>
              </circle>
            );
            offset += arc;
            return el;
          })}
        </g>
      </svg>
      <div className={styles.donutCenter} aria-hidden="true">
        <span className={styles.donutTotalLabel}>Total</span>
        <span className={`${styles.donutTotalValue} tabular`}>{formatBytes(total)}</span>
      </div>
    </div>
  );
}

function StorageOverview({ listing, stats }: { listing: GroupedListing; stats: CacheStats | null }) {
  const items = listing.items ?? [];
  const fileCount = items.reduce((n, g) => n + g.file_count, 0);
  const quotaBytes = stats?.quota_bytes ?? 0;
  const usedBytes = stats?.used_bytes ?? listing.grand_total_bytes;
  const quotaPct = quotaBytes > 0 ? (usedBytes / quotaBytes) * 100 : 0;

  const segments: StorageSegment[] = useMemo(() => {
    // Cap at 6 slices + Other so the donut stays readable; a pie
    // with 9 thin wedges loses meaning fast.
    const top = items.slice(0, 6);
    const topBytes = top.reduce((n, g) => n + g.total_bytes, 0);
    const otherBytes = Math.max(0, listing.grand_total_bytes - topBytes);
    const out: StorageSegment[] = top.map((g) => ({
      label: modelLabel(g),
      bytes: g.total_bytes,
      pct: listing.grand_total_bytes > 0 ? (g.total_bytes / listing.grand_total_bytes) * 100 : 0,
    }));
    if (otherBytes > 0) {
      out.push({
        label: 'Other',
        bytes: otherBytes,
        pct: listing.grand_total_bytes > 0 ? (otherBytes / listing.grand_total_bytes) * 100 : 0,
      });
    }
    return out;
  }, [items, listing.grand_total_bytes]);

  return (
    <Card className={styles.overview}>
      <div className={styles.stats}>
        <div className={styles.statCard}>
          <span className={styles.statLabel}>Cache usage</span>
          <span className={`${styles.statValue} tabular`}>
            {quotaBytes > 0 ? `${formatBytes(usedBytes)} / ${formatBytes(quotaBytes)}` : formatBytes(usedBytes)}
          </span>
          {quotaBytes > 0 ? <UsageBar percent={quotaPct} fullWidth /> : null}
          <span className={styles.statMeta}>
            {quotaBytes > 0
              ? `${formatPercent(quotaPct)} of quota · ${formatExactBytes(usedBytes)} cached`
              : 'Unlimited (no quota set)'}
          </span>
        </div>
        <div className={styles.statCard}>
          <span className={styles.statLabel}>Models</span>
          <span className={`${styles.statValue} tabular`}>{items.length}</span>
          <span className={styles.statMeta}>Unique repos</span>
        </div>
        <div className={styles.statCard}>
          <span className={styles.statLabel}>Files</span>
          <span className={`${styles.statValue} tabular`}>{fileCount}</span>
          <span className={styles.statMeta}>Across all models</span>
        </div>
      </div>
      <div className={styles.chartCard}>
        <h2 className={styles.chartTitle}>Storage by model</h2>
        <div className={styles.chartBody}>
          <StorageDonut segments={segments} total={listing.grand_total_bytes} />
          <ul className={styles.chartLegend}>
            {segments.map((seg, i) => (
              <li key={seg.label} className={styles.legendItem}>
                <span
                  className={styles.legendSwatch}
                  style={{ background: CHART_COLORS[i % CHART_COLORS.length] }}
                  aria-hidden="true"
                />
                <span className={styles.legendLabel} title={seg.label}>
                  {seg.label}
                </span>
                <span className={`${styles.legendSize} tabular`}>{formatBytes(seg.bytes)}</span>
                <span className={`${styles.legendPct} tabular`}>{seg.pct.toFixed(1)}%</span>
              </li>
            ))}
          </ul>
        </div>
      </div>
    </Card>
  );
}

export default function ModelsPage() {
  const [listing, setListing] = useState<GroupedListing | null>(null);
  const [cacheStats, setCacheStats] = useState<CacheStats | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  const [search, setSearch] = useState('');
  const [minSize, setMinSize] = useState<MinSizeKey>('none');
  const [sort, setSort] = useState<SortKey>('size-desc');
  const [purgeTarget, setPurgeTarget] = useState<ModelGroup | null>(null);
  const [purging, setPurging] = useState(false);

  const load = useCallback(async () => {
    try {
      const [data, stats] = await Promise.all([api.modelsGrouped(500, true), api.cacheStats()]);
      setListing(data);
      setCacheStats(stats);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load models');
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const filtered = useMemo(() => {
    if (!listing) return [];
    const q = search.trim().toLowerCase();
    const minBytes = MIN_SIZE_BYTES[minSize];
    let items = (listing.items ?? []).filter((g) => {
      if (g.total_bytes < minBytes) return false;
      if (!q) return true;
      return modelLabel(g).toLowerCase().includes(q);
    });
    items = [...items].sort((a, b) => {
      switch (sort) {
        case 'size-asc':
          return a.total_bytes - b.total_bytes;
        case 'name-asc':
          return modelLabel(a).localeCompare(modelLabel(b));
        case 'files-desc':
          return b.file_count - a.file_count;
        default:
          return b.total_bytes - a.total_bytes;
      }
    });
    return items;
  }, [listing, search, minSize, sort]);

  async function confirmPurge() {
    if (!purgeTarget) return;
    setPurging(true);
    setError(null);
    setSuccess(null);
    try {
      const res = await api.purgeCacheModel(purgeTarget.org, purgeTarget.name);
      const parts = [`Purged ${res.purged} files, freed ${formatBytes(res.bytes_freed)}`];
      // Xet / LFS chunks can be shared across HF repos.  When this
      // model was one of several owners of a body, the server trims
      // it from the owner set instead of deleting the body, so the
      // remaining model keeps serving warm.  Surface the count so
      // operators understand why disk usage may not have dropped
      // by the model's full nominal size.
      if (res.trimmed > 0) {
        parts.push(
          `${res.trimmed} shared ${res.trimmed === 1 ? 'file' : 'files'} kept (still owned by other models)`,
        );
      }
      setSuccess(parts.join('. '));
      setPurgeTarget(null);
      await load();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Purge failed');
    } finally {
      setPurging(false);
    }
  }

  const grandTotal = listing?.grand_total_bytes ?? 0;

  return (
    <ConsoleShell>
      <div className="animate-in">
        <PageHeader
          title="Models"
          subtitle="Disk usage by model. Expand a row to see the cached files, or purge an entire model to free space."
          actions={
            <Link href="/imports">
              <Button variant="secondary">Queue import</Button>
            </Link>
          }
        />

        {error ? <Alert tone="critical">{error}</Alert> : null}
        {success ? <Alert tone="success">{success}</Alert> : null}

        {listing === null ? (
          <Skeleton height={280} />
        ) : (listing.items ?? []).length === 0 ? (
          <EmptyState
            title="No models cached yet"
            description="Queue a Hugging Face model through Imports and it will appear here once cached."
          />
        ) : (
          <>
            <StorageOverview listing={listing} stats={cacheStats} />

            <div className={styles.toolbar}>
              <label className={styles.toolbarField} htmlFor="models-search">
                <span className={styles.toolbarLabel}>Search</span>
                <Input
                  id="models-search"
                  name="search"
                  type="search"
                  placeholder="org or model name"
                  value={search}
                  onChange={(e) => setSearch(e.target.value)}
                />
              </label>
              <label className={styles.toolbarField} htmlFor="models-min-size">
                <span className={styles.toolbarLabel}>Min size</span>
                <select
                  id="models-min-size"
                  name="min_size"
                  className={styles.select}
                  value={minSize}
                  onChange={(e) => setMinSize(e.target.value as MinSizeKey)}
                >
                  <option value="none">Any</option>
                  <option value="10mb">&gt; 10 MB</option>
                  <option value="100mb">&gt; 100 MB</option>
                  <option value="1gb">&gt; 1 GB</option>
                </select>
              </label>
              <label className={styles.toolbarField} htmlFor="models-sort">
                <span className={styles.toolbarLabel}>Sort</span>
                <select
                  id="models-sort"
                  name="sort"
                  className={styles.select}
                  value={sort}
                  onChange={(e) => setSort(e.target.value as SortKey)}
                >
                  <option value="size-desc">Largest first</option>
                  <option value="size-asc">Smallest first</option>
                  <option value="name-asc">Name A–Z</option>
                  <option value="files-desc">Most files</option>
                </select>
              </label>
            </div>

            {filtered.length === 0 ? (
              <EmptyState
                title="No models match"
                description="Try clearing the search or lowering the minimum size."
              />
            ) : (
              <div className={styles.modelTable} aria-label="Cached models">
                <div className={styles.modelHeader} aria-hidden="true">
                  <span>Model</span>
                  <span>Revisions</span>
                  <span>Files</span>
                  <span>Size</span>
                  <span>Actions</span>
                </div>
                {filtered.map((group) => {
                  const quotaBytes = cacheStats?.quota_bytes ?? 0;
                  const pct =
                    quotaBytes > 0
                      ? (group.total_bytes / quotaBytes) * 100
                      : grandTotal > 0
                        ? (group.total_bytes / grandTotal) * 100
                        : 0;
                  return (
                    <Disclosure
                      key={`${group.upstream_host}/${group.org}/${group.name}`}
                      actions={
                        <Button
                          variant="danger"
                          size="sm"
                          onClick={() => setPurgeTarget(group)}
                        >
                          Purge
                        </Button>
                      }
                      summary={
                        <div className={styles.modelRowSummary}>
                          <div>
                            <div className={styles.modelName}>{group.name}</div>
                            <div className={styles.modelId}>{modelLabel(group)}</div>
                          </div>
                          <RevisionSummary revisions={group.revisions} />
                          <div className="tabular">{group.file_count}</div>
                          <div className={styles.sizeCell}>
                            <span className="tabular">{formatBytes(group.total_bytes)}</span>
                            <UsageBar percent={pct} />
                          </div>
                        </div>
                      }
                    >
                      <div className={styles.modelDetails}>
                        <DownloadCommands group={group} />
                        <RevisionDetails revisions={group.revisions} />
                        <DataTable>
                          <thead>
                            <tr>
                              <th scope="col">File</th>
                              <th scope="col">Size</th>
                            </tr>
                          </thead>
                          <tbody>
                            {(group.files ?? []).map((f) => (
                              <tr key={`${f.upstream_host}${f.path}`}>
                                <td style={{ fontFamily: 'var(--font-mono)', fontSize: 13 }}>{f.path}</td>
                                <td className="tabular">{formatBytes(f.total_bytes)}</td>
                              </tr>
                            ))}
                          </tbody>
                        </DataTable>
                      </div>
                    </Disclosure>
                  );
                })}
              </div>
            )}
          </>
        )}

        {purgeTarget ? (
          <Modal
            title="Purge cached model"
            onClose={() => {
              if (!purging) setPurgeTarget(null);
            }}
            footer={
              <>
                <Button variant="secondary" disabled={purging} onClick={() => setPurgeTarget(null)}>
                  Cancel
                </Button>
                <Button variant="danger" loading={purging} onClick={() => void confirmPurge()}>
                  Purge cached files
                </Button>
              </>
            }
          >
            <p>
              Remove all cached files for <strong>{modelLabel(purgeTarget)}</strong> (
              {formatBytes(purgeTarget.total_bytes)}, {purgeTarget.file_count} files)? Files will be
              re-downloaded automatically the next time someone requests them.
            </p>
          </Modal>
        ) : null}
      </div>
    </ConsoleShell>
  );
}
