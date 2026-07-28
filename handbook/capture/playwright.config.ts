import { defineConfig } from '@playwright/test';

export default defineConfig({
	testDir: './specs',
	outputDir: './test-results',
	timeout: 12 * 60 * 1000,
	workers: 1,
	retries: 0,
	reporter: [['list'], ['html', { outputFolder: './playwright-report', open: 'never' }]],
	use: {
		baseURL: process.env.REPOKARTA_CAPTURE_URL ?? 'http://127.0.0.1:7332',
		colorScheme: 'dark',
		locale: 'en-US',
		reducedMotion: 'reduce',
		timezoneId: 'Europe/Warsaw',
		trace: 'retain-on-failure',
	},
});
