# RepoKarta Handbook

The RepoKarta Handbook is a visual, evidence-backed guide built with Astro and
Starlight. Product screenshots and short WebM walkthroughs are captured from a
local RepoKarta process against pinned demo repositories.

## Local development

From the RepoKarta repository root:

```powershell
npm --prefix handbook install
npm --prefix handbook run astro -- dev --background
```

The handbook is then available at `http://localhost:4321`. Stop the background
server with:

```powershell
npm --prefix handbook run astro -- dev stop
```

## Build and validate

```powershell
npm --prefix handbook run build
npm --prefix handbook audit --audit-level=high
```

## Refresh visual captures

Clone `spring-petclinic-microservices` and `bank-of-anthos` beside RepoKarta,
then run:

```powershell
.\scripts\capture-handbook.ps1
```

The script builds an isolated RepoKarta binary, indexes pinned local clones,
runs the Playwright capture scenarios, and refreshes
`handbook/capture-manifest.json`.
