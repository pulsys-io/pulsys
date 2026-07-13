import { defineConfig, devices } from '@playwright/test';

const PORT = 4321;
const BASE_PATH = process.env.PUBLIC_BASE_PATH ?? '/pulsys';
const BASE = `http://127.0.0.1:${PORT}${BASE_PATH === '/' ? '' : BASE_PATH.replace(/\/?$/, '/')}`;

export default defineConfig({
  testDir: './tests/visual',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  reporter: 'list',
  use: {
    baseURL: BASE,
    trace: 'off',
    screenshot: 'off',
    colorScheme: 'dark',
  },
  projects: [
    {
      name: 'desktop',
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1280, height: 900 },
      },
    },
    {
      name: 'mobile',
      use: {
        ...devices['Pixel 7'],
        viewport: { width: 390, height: 844 },
      },
    },
  ],
  webServer: {
    command: 'npm run preview -- --host 127.0.0.1 --port 4321',
    url: BASE,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
  },
});
