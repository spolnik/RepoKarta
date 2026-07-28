# Dependency management

The Dependencies workspace is read-only. Declaration evidence is always bound
to a repository revision, manifest path, and line; registry observations have
their own source and timestamp.

Version checks use the explicit states `current`, `behind`, `ahead`,
`prerelease`, `unavailable`, `private_internal`, `unresolved`,
`registry_error`, and `stale`. Version distance is reported separately as
`major`, `minor`, `patch`, `none`, or `unknown`. The browser and JSON API can
filter by ecosystem, package, repository, check status, and distance.

Registry routing is fail closed. Explicitly configured private registry
prefixes are evaluated before public registries. Scoped npm packages and
coordinates with common internal/private markers are not sent to a public
registry unless an administrator adds an explicit safe registry route. A
skipped declaration is returned as `private_internal`; it is not silently
omitted.

Private routes are configured with `REPOKARTA_DEPENDENCY_REGISTRIES`. The value
is a JSON array; keep credentials in the environment variable named by
`token_env`, not in the JSON itself:

```json
[
  {
    "ecosystem": "npm",
    "base_url": "https://registry.example.test",
    "metadata_url_template": "/{package}",
    "package_prefixes": ["@acme/"],
    "token_env": "ACME_REGISTRY_TOKEN"
  }
]
```

An explicit route may also identify a deliberately public registry for a
matching prefix. RepoKarta never falls back from a matched private route to a
public registry when that route is unavailable or returns an error.

Cached observations, offline behavior, rate limits, stale states, and registry
errors remain visible. RepoKarta never edits manifests or lockfiles.
