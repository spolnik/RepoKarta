import {
	expect,
	test,
	type APIRequestContext,
	type Browser,
	type BrowserContext,
	type Page,
} from '@playwright/test';
import { spawn } from 'node:child_process';
import { mkdir, rm } from 'node:fs/promises';
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const require = createRequire(import.meta.url);
const ffmpegPath = (require('@ffmpeg-installer/ffmpeg') as { path: string }).path;
const captureFPS = 10;

const captureDirectory = path.resolve(
	path.dirname(fileURLToPath(import.meta.url)),
	'../../src/assets/captures',
);
const videoDirectory = path.resolve(
	path.dirname(fileURLToPath(import.meta.url)),
	'../../public/media/captures',
);
const captureStyle = path.resolve(
	path.dirname(fileURLToPath(import.meta.url)),
	'../capture.css',
);

type Repository = {
	id: number;
	name: string;
	index_state: string;
	indexed_commit?: string;
};

async function waitForRepository(
	request: APIRequestContext,
	name: string,
): Promise<Repository> {
	let selected: Repository | undefined;
	await expect
		.poll(
			async () => {
				const response = await request.get('/api/repositories');
				if (!response.ok()) return `HTTP ${response.status()}`;
				const body = (await response.json()) as { repositories: Repository[] };
				selected = body.repositories.find((repository) => repository.name === name);
				return selected?.index_state ?? 'missing';
			},
			{
				message: `waiting for ${name} to become searchable`,
				timeout: 10 * 60 * 1000,
				intervals: [500, 1000, 2000, 5000],
			},
		)
		.toBe('ready');
	return selected!;
}

async function capturePage(
	browser: Browser,
	name: string,
	run: (page: Page) => Promise<void>,
	preparePoster?: (page: Page) => Promise<void>,
): Promise<void> {
	await mkdir(captureDirectory, { recursive: true });
	await mkdir(videoDirectory, { recursive: true });
	const frameDirectory = path.resolve(
		path.dirname(fileURLToPath(import.meta.url)),
		`../test-results/frames/${name}`,
	);
	await rm(frameDirectory, { recursive: true, force: true });
	await mkdir(frameDirectory, { recursive: true });
	const context = await browser.newContext({
		viewport: { width: 1600, height: 900 },
		deviceScaleFactor: 1.2,
		colorScheme: 'dark',
		reducedMotion: 'reduce',
	});
	const page = await context.newPage();
	let stopFrames = false;
	let frameNumber = 0;
	const recordingStartedAt = Date.now();
	const frameTask = (async () => {
		await page.waitForURL((url) => url.href !== 'about:blank', { timeout: 30_000 });
		while (!stopFrames) {
			const startedAt = Date.now();
			await page.screenshot({
				path: path.join(frameDirectory, `frame-${String(frameNumber).padStart(6, '0')}.jpg`),
				type: 'jpeg',
				quality: 96,
				scale: 'device',
			});
			frameNumber += 1;
			const remainder = Math.max(0, (1000 / captureFPS) - (Date.now() - startedAt));
			if (remainder > 0) await new Promise((resolve) => setTimeout(resolve, remainder));
		}
	})();
	try {
		await run(page);
		stopFrames = true;
		await frameTask;
		if (preparePoster) await preparePoster(page);
		await page.screenshot({
			path: path.join(captureDirectory, `${name}.png`),
			fullPage: false,
			scale: 'device',
		});
	} finally {
		stopFrames = true;
		await frameTask;
		await closeContext(context);
	}
	const recordingDurationMilliseconds = Date.now() - recordingStartedAt;
	await encodeFrames(
		frameDirectory,
		path.join(videoDirectory, `${name}.webm`),
		frameNumber,
		recordingDurationMilliseconds,
	);
	await rm(frameDirectory, { recursive: true, force: true });
}

async function encodeFrames(
	frameDirectory: string,
	outputPath: string,
	frameCount: number,
	durationMilliseconds: number,
): Promise<void> {
	const inputPattern = path.join(frameDirectory, 'frame-%06d.jpg');
	const sourceFPS = frameCount / (durationMilliseconds / 1000);
	const args = [
		'-y',
		'-framerate', sourceFPS.toFixed(6),
		'-i', inputPattern,
		'-an',
		'-c:v', 'libvpx-vp9',
		'-crf', '20',
		'-b:v', '0',
		'-pix_fmt', 'yuv420p',
		'-row-mt', '1',
		'-deadline', 'good',
		'-cpu-used', '2',
		'-r', '30',
		outputPath,
	];
	await new Promise<void>((resolve, reject) => {
		const encoder = spawn(ffmpegPath, args, { stdio: ['ignore', 'ignore', 'pipe'] });
		let errorOutput = '';
		encoder.stderr.on('data', (chunk) => {
			errorOutput += chunk.toString();
		});
		encoder.on('error', reject);
		encoder.on('close', (code) => {
			if (code === 0) resolve();
			else reject(new Error(`ffmpeg exited with ${code}:\n${errorOutput}`));
		});
	});
}

async function captureStill(
	browser: Browser,
	name: string,
	run: (page: Page) => Promise<void>,
): Promise<void> {
	await mkdir(captureDirectory, { recursive: true });
	const context = await browser.newContext({
		viewport: { width: 1440, height: 900 },
		deviceScaleFactor: 1,
		colorScheme: 'dark',
		reducedMotion: 'reduce',
	});
	const page = await context.newPage();
	page.setDefaultNavigationTimeout(45_000);
	try {
		await run(page);
		await page.screenshot({
			path: path.join(captureDirectory, `${name}.png`),
			fullPage: false,
		});
	} finally {
		await closeContext(context);
	}
}

async function closeContext(context: BrowserContext): Promise<void> {
	const close = context.close().catch(() => undefined);
	await Promise.race([
		close,
		new Promise<void>((resolve) => setTimeout(resolve, 5000)),
	]);
}

async function beat(page: Page, milliseconds = 1800): Promise<void> {
	await page.addStyleTag({ path: captureStyle });
	await page.waitForTimeout(milliseconds);
}

test.beforeAll(async ({ request }) => {
	await waitForRepository(request, 'RepoKarta');
	await waitForRepository(request, 'spring-petclinic-microservices');
	await waitForRepository(request, 'bank-of-anthos');
});

test('capture search workflow', async ({ browser }) => {
	await capturePage(browser, 'search-overview', async (page) => {
		await page.goto('/');
		await beat(page, 2200);
		await page.locator('#search-query').pressSequentially('OwnerResource', { delay: 95 });
		await beat(page, 1400);
		await page.locator('select[name="repo"]').selectOption({
			label: 'spring-petclinic-microservices',
		});
		await beat(page, 1300);
		await page.getByRole('button', { name: 'Search' }).click();
		await expect(page.getByRole('article').first()).toContainText('OwnerResource', {
			timeout: 90_000,
		});
		await beat(page, 3300);
		await page.getByRole('article').first().scrollIntoViewIfNeeded();
		await page.getByRole('article').first().hover();
		await beat(page, 2600);
		await page.getByRole('link', { name: /source/i }).first().click();
		await expect(page).toHaveURL(/\/source\//);
		await beat(page, 3000);
	}, async (page) => {
		await page.goto('/');
		await beat(page, 1000);
		await page.locator('#search-query').fill('OwnerResource');
		await page.locator('select[name="repo"]').selectOption({
			label: 'spring-petclinic-microservices',
		});
		await page.getByRole('button', { name: 'Search' }).click();
		await expect(page.getByRole('article').first()).toContainText('OwnerResource');
		await page.getByRole('article').first().scrollIntoViewIfNeeded();
		await beat(page, 1400);
	});
});

test('capture source browser workflow', async ({ browser, request }) => {
	const repository = await waitForRepository(request, 'spring-petclinic-microservices');
	const sourcePath =
		'spring-petclinic-customers-service/src/main/java/org/springframework/samples/petclinic/customers/web/OwnerResource.java';
	await capturePage(browser, 'source-browser', async (page) => {
		await page.goto(
			`/source/${repository.id}?path=${encodeURIComponent(sourcePath)}&lines=30-115&focus=39-75#L39`,
		);
		await expect(page).toHaveTitle(/OwnerResource\.java/, { timeout: 60_000 });
		await expect(page.locator('.source-viewer')).toContainText('class OwnerResource');
		await beat(page, 4200);
		await page.locator('.source-viewer').hover();
		await page.mouse.wheel(0, 420);
		await beat(page, 3600);
		await page.mouse.wheel(0, -250);
		await beat(page, 3600);
		const identifier = page.locator('input[name="identifier"]').first();
		if (await identifier.isVisible().catch(() => false)) {
			await identifier.fill('OwnerResource');
			await beat(page, 2800);
		}
		await page.mouse.wheel(0, -600);
		await beat(page, 3600);
	});
});

test('capture repository map workflow', async ({ browser, request }) => {
	const repository = await waitForRepository(request, 'spring-petclinic-microservices');
	await capturePage(browser, 'repository-map', async (page) => {
		await page.goto(`/maps?repository=${repository.id}`);
		await expect(page.locator('[data-map-status]')).toContainText(/facts|map ready/i, {
			timeout: 8 * 60 * 1000,
		});
		await beat(page, 4200);
		await page.getByRole('button', { name: 'Packages' }).click();
		await beat(page, 3800);
		await page.getByRole('button', { name: 'Dependencies' }).click();
		await beat(page, 3800);
		await page.getByRole('button', { name: 'Routes' }).click();
		await beat(page, 4200);
		const firstFact = page.locator('[data-map-fact]').first();
		if (await firstFact.isVisible().catch(() => false)) {
			await firstFact.click();
			await beat(page, 3200);
		}
	});
});

test('capture complete product surfaces', async ({ browser, request }) => {
	const petclinic = await waitForRepository(request, 'spring-petclinic-microservices');
	const repokarta = await waitForRepository(request, 'RepoKarta');
	const captures: Array<[string, string]> = [
		['dependencies-topology', `/dependencies?view=topology&repository=${petclinic.id}`],
		['dependency-inventory', `/dependencies?view=inventory&repository=${petclinic.id}`],
		['insights-overview', `/insights?view=overview&repository=${repokarta.id}`],
		['wiki-workspace', `/wiki?repository=${petclinic.id}`],
		['chat-workspace', '/chat'],
		['mcp-setup', '/mcp/setup'],
	];
	for (const [name, url] of captures) {
		await captureStill(browser, name, async (page) => {
			await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 45_000 });
			await expect(page.locator('body')).toBeVisible();
			await beat(page, 2200);
			if (name === 'mcp-setup') {
				await page.locator('.mcp-code-block-compact pre code').evaluate((element) => {
					const configuration = JSON.parse(element.textContent || '{}') as {
						mcpServers?: { repokarta?: { command?: string } };
					};
					if (configuration.mcpServers?.repokarta) {
						configuration.mcpServers.repokarta.command = 'repokarta';
						element.textContent = JSON.stringify(configuration, null, 2);
					}
				});
			}
		});
	}
});
