import { cp, mkdir, readdir, rm } from 'node:fs/promises';
import path from 'node:path';

const projectDirectory = process.cwd();
const distributionDirectory = path.join(projectDirectory, 'dist');
const clientDirectory = path.join(distributionDirectory, 'client');
const serverDirectory = path.join(distributionDirectory, 'server');
const metadataDirectory = path.join(distributionDirectory, '.openai');

await rm(clientDirectory, { force: true, recursive: true });
await rm(serverDirectory, { force: true, recursive: true });
await rm(metadataDirectory, { force: true, recursive: true });
await mkdir(clientDirectory, { recursive: true });

const entries = await readdir(distributionDirectory, { withFileTypes: true });
for (const entry of entries) {
	if (entry.name === 'client') continue;
	await cp(
		path.join(distributionDirectory, entry.name),
		path.join(clientDirectory, entry.name),
		{ recursive: entry.isDirectory() },
	);
}

await mkdir(serverDirectory, { recursive: true });
await cp(
	path.join(projectDirectory, 'hosting', 'worker.js'),
	path.join(serverDirectory, 'index.js'),
);

await mkdir(metadataDirectory, { recursive: true });
await cp(
	path.join(projectDirectory, '.openai', 'hosting.json'),
	path.join(metadataDirectory, 'hosting.json'),
);
