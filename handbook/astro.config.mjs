// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// https://astro.build/config
export default defineConfig({
	vite: {
		server: {
			watch: {
				ignored: [
					'**/.capture-bin/**',
					'**/.capture-cache/**',
					'**/.capture-data/**',
					'**/.demo-workspace/**',
					'**/capture/playwright-report/**',
					'**/capture/test-results/**',
				],
			},
		},
	},
	integrations: [
		starlight({
			title: 'RepoKarta Field Guide',
			description:
				'The complete visual field guide to RepoKarta: code intelligence with evidence, from one repository to a software fleet.',
			favicon: '/favicon-32.png',
			head: [
				{ tag: 'meta', attrs: { property: 'og:image', content: '/og.png' } },
				{ tag: 'meta', attrs: { property: 'og:image:width', content: '1200' } },
				{ tag: 'meta', attrs: { property: 'og:image:height', content: '630' } },
				{ tag: 'meta', attrs: { name: 'twitter:card', content: 'summary_large_image' } },
			],
			logo: {
				src: './src/assets/repokarta-mark.png',
				alt: 'RepoKarta',
			},
			customCss: ['./src/styles/repokarta.css'],
			lastUpdated: true,
			editLink: {
				baseUrl: 'https://github.com/spolnik/RepoKarta/edit/main/handbook/',
			},
			social: [
				{
					icon: 'github',
					label: 'RepoKarta on GitHub',
					href: 'https://github.com/spolnik/RepoKarta',
				},
			],
			sidebar: [
				{
					label: '00 · Orient',
					items: [
						{ label: 'Field guide', slug: 'index' },
						{ label: 'Three-system laboratory', slug: 'start/demo-workspace' },
						{ label: 'How RepoKarta works', slug: 'start/how-it-works' },
						{ label: 'Evidence contract', slug: 'start/evidence-and-completeness' },
					],
				},
				{
					label: '01 · Investigate',
					items: [
						{ label: 'Search, contexts & monitors', slug: 'workflows/search' },
						{ label: 'Source, symbols & history', slug: 'workflows/source-browser' },
						{ label: 'Maps & system topology', slug: 'workflows/maps' },
						{ label: 'Dependencies & advisories', slug: 'workflows/dependencies' },
						{ label: 'Insights & trends', slug: 'workflows/insights' },
					],
				},
				{
					label: '02 · Explain',
					items: [
						{ label: 'Deep Wiki', slug: 'workflows/wiki' },
						{ label: 'Chat & Deep Search', slug: 'workflows/chat' },
						{ label: 'MCP for coding agents', slug: 'workflows/mcp' },
					],
				},
				{
					label: '03 · Operate',
					items: [
						{ label: 'Local launch & providers', slug: 'configuration/local-launch' },
						{ label: 'Shared deployment & access', slug: 'configuration/shared-deployment' },
						{ label: 'Storage, maintenance & telemetry', slug: 'configuration/operations' },
					],
				},
				{
					label: '04 · Reference',
					collapsed: true,
					items: [
						{ label: 'Complete feature atlas', slug: 'reference/feature-atlas' },
						{ label: 'CLI & environment', slug: 'configuration/command-line' },
						{ label: 'MCP tool catalog', slug: 'reference/mcp-tools' },
					],
				},
			],
		}),
	],
});
