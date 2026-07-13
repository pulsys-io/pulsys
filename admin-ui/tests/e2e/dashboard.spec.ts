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

test('dashboard renders cache usage from stats endpoint', async ({ page }) => {
  await page.route('**/admin/api/v1/audit?limit=8', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ items: [] }),
    });
  });
  await page.route('**/admin/api/v1/models?limit=500', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        items: [
          { path: '/Org/Model/resolve/main/a.bin', upstream_host: 'huggingface.co', status_code: 200, total_bytes: 1 },
          { path: '/Org/Model/resolve/main/b.bin', upstream_host: 'huggingface.co', status_code: 200, total_bytes: 1 },
        ],
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

  await page.goto('/');
  await expect(page.getByText('Cache usage')).toBeVisible();
  await expect(page.getByText('942.3 MiB / 10.00 GiB')).toBeVisible();
  await expect(page.getByText('2 models · 2 cached objects')).toBeVisible();
});
