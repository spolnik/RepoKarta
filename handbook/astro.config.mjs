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
			title: 'RepoKarta Handbook',
			description:
				'A visual, evidence-backed guide to searching, exploring, mapping, and documenting software with RepoKarta.',
			favicon: '/favicon-32.png',
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
					label: 'Start here',
					items: [
						{ label: 'Welcome', slug: 'index' },
						{ label: 'The demo workspace', slug: 'start/demo-workspace' },
						{ label: 'Evidence and completeness', slug: 'start/evidence-and-completeness' },
					],
				},
				{
					label: 'Everyday workflows',
					items: [
						{ label: 'Search', slug: 'workflows/search' },
						{ label: 'Source browser', slug: 'workflows/source-browser' },
						{ label: 'Repository maps', slug: 'workflows/maps' },
					],
				},
				{
					label: 'Configuration',
					items: [
						{ label: 'Local launch profiles', slug: 'configuration/local-launch' },
						{ label: 'Command-line reference', slug: 'configuration/command-line' },
					],
				},
				{
					label: 'Coming next',
					collapsed: true,
					items: [
						{ label: 'Handbook roadmap', slug: 'reference/roadmap' },
					],
				},
			],
		}),
	],
});
