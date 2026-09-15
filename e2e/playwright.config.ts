import { defineConfig, devices } from '@playwright/test';

const PORT = process.env.PORT ?? '8080';
const BASE_URL = process.env.BASE_URL ?? `http://localhost:${PORT}`;
const DATABASE_URL =
  process.env.DATABASE_URL ??
  'postgres://postgres:postgres@localhost:5432/linkpulse?sslmode=disable';

export default defineConfig({
  testDir: './tests',
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  reporter: [['html', { open: 'never' }], ['list']],
  use: {
    baseURL: BASE_URL,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  // 直接把 Go server 起在測試前面，測完自動關掉；本機開發如果已經手動
  // 起了一個 server，會直接重複使用，不會再啟動第二個。
  webServer: {
    command: 'go run main.go',
    cwd: '..',
    url: `${BASE_URL}/healthz`,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
    env: {
      PORT,
      BASE_URL,
      DATABASE_URL,
    },
  },
});
