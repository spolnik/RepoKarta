import { expect, test, type APIRequestContext, type Browser, type Page } from '@playwright/test';
import { mkdir } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

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
): Promise<void> {
	await mkdir(captureDirectory, { recursive: true });
	await mkdir(videoDirectory, { recursive: true });
	const context = await browser.newContext({
		viewport: { width: 1440, height: 900 },
		deviceScaleFactor: 1,
		colorScheme: 'dark',
		reducedMotion: 'reduce',
		recordVideo: {
			dir: path.resolve(
				path.dirname(fileURLToPath(import.meta.url)),
				'../test-results/raw-video',
			),
			size: { width: 1280, height: 720 },
		},
	});
	const page = await context.newPage();
	const video = page.video();
	try {
		await run(page);
		await page.screenshot({
			path: path.join(captureDirectory, `${name}.png`),
			fullPage: false,
		});
	} finally {
		await context.close();
		if (video) {
			await video.saveAs(path.join(videoDirectory, `${name}.webm`));
		}
	}
}

async function settle(page: Page): Promise<void> {
	await page.addStyleTag({ path: captureStyle });
	await page.waitForTimeout(650);
}

test.beforeAll(async ({ request }) => {
	await waitForRepository(request, 'RepoKarta');
	await waitForRepository(request, 'spring-petclinic-microservices');
	await waitForRepository(request, 'bank-of-anthos');
});

test('capture search workflow', async ({ browser }) => {
	await capturePage(browser, 'search-overview', async (page) => {
		await page.goto('/');
		await settle(page);
		await page.locator('#search-query').pressSequentially('OwnerResource', { delay: 55 });
		await page.locator('select[name="repo"]').selectOption({
			label: 'spring-petclinic-microservices',
		});
		await page.getByRole('button', { name: 'Search' }).click();
		await expect(page.getByRole('article').first()).toContainText('OwnerResource', {
			timeout: 90_000,
		});
		await settle(page);
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
		await settle(page);
	});
});

test('capture repository map workflow', async ({ browser, request }) => {
	const repository = await waitForRepository(request, 'spring-petclinic-microservices');
	await capturePage(browser, 'repository-map', async (page) => {
		await page.goto(`/maps?repository=${repository.id}`);
		await expect(page.locator('[data-map-status]')).toContainText(/facts|map ready/i, {
			timeout: 8 * 60 * 1000,
		});
		await page.getByRole('button', { name: 'Routes' }).click();
		await settle(page);
	});
});
