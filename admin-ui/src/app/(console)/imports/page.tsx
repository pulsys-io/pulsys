'use client';

import { useCallback, useEffect, useState } from 'react';
import { ConsoleShell } from '@/components/shell';
import {
  Alert,
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  Modal,
  PageHeader,
  Skeleton,
  UsageBar,
} from '@/components/ui';
import { api, type ImportJob } from '@/lib/api';
import styles from './imports.module.css';

function formatBytes(n?: number): string {
  if (n == null) return '—';
  if (n < 1024) return `${n} B`;
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(1)} KiB`;
  if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(1)} MiB`;
  return `${(n / 1024 ** 3).toFixed(2)} GiB`;
}

function formatSpeed(n?: number): string {
  if (!n || n <= 0) return '—';
  return `${formatBytes(n)}/s`;
}

function importRepo(job: ImportJob): string {
  return job.payload?.repo_id ?? 'Unknown repo';
}

function importRevision(job: ImportJob): string {
  return job.payload?.revision ?? 'main';
}

function statusTone(status: ImportJob['status']): 'neutral' | 'success' | 'warning' | 'critical' | 'accent' {
  switch (status) {
    case 'succeeded':
      return 'success';
    case 'failed':
      return 'critical';
    case 'canceled':
      return 'neutral';
    case 'running':
      return 'accent';
    case 'retrying':
      return 'warning';
    default:
      return 'warning';
  }
}

// Visible motion cue inside the status badge for jobs that are
// actually doing work in the background.  Without this a `running`
// row with no progress yet (worker just claimed the job; first
// HEAD/GET to HF not back yet) looks indistinguishable from a hung
// or stuck job.  Queued is intentionally excluded: a queued row is
// genuinely idle and the badge dot would lie.
function isAnimatedStatus(status: ImportJob['status']): boolean {
  return status === 'running' || status === 'retrying';
}

// Phase placeholder when the worker has not reported one yet.
// `running` lands here for the first second or two after the row
// is claimed; `retrying` lands here while River waits out its
// backoff between worker attempts.  Both used to display an empty
// row which read as "this is broken".
function displayPhase(job: ImportJob): string {
  if (job.progress?.phase) return job.progress.phase;
  if (job.status === 'running') return 'Starting…';
  if (job.status === 'retrying') return 'Waiting to retry…';
  if (job.status === 'queued') return 'Queued';
  return job.status;
}

// True after Cancel has been requested but River hasn't yet flipped
// the row to a terminal state. The row's badge becomes "Cancelling…"
// and the Cancel button is suppressed -- repeated Cancel clicks would
// just re-NOTIFY River without changing anything.
function isCancelPending(job: ImportJob): boolean {
  if (!job.cancel_requested_at) return false;
  return job.status === 'running' || job.status === 'retrying' || job.status === 'queued';
}

// True for rows where the operator should be offered the "Force
// remove" escape hatch. We surface it on every non-terminal row,
// not just those with a pending cancel, because the most common
// reason to need force-remove is a row orphaned by a previous proxy
// restart -- in that case cancel_attempted_at may not be set, and
// the user has no signal that the worker is dead.
function canForceCancel(status: ImportJob['status']): boolean {
  return status === 'queued' || status === 'running' || status === 'retrying';
}

// Active = still polling.  Retrying jobs are between worker
// attempts and may transition back to running, so we keep
// fetching to reflect attempt counter + progress changes.
function isActiveImport(job: ImportJob): boolean {
  return job.status === 'queued' || job.status === 'running' || job.status === 'retrying';
}

// Cancel applies to any non-terminal state.  River.JobCancel
// is a no-op on already-terminal rows, so a click-vs-poll race
// degrades to a successful 204 with no audit noise.
function canCancel(status: ImportJob['status']): boolean {
  return status === 'queued' || status === 'running' || status === 'retrying';
}

// Delete is offered for anything not currently locked by the
// worker.  Retrying counts as deletable because the row is
// sitting between attempts; the store re-checks the actual
// River state on the server side, so a race that moves the
// row back into Running yields a clean 409 we surface as the
// "Cancel first" error.
function canDelete(status: ImportJob['status']): boolean {
  return status === 'succeeded' || status === 'failed' || status === 'canceled' || status === 'retrying';
}

export default function ImportsPage() {
  const [imports, setImports] = useState<ImportJob[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  const [busyID, setBusyID] = useState<string | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<ImportJob | null>(null);
  // Separate modal state from delete so the force-remove confirmation
  // can show its own warning copy. Force-remove is destructive in a
  // different way than delete (it short-circuits the queue's safety
  // model), so the user needs to know what they're doing.
  const [forceTarget, setForceTarget] = useState<ImportJob | null>(null);
  // Prefixed `form*` rather than `import*` to avoid shadowing the
  // top-of-file importRepo/importRevision helpers that read fields
  // off an ImportJob row.  TypeScript caught the collision but it
  // would also be confusing at read time.
  const [formRepoID, setFormRepoID] = useState('');
  const [formRevision, setFormRevision] = useState('main');
  const [creatingImport, setCreatingImport] = useState(false);

  const loadImports = useCallback(async () => {
    try {
      const data = await api.imports(50);
      setImports(data.items ?? []);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load imports');
    }
  }, []);

  useEffect(() => {
    void loadImports();
  }, [loadImports]);

  useEffect(() => {
    if (!imports || !imports.some(isActiveImport)) return;
    const id = window.setInterval(() => {
      void loadImports();
    }, 2500);
    return () => window.clearInterval(id);
  }, [imports, loadImports]);

  async function onCancel(job: ImportJob) {
    setBusyID(job.id);
    setError(null);
    setSuccess(null);
    try {
      await api.cancelImport(job.id);
      setSuccess(`Cancelled import ${importRepo(job)}`);
      await loadImports();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Cancel failed');
    } finally {
      setBusyID(null);
    }
  }

  async function submitImport() {
    const repoID = formRepoID.trim();
    const revision = formRevision.trim();
    if (!repoID) {
      setError('Repo is required');
      return;
    }
    setCreatingImport(true);
    setError(null);
    setSuccess(null);
    try {
      await api.createImport({ repo_id: repoID, revision: revision || undefined });
      setSuccess(`Queued import for ${repoID}.`);
      setFormRepoID('');
      setFormRevision('main');
      // Refresh now so the just-queued row appears immediately
      // instead of waiting for the next 2.5s active-jobs poll tick.
      // Also re-arms the polling effect, which only spins up when
      // there is at least one active job in state.
      await loadImports();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Import failed');
    } finally {
      setCreatingImport(false);
    }
  }

  async function confirmDelete() {
    if (!deleteTarget) return;
    setBusyID(deleteTarget.id);
    setError(null);
    setSuccess(null);
    try {
      await api.deleteImport(deleteTarget.id);
      setSuccess(`Removed import ${importRepo(deleteTarget)}`);
      setDeleteTarget(null);
      await loadImports();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Delete failed');
    } finally {
      setBusyID(null);
    }
  }

  async function confirmForceCancel() {
    if (!forceTarget) return;
    setBusyID(forceTarget.id);
    setError(null);
    setSuccess(null);
    try {
      await api.forceCancelImport(forceTarget.id);
      setSuccess(`Force-canceled import ${importRepo(forceTarget)}`);
      setForceTarget(null);
      await loadImports();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Force-cancel failed');
    } finally {
      setBusyID(null);
    }
  }

  const safeImports = imports ?? [];
  const activeCount = safeImports.filter(isActiveImport).length;

  return (
    <ConsoleShell>
      <div className="animate-in">
        <PageHeader
          title="Imports"
          subtitle="Background cache-warming jobs. Cancel an active import or remove a finished one from the history."
          actions={
            <Button variant="secondary" onClick={() => void loadImports()}>
              Refresh
            </Button>
          }
        />

        {error ? <Alert tone="critical">{error}</Alert> : null}
        {success ? <Alert tone="success">{success}</Alert> : null}

        <Card className={styles.importCard}>
          <div className={styles.importHeader}>
            <div>
              <h2 className={styles.importTitle}>Import from HF</h2>
              <p className={styles.importSubtitle}>
                Queue a Hugging Face model to pre-warm the proxy cache. The server uses its
                read-only HF token; no user token is needed here.
              </p>
            </div>
          </div>
          <form
            className={styles.importForm}
            onSubmit={(e) => {
              e.preventDefault();
              void submitImport();
            }}
          >
            <label className={styles.formField} htmlFor="import-repo-id">
              <span className={styles.formLabel}>Repo</span>
              <Input
                id="import-repo-id"
                name="repo_id"
                value={formRepoID}
                placeholder="Qwen/Qwen2.5-0.5B"
                onChange={(e) => setFormRepoID(e.target.value)}
                autoComplete="off"
              />
            </label>
            <label className={styles.formField} htmlFor="import-revision">
              <span className={styles.formLabel}>Revision</span>
              <Input
                id="import-revision"
                name="revision"
                value={formRevision}
                placeholder="main"
                onChange={(e) => setFormRevision(e.target.value)}
                autoComplete="off"
              />
            </label>
            <Button type="submit" loading={creatingImport}>
              Import
            </Button>
          </form>
        </Card>

        <div className={styles.toolbar}>
          <span className={styles.toolbarMeta}>
            {imports === null
              ? 'Loading…'
              : `${safeImports.length} total · ${activeCount} active`}
          </span>
        </div>

        {imports === null ? (
          <Skeleton height={160} />
        ) : safeImports.length === 0 ? (
          <EmptyState
            title="No imports yet"
            description="Use the form above to queue a Hugging Face model and pre-warm the proxy cache."
          />
        ) : (
          <Card>
            <div className={styles.list}>
              {safeImports.map((job) => {
                const progress = job.progress ?? {};
                const hasTotal = (progress.bytes_total ?? 0) > 0;
                const pct = hasTotal
                  ? (Number(progress.bytes_done ?? 0) / (progress.bytes_total as number)) * 100
                  : 0;
                const cancelPending = isCancelPending(job);
                // Suppress regular Cancel once a cancel has already been
                // dispatched -- clicking it again would just re-notify
                // River without changing state.
                const showCancel = canCancel(job.status) && !cancelPending;
                const showForce = canForceCancel(job.status);
                const showDelete = canDelete(job.status);
                const rowBusy = busyID === job.id;
                const animated = isAnimatedStatus(job.status);
                // Show some kind of progress block whenever the job is
                // actively doing work, so a `running` row without byte
                // numbers yet still has motion on screen.
                const showProgress = hasTotal || animated;
                // Indeterminate variant: worker is busy but has not
                // posted a byte total yet (cold start, retry warmup).
                // Determinate variant: real download with totals.
                const indeterminate = animated && !hasTotal;
                return (
                  <article key={job.id} className={styles.row} aria-busy={rowBusy || animated || undefined}>
                    <div className={styles.head}>
                      <div className={styles.headTitle}>
                        <span className={styles.repo}>{importRepo(job)}</span>
                        <span className={styles.meta}>
                          <span className={styles.metaItem}>
                            revision <code>{importRevision(job)}</code>
                          </span>
                          {job.attempt > 0 ? (
                            <span className={styles.metaItem}>attempt {job.attempt}</span>
                          ) : null}
                        </span>
                      </div>
                      <div className={styles.actions}>
                        <Badge tone={cancelPending ? 'warning' : statusTone(job.status)}>
                          {animated || cancelPending ? (
                            <span className={styles.statusDot} aria-hidden />
                          ) : null}
                          {cancelPending ? 'Cancelling…' : job.status}
                        </Badge>
                        {showCancel ? (
                          <Button
                            variant="secondary"
                            size="sm"
                            loading={rowBusy}
                            onClick={() => void onCancel(job)}
                          >
                            Cancel
                          </Button>
                        ) : null}
                        {showForce ? (
                          <Button
                            variant="ghost"
                            size="sm"
                            disabled={rowBusy}
                            onClick={() => setForceTarget(job)}
                            className={styles.forceButton}
                          >
                            Force remove
                          </Button>
                        ) : null}
                        {showDelete ? (
                          <Button
                            variant="danger"
                            size="sm"
                            disabled={rowBusy}
                            onClick={() => setDeleteTarget(job)}
                          >
                            Delete
                          </Button>
                        ) : null}
                      </div>
                    </div>

                    {showProgress ? (
                      <div
                        className={styles.progress}
                        role="progressbar"
                        aria-valuenow={hasTotal ? Math.round(pct) : undefined}
                        aria-valuemin={0}
                        aria-valuemax={hasTotal ? 100 : undefined}
                        aria-valuetext={indeterminate ? 'Working' : `${Math.round(pct)}%`}
                      >
                        {indeterminate ? (
                          <span className={styles.progressIndeterminate} aria-hidden />
                        ) : (
                          <UsageBar percent={pct} fullWidth />
                        )}
                        <span className="tabular">
                          {hasTotal
                            ? `${formatBytes(progress.bytes_done)} / ${formatBytes(progress.bytes_total)}`
                            : 'Preparing…'}
                        </span>
                      </div>
                    ) : null}

                    <div className={styles.meta}>
                      <span className={styles.metaItem}>{displayPhase(job)}</span>
                      <span className={styles.metaItem}>{formatSpeed(progress.download_bps)}</span>
                      {progress.current_file ? (
                        <span className={`${styles.metaItem} ${styles.currentFile}`} title={progress.current_file}>
                          {progress.current_file}
                        </span>
                      ) : null}
                      {!job.error && progress.message ? (
                        <span className={styles.metaItem}>{progress.message}</span>
                      ) : null}
                    </div>

                    {job.error ? (
                      <div className={styles.errorBlock} role="alert">
                        <span className={styles.errorIcon} aria-hidden>
                          <svg width="14" height="14" viewBox="0 0 16 16" fill="none">
                            <circle cx="8" cy="8" r="7" fill="currentColor" opacity="0.12" />
                            <path
                              d="M8 4.5v4M8 11h.007"
                              stroke="currentColor"
                              strokeWidth="1.5"
                              strokeLinecap="round"
                            />
                          </svg>
                        </span>
                        <div className={styles.errorBody}>
                          <p className={styles.errorTitle}>{job.error}</p>
                          {job.error_hint ? (
                            <p className={styles.errorHint}>{job.error_hint}</p>
                          ) : null}
                          {job.error_detail ? (
                            <details className={styles.errorDetails}>
                              <summary className={styles.errorSummary}>Technical details</summary>
                              <pre className={styles.errorRaw}>{job.error_detail}</pre>
                            </details>
                          ) : null}
                        </div>
                      </div>
                    ) : null}
                  </article>
                );
              })}
            </div>
          </Card>
        )}

        {deleteTarget ? (
          <Modal
            title="Remove import"
            onClose={() => {
              if (busyID !== deleteTarget.id) setDeleteTarget(null);
            }}
            footer={
              <>
                <Button
                  variant="secondary"
                  disabled={busyID === deleteTarget.id}
                  onClick={() => setDeleteTarget(null)}
                >
                  Cancel
                </Button>
                <Button
                  variant="danger"
                  loading={busyID === deleteTarget.id}
                  onClick={() => void confirmDelete()}
                >
                  Remove from history
                </Button>
              </>
            }
          >
            <p>
              Remove the <strong>{deleteTarget.status}</strong> import for{' '}
              <strong>{importRepo(deleteTarget)}</strong> from this list? Cached files are not
              affected — this only deletes the job row.
            </p>
          </Modal>
        ) : null}

        {forceTarget ? (
          <Modal
            title="Force remove import"
            onClose={() => {
              if (busyID !== forceTarget.id) setForceTarget(null);
            }}
            footer={
              <>
                <Button
                  variant="secondary"
                  disabled={busyID === forceTarget.id}
                  onClick={() => setForceTarget(null)}
                >
                  Cancel
                </Button>
                <Button
                  variant="danger"
                  loading={busyID === forceTarget.id}
                  onClick={() => void confirmForceCancel()}
                >
                  Force remove
                </Button>
              </>
            }
          >
            <p>
              Force-cancel the <strong>{forceTarget.status}</strong> import for{' '}
              <strong>{importRepo(forceTarget)}</strong>?
            </p>
            <p className={styles.modalCaption}>
              Use this when regular Cancel doesn't take effect — for example when a job is stuck
              after a proxy restart. It marks the row as canceled immediately and bypasses the
              queue's safety checks. Any worker still touching the row will be a harmless no-op.
              Cached files are not affected.
            </p>
          </Modal>
        ) : null}
      </div>
    </ConsoleShell>
  );
}
