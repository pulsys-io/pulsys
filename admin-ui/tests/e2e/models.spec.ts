import { expect, test } from '@playwright/test';

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

test('models page renders cache listing and links to imports', async ({ page }) => {
  const consoleErrors: string[] = [];
  page.on('pageerror', (error) => consoleErrors.push(error.message));
  page.on('console', (message) => {
    if (message.type() === 'error') consoleErrors.push(message.text());
  });

  await page.route('**/admin/api/v1/models/grouped?**', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        items: [
          {
            org: 'Qwen',
            name: 'Qwen2.5-0.5B',
            upstream_host: 'huggingface.co',
            revisions: null,
            file_count: 2,
            total_bytes: 988097824,
            files: null,
          },
        ],
        grand_total_bytes: 988097824,
      }),
    });
  });
  await page.route('**/admin/api/v1/cache/stats', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        used_bytes: 988097824,
        quota_bytes: 10737418240,
        free_disk_bytes: 50000000000,
        entry_count: 2,
      }),
    });
  });

  await page.goto('/models');
  await expect(page.getByRole('heading', { name: 'Models', exact: true })).toBeVisible();
  await expect(page.getByText('Cache usage')).toBeVisible();
  await expect(page.getByText('942.3 MiB / 10.00 GiB')).toBeVisible();
  await expect(page.getByText(/9% of quota/)).toBeVisible();
  // The import form moved to /imports; the models page surfaces a
  // PageHeader action that links there instead of duplicating it.
  await expect(page.getByRole('heading', { name: 'Import from HF' })).toHaveCount(0);
  const queueImportLink = page.getByRole('link', { name: 'Queue import' });
  await expect(queueImportLink).toBeVisible();
  await expect(queueImportLink).toHaveAttribute('href', '/imports');
  await expect(page.locator('summary').filter({ hasText: 'Qwen2.5-0.5B' })).toBeVisible();
  await expect(page.getByText('0 revisions')).toBeVisible();

  await page.locator('summary').filter({ hasText: 'Qwen2.5-0.5B' }).click();
  const download = page.getByTestId('model-download-commands');
  await expect(download).toBeVisible();
  const command = page.getByTestId('model-download-command');
  await expect(command).toContainText('export HF_ENDPOINT="http://127.0.0.1:8080"');
  await expect(command).toContainText('export HF_TOKEN="YOUR_PULSYS_TOKEN"');
  await expect(command).toContainText('hf download "Qwen/Qwen2.5-0.5B"');
  await expect(command).toContainText('--local-dir "./Qwen2.5-0.5B"');
  await expect(page.getByRole('button', { name: 'Copy' })).toBeVisible();

  await page.screenshot({
    path: 'test-artifacts/models-list-only.png',
    fullPage: true,
  });
  expect(consoleErrors).toEqual([]);
});
