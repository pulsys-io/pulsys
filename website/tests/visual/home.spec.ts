import { test, expect } from '@playwright/test';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const OUT = path.join(__dirname, '../../test-results/screenshots');

test.describe('home visual', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('./');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1200);
  });

  test('hero viewport', async ({ page }, testInfo) => {
    await expect(page.locator('.lp-hero')).toBeVisible();
    await page.locator('.lp-hero').screenshot({
      path: path.join(OUT, testInfo.project.name, 'hero.png'),
    });
  });

  test('architecture diagram', async ({ page }, testInfo) => {
    const arch = page.locator('.lp-arch-diag');
    await arch.scrollIntoViewIfNeeded();
    await page.waitForTimeout(300);
    await expect(arch).toBeVisible();
    await arch.screenshot({
      path: path.join(OUT, testInfo.project.name, 'architecture.png'),
    });
  });

  test('proof section', async ({ page }, testInfo) => {
    const proof = page.locator('.lp-proof');
    await proof.scrollIntoViewIfNeeded();
    await page.waitForTimeout(400);
    await proof.screenshot({
      path: path.join(OUT, testInfo.project.name, 'proof.png'),
    });
  });

  test('feature grid', async ({ page }, testInfo) => {
    const features = page.locator('.lp-features');
    await features.scrollIntoViewIfNeeded();
    await page.waitForTimeout(300);
    await expect(features.locator('.lp-feature')).toHaveCount(6);
    await features.screenshot({
      path: path.join(OUT, testInfo.project.name, 'features.png'),
    });
  });

  test('use cases and quick start', async ({ page }, testInfo) => {
    const usecases = page.locator('.lp-usecases');
    await usecases.scrollIntoViewIfNeeded();
    await expect(usecases.locator('.lp-usecase')).toHaveCount(3);
    const quickstart = page.locator('.lp-quickstart');
    await quickstart.scrollIntoViewIfNeeded();
    await page.waitForTimeout(300);
    await expect(quickstart.locator('.lp-quickstart__code')).toHaveCount(2);
    await quickstart.screenshot({
      path: path.join(OUT, testInfo.project.name, 'quickstart.png'),
    });
  });

  test('full page', async ({ page }, testInfo) => {
    await page.screenshot({
      path: path.join(OUT, testInfo.project.name, 'full-page.png'),
      fullPage: true,
    });
  });

  test('benchmark charts visible', async ({ page }) => {
    await page.locator('.lp-proof').scrollIntoViewIfNeeded();
    const chart = page.locator('.lp-chart img').first();
    await expect(chart).toBeVisible();
    const box = await chart.boundingBox();
    expect(box?.width).toBeGreaterThan(120);
  });

  test('docs benchmarks page', async ({ page }) => {
    await page.goto('docs/benchmarks/');
    await page.waitForLoadState('networkidle');
    await expect(page.locator('#benchmarks')).toContainText(/benchmark/i);
  });
});
