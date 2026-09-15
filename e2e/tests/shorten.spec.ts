import { test, expect } from '@playwright/test';

test.describe('LinkPulse 前端', () => {
  test('首頁載入，標題與輸入表單都在', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('h1')).toContainText('LinkPulse');
    await expect(page.locator('#url-input')).toBeVisible();
    await expect(page.locator('#submit-btn')).toBeVisible();
  });

  test('輸入合法網址後可以成功產生短網址', async ({ page }) => {
    const originalUrl = `https://example.com/playwright-${Date.now()}`;

    await page.goto('/');
    await page.locator('#url-input').fill(originalUrl);
    await page.locator('#submit-btn').click();

    const resultLink = page.locator('#result-link');
    await expect(page.locator('#result')).toHaveClass(/show/);
    await expect(resultLink).toBeVisible();

    const href = await resultLink.getAttribute('href');
    expect(href).toMatch(/^http:\/\/localhost:8080\/[A-Za-z0-9]{7}$/);

    // 輸入框應該被清空，準備下一次輸入
    await expect(page.locator('#url-input')).toHaveValue('');
  });

  test('不安全的網址（javascript: scheme）會顯示錯誤訊息', async ({ page }) => {
    await page.goto('/');
    await page.locator('#url-input').fill('javascript:alert(1)');
    await page.locator('#submit-btn').click();

    const errorMsg = page.locator('#error-msg');
    await expect(errorMsg).toBeVisible();
    await expect(errorMsg).toContainText('invalid or unsafe url');
    await expect(page.locator('#result')).not.toHaveClass(/show/);
  });

  test('建立成功後，短網址會出現在歷史紀錄清單', async ({ page }) => {
    const originalUrl = `https://example.com/history-${Date.now()}`;

    await page.goto('/');
    await page.locator('#url-input').fill(originalUrl);
    await page.locator('#submit-btn').click();

    await expect(page.locator('#result')).toHaveClass(/show/);

    const historyItems = page.locator('#history-list li');
    await expect(historyItems.first()).toBeVisible();
    await expect(historyItems.first().locator('.original')).toHaveText(originalUrl);
  });

  test('點擊複製按鈕會把短網址複製到剪貼簿', async ({ page, context, browserName }) => {
    test.skip(browserName !== 'chromium', 'clipboard permission API 只穩定支援 Chromium');
    await context.grantPermissions(['clipboard-read', 'clipboard-write']);

    const originalUrl = `https://example.com/copy-${Date.now()}`;

    await page.goto('/');
    await page.locator('#url-input').fill(originalUrl);
    await page.locator('#submit-btn').click();
    await expect(page.locator('#result')).toHaveClass(/show/);

    const shortUrl = await page.locator('#result-link').getAttribute('href');
    await page.locator('#copy-btn').click();

    await expect(page.locator('#copy-btn')).toHaveText('已複製');

    const clipboardText = await page.evaluate(() => navigator.clipboard.readText());
    expect(clipboardText).toBe(shortUrl);
  });

  test('產生的短網址真的能導向原始網址', async ({ page }) => {
    const originalUrl = `https://example.com/redirect-check-${Date.now()}`;

    await page.goto('/');
    await page.locator('#url-input').fill(originalUrl);
    await page.locator('#submit-btn').click();
    await expect(page.locator('#result')).toHaveClass(/show/);

    const shortUrl = await page.locator('#result-link').getAttribute('href');
    expect(shortUrl).not.toBeNull();

    const response = await page.request.get(shortUrl!, { maxRedirects: 0 });
    expect(response.status()).toBe(302);
    expect(response.headers()['location']).toBe(originalUrl);
  });
});
