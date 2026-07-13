import { expect, test } from '@playwright/test';

const LONG_ERROR =
  'GET http://localhost:8082/_p/cas-bridge.xethub.hf.co/reconstruction/abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890?Action=getReconstructionInfo&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAEXAMPLEKEYID%2F20260522%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260522T194512Z&X-Amz-Expires=3600&X-Amz-SignedHeaders=host&X-Amz-Signature=0011223344556677889900aabbccddeeff0011223344556677889900aabbccdd: dial tcp [::1]:8082: connect: connection refused';

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => {
    window.sessionStorage.setItem(
      'pulsys_user',
      JSON.stringify({
        user_id: 'u1',
        email: 'admin@test.local',
        role: 'admin',
      }),
    );
  });

  await page.route('**/admin/api/v1/tenant', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        id: 'tid1',
        name: 'default',
        display_name: 'Default',
        created_at: '2026-05-22T00:00:00Z',
      }),
    });
  });
  await page.route('**/auth/csrf', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ csrf_token: 'test-csrf' }),
    });
  });
});

test('imports page renders empty state when no jobs exist', async ({ page }) => {
  await page.route('**/admin/api/v1/imports?limit=50', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ items: [] }),
    });
  });

  await page.goto('/imports');
  await expect(page.getByRole('heading', { name: 'Imports', exact: true })).toBeVisible();
  // The import form is now embedded on this page.
  await expect(page.getByRole('heading', { name: 'Import from HF' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'No imports yet' })).toBeVisible();
});

// Verifies the inline import form on /imports queues a job and
// refetches the list so the new row shows up immediately rather
// than waiting for the 2.5s polling tick.  Covers the "queue +
// observe in one screen" UX promise that motivated moving the
// form off /models.
test('import form posts to /admin/api/v1/imports and refreshes the list', async ({ page }) => {
  let listCalls = 0;
  await page.route('**/admin/api/v1/imports?limit=50', async (route) => {
    listCalls += 1;
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        items:
          listCalls === 1
            ? []
            : [
                {
                  id: 'job-new',
                  type: 'hf_cache_import',
                  status: 'queued',
                  payload: { repo_id: 'Qwen/Qwen2.5-0.5B', revision: 'main' },
                  progress: {},
                  attempt: 0,
                  created_at: '2026-05-22T00:00:00Z',
                  updated_at: '2026-05-22T00:00:00Z',
                },
              ],
      }),
    });
  });

  let postedBody: { repo_id?: string; revision?: string } | null = null;
  await page.route('**/admin/api/v1/imports', async (route) => {
    expect(route.request().method()).toBe('POST');
    postedBody = JSON.parse(route.request().postData() ?? '{}');
    await route.fulfill({
      status: 201,
      contentType: 'application/json',
      body: JSON.stringify({
        id: 'job-new',
        type: 'hf_cache_import',
        status: 'queued',
        payload: { repo_id: 'Qwen/Qwen2.5-0.5B', revision: 'main' },
        progress: {},
        attempt: 0,
        created_at: '2026-05-22T00:00:00Z',
        updated_at: '2026-05-22T00:00:00Z',
      }),
    });
  });

  await page.goto('/imports');
  await expect(page.getByRole('heading', { name: 'No imports yet' })).toBeVisible();

  await page.getByLabel('Repo').fill('Qwen/Qwen2.5-0.5B');
  await page.getByRole('button', { name: 'Import', exact: true }).click();

  await expect.poll(() => postedBody?.repo_id).toBe('Qwen/Qwen2.5-0.5B');
  await expect.poll(() => postedBody?.revision).toBe('main');
  await expect(page.getByText('Queued import for Qwen/Qwen2.5-0.5B.')).toBeVisible();
  // The success handler triggers an immediate loadImports() so the
  // new queued row appears without waiting for the polling tick.
  await expect(page.locator('article').filter({ hasText: 'Qwen/Qwen2.5-0.5B' })).toBeVisible();
});

test('running imports show Cancel and no Delete', async ({ page }) => {
  await page.route('**/admin/api/v1/imports?limit=50', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        items: [
          {
            id: 'job-running',
            type: 'hf_cache_import',
            status: 'running',
            payload: { repo_id: 'Qwen/Qwen2.5-0.5B', revision: 'main' },
            progress: {
              phase: 'downloading',
              bytes_done: 524288000,
              bytes_total: 1048576000,
              download_bps: 10485760,
              current_file: 'model.safetensors',
            },
            attempt: 1,
            created_at: '2026-05-22T00:00:00Z',
            updated_at: '2026-05-22T00:01:00Z',
          },
        ],
      }),
    });
  });

  await page.goto('/imports');
  const row = page.locator('article').filter({ hasText: 'Qwen/Qwen2.5-0.5B' });
  await expect(row).toBeVisible();
  await expect(row.getByText('running')).toBeVisible();
  await expect(row.getByText('model.safetensors')).toBeVisible();
  await expect(row.getByRole('button', { name: 'Cancel', exact: true })).toBeVisible();
  // Force remove is offered as a tertiary affordance alongside Cancel
  // so the operator always has an escape hatch for stuck rows. Delete
  // is still hidden -- it requires the row to be in a non-running
  // state and force-cancel-then-delete is the documented flow.
  await expect(row.getByRole('button', { name: 'Force remove' })).toBeVisible();
  await expect(row.getByRole('button', { name: 'Delete' })).toHaveCount(0);
  // Determinate progress: bytes_total known -> percent reported via
  // aria-valuenow so screen readers and tests can verify the cue.
  const progressbar = row.getByRole('progressbar');
  await expect(progressbar).toBeVisible();
  await expect(progressbar).toHaveAttribute('aria-valuenow', '50');
});

// Regression for "the job is running but the UI looks frozen":
// before any byte totals come back from the worker we now render
// an indeterminate sliding bar + a pulsing status dot so the row
// reads as alive, not stuck.
test('running with no byte total renders indeterminate progress', async ({ page }) => {
  await page.route('**/admin/api/v1/imports?limit=50', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        items: [
          {
            id: 'job-warming',
            type: 'hf_cache_import',
            status: 'running',
            payload: { repo_id: 'Qwen/Qwen2.5-0.5B', revision: 'main' },
            progress: {},
            attempt: 1,
            created_at: '2026-05-22T00:00:00Z',
            updated_at: '2026-05-22T00:00:01Z',
          },
        ],
      }),
    });
  });

  await page.goto('/imports');
  const row = page.locator('article').filter({ hasText: 'Qwen/Qwen2.5-0.5B' });
  await expect(row).toBeVisible();
  await expect(row).toHaveAttribute('aria-busy', 'true');
  const progressbar = row.getByRole('progressbar');
  await expect(progressbar).toBeVisible();
  await expect(progressbar).toHaveAttribute('aria-valuetext', 'Working');
  await expect(progressbar).not.toHaveAttribute('aria-valuenow', /\d+/);
  // Phase placeholder so the row never reads as empty.
  await expect(row.getByText('Starting…')).toBeVisible();
  await expect(row.getByText('Preparing…')).toBeVisible();
});

test('retrying import shows both Cancel and Delete', async ({ page }) => {
  // Regression: River parks failing-but-not-yet-discarded jobs in
  // Retryable state.  The previous mapping treated this as
  // "running", which hid Delete -- users could see "Error details"
  // but had no way to remove the job from the queue.
  await page.route('**/admin/api/v1/imports?limit=50', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        items: [
          {
            id: 'job-retrying',
            type: 'hf_cache_import',
            status: 'retrying',
            payload: { repo_id: 'Qwen/Qwen2.5-0.5B', revision: 'main' },
            progress: { phase: 'downloading', bytes_done: 1700000, bytes_total: 999000000 },
            error: 'Network error',
            error_hint: 'The proxy could not reach Hugging Face.',
            error_detail:
              'GET http://localhost:8082/_p/cas-bridge/...: dial tcp 127.0.0.1:8082: connect: connection refused',
            attempt: 4,
            created_at: '2026-05-22T00:00:00Z',
            updated_at: '2026-05-22T00:05:00Z',
          },
        ],
      }),
    });
  });

  await page.goto('/imports');
  const row = page.locator('article').filter({ hasText: 'Qwen/Qwen2.5-0.5B' });
  await expect(row).toBeVisible();
  await expect(row.getByText('retrying')).toBeVisible();
  await expect(row.getByText('attempt 4')).toBeVisible();
  await expect(row.getByRole('button', { name: 'Cancel' })).toBeVisible();
  await expect(row.getByRole('button', { name: 'Delete' })).toBeVisible();
  // Apple-style alert: title in primary text, hint in muted text.
  await expect(row.getByText('Network error', { exact: true })).toBeVisible();
  await expect(row.getByText('The proxy could not reach Hugging Face.')).toBeVisible();
  await expect(row.locator('summary')).toBeVisible();
});

test('failed import shows Delete with collapsible long error', async ({ page }) => {
  await page.route('**/admin/api/v1/imports?limit=50', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        items: [
          {
            id: 'job-failed',
            type: 'hf_cache_import',
            status: 'failed',
            payload: { repo_id: 'Qwen/Qwen2.5-0.5B', revision: 'main' },
            progress: { phase: 'downloading' },
            error: 'Import failed',
            error_hint: 'Open technical details below for the underlying error.',
            error_detail: LONG_ERROR,
            attempt: 1,
            created_at: '2026-05-22T00:00:00Z',
            updated_at: '2026-05-22T00:01:00Z',
          },
        ],
      }),
    });
  });

  await page.goto('/imports');
  const row = page.locator('article').filter({ hasText: 'Qwen/Qwen2.5-0.5B' });
  await expect(row).toBeVisible();
  await expect(row.getByRole('button', { name: 'Delete' })).toBeVisible();
  await expect(row.getByRole('button', { name: 'Cancel' })).toHaveCount(0);

  // Error is collapsed by default and lives inside a <details> -- the raw
  // text must NOT be visible until the user expands the disclosure.
  await expect(row.getByRole('alert').getByText('Import failed')).toBeVisible();
  await expect(
    row.getByRole('alert').getByText('Open technical details below for the underlying error.'),
  ).toBeVisible();
  const summary = row.locator('summary');
  await expect(summary).toBeVisible();
  await expect(row.getByText(LONG_ERROR)).not.toBeVisible();

  await summary.click();
  await expect(row.getByText(LONG_ERROR)).toBeVisible();

  // The error block must be width-constrained: its rendered width should
  // not exceed the row width (catches the original "long URL blows out
  // the layout" regression).
  const errorBox = row.locator('pre');
  const rowBox = await row.boundingBox();
  const errBox = await errorBox.boundingBox();
  expect(rowBox).not.toBeNull();
  expect(errBox).not.toBeNull();
  if (rowBox && errBox) {
    expect(errBox.width).toBeLessThanOrEqual(rowBox.width + 1);
  }
});

// Regression for "I clicked Cancel and got 204, but the job is still
// running and I can't delete it." This is the orphaned-running-row
// repro: a previous proxy process left the row in `running` state
// with cancel_requested_at populated (River signalled the dead worker
// but state never flipped). The UI must:
//   1. Show a `Cancelling…` badge so the user has feedback that the
//      cancel signal has been sent.
//   2. Suppress the regular Cancel button -- clicking it again just
//      re-notifies River.
//   3. Surface a `Force remove` escape hatch that calls the new
//      /force-cancel endpoint.
test('orphaned running row offers Force remove and POSTs to force-cancel', async ({ page }) => {
  let listCalls = 0;
  await page.route('**/admin/api/v1/imports?limit=50', async (route) => {
    listCalls += 1;
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        items:
          listCalls === 1
            ? [
                {
                  id: 'job-orphan',
                  type: 'hf_cache_import',
                  status: 'running',
                  payload: { repo_id: 'Qwen/Qwen2.5-0.5B', revision: 'main' },
                  progress: { phase: 'downloading', bytes_done: 0, bytes_total: 0 },
                  cancel_requested_at: '2026-05-22T00:00:30Z',
                  attempt: 1,
                  created_at: '2026-05-22T00:00:00Z',
                  updated_at: '2026-05-22T00:00:30Z',
                },
              ]
            : [
                {
                  id: 'job-orphan',
                  type: 'hf_cache_import',
                  status: 'canceled',
                  payload: { repo_id: 'Qwen/Qwen2.5-0.5B', revision: 'main' },
                  progress: { phase: 'downloading' },
                  attempt: 1,
                  created_at: '2026-05-22T00:00:00Z',
                  completed_at: '2026-05-22T00:01:00Z',
                  updated_at: '2026-05-22T00:01:00Z',
                },
              ],
      }),
    });
  });

  let forceCancelCalled = false;
  await page.route('**/admin/api/v1/imports/job-orphan/force-cancel', async (route) => {
    expect(route.request().method()).toBe('POST');
    forceCancelCalled = true;
    await route.fulfill({ status: 204, body: '' });
  });

  await page.goto('/imports');
  const row = page.locator('article').filter({ hasText: 'Qwen/Qwen2.5-0.5B' });
  await expect(row).toBeVisible();

  // Badge swaps from `running` to `Cancelling…` while cancel is
  // outstanding so the row visibly reflects the pending signal.
  await expect(row.getByText('Cancelling…')).toBeVisible();
  // Cancel button is suppressed once cancel_requested_at is set;
  // repeated Cancel clicks would just re-notify River.
  await expect(row.getByRole('button', { name: 'Cancel', exact: true })).toHaveCount(0);

  await row.getByRole('button', { name: 'Force remove' }).click();
  // The destructive escape hatch is gated by a confirmation modal so
  // an accidental click cannot wipe queue state. Apple HIG: confirm
  // any irreversible action that bypasses the queue's safety.
  await expect(page.getByRole('heading', { name: 'Force remove import' })).toBeVisible();
  await page
    .getByRole('button', { name: 'Force remove' })
    .last()
    .click();

  await expect.poll(() => forceCancelCalled).toBe(true);
  await expect(row.getByText('canceled', { exact: true })).toBeVisible();
});

// Force remove is also available on a row that has never had Cancel
// clicked -- this is the second path to the same escape hatch and
// covers the case where the user opens the admin UI fresh and sees
// a stuck row left behind by a previous proxy run.
test('Force remove is available on a fresh running row (no cancel yet)', async ({ page }) => {
  await page.route('**/admin/api/v1/imports?limit=50', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        items: [
          {
            id: 'job-fresh',
            type: 'hf_cache_import',
            status: 'running',
            payload: { repo_id: 'Qwen/Qwen2.5-0.5B', revision: 'main' },
            progress: { phase: 'downloading', bytes_done: 0, bytes_total: 0 },
            attempt: 1,
            created_at: '2026-05-22T00:00:00Z',
            updated_at: '2026-05-22T00:00:01Z',
          },
        ],
      }),
    });
  });

  await page.goto('/imports');
  const row = page.locator('article').filter({ hasText: 'Qwen/Qwen2.5-0.5B' });
  await expect(row).toBeVisible();
  // Both affordances visible: primary Cancel + tertiary Force remove.
  await expect(row.getByRole('button', { name: 'Cancel', exact: true })).toBeVisible();
  await expect(row.getByRole('button', { name: 'Force remove' })).toBeVisible();
});

// Canceled is a user action, not a failure: the row shows the badge
// alone, never a red error block. (Matches GitHub Actions "Canceled"
// styling and Apple HIG -- not every terminal state is critical.)
test('canceled import shows no error block', async ({ page }) => {
  await page.route('**/admin/api/v1/imports?limit=50', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        items: [
          {
            id: 'job-canceled',
            type: 'hf_cache_import',
            status: 'canceled',
            payload: { repo_id: 'Qwen/Qwen2.5-0.5B', revision: 'main' },
            progress: { phase: 'downloading' },
            // Even when the backend has a JobCancelError on file, the
            // store strips it from the user-facing surface so the UI
            // never renders it as an error. Asserting that empty error
            // mirrors what the store sends in production.
            attempt: 1,
            created_at: '2026-05-22T00:00:00Z',
            updated_at: '2026-05-22T00:01:00Z',
          },
        ],
      }),
    });
  });

  await page.goto('/imports');
  const row = page.locator('article').filter({ hasText: 'Qwen/Qwen2.5-0.5B' });
  await expect(row).toBeVisible();
  await expect(row.getByText('canceled', { exact: true })).toBeVisible();
  await expect(row.getByRole('alert')).toHaveCount(0);
  await expect(row.getByRole('button', { name: 'Delete' })).toBeVisible();
});

test('delete button calls DELETE /admin/api/v1/imports/{id}', async ({ page }) => {
  let listCalls = 0;
  await page.route('**/admin/api/v1/imports?limit=50', async (route) => {
    listCalls += 1;
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        items:
          listCalls === 1
            ? [
                {
                  id: 'job-failed',
                  type: 'hf_cache_import',
                  status: 'failed',
                  payload: { repo_id: 'Qwen/Qwen2.5-0.5B', revision: 'main' },
                  progress: { phase: 'downloading' },
                  error: 'boom',
                  attempt: 1,
                  created_at: '2026-05-22T00:00:00Z',
                  updated_at: '2026-05-22T00:01:00Z',
                },
              ]
            : [],
      }),
    });
  });

  let deleteCalled = false;
  await page.route('**/admin/api/v1/imports/job-failed', async (route) => {
    expect(route.request().method()).toBe('DELETE');
    deleteCalled = true;
    await route.fulfill({ status: 204, body: '' });
  });

  await page.goto('/imports');
  await page.getByRole('button', { name: 'Delete' }).click();
  await page.getByRole('button', { name: 'Remove from history' }).click();

  await expect.poll(() => deleteCalled).toBe(true);
  await expect(page.getByRole('heading', { name: 'No imports yet' })).toBeVisible();
});
