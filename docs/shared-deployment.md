# Shared deployment operations

RepoKarta remains a single-process, read-only code intelligence service. A
shared deployment adds authenticated users and repository authorization; it
does not turn RepoKarta into a Git host or allow repository mutation.

## Production boundary

- Terminate TLS at a trusted reverse proxy and forward only to RepoKarta's
  private listener.
- Use `cloudflare-access` or `saml` for named users. `open` is an explicit
  startup-gated exception and has one anonymous identity.
- Run RepoKarta as a dedicated operating-system account. That account needs
  read access to the configured repository root and read/write access only to
  the RepoKarta data directory.
- Keep the bootstrap administrator password and SCIM bearer-token files
  readable only by that account. The password, SCIM token, and SAML private
  key must not be copied into an environment file, command history, backup
  manifest, or diagnostic bundle.
- Block direct network access to the RepoKarta listener. The public URL must
  be the exact HTTPS origin presented by the reverse proxy.

## First start

1. Select the systemd, launchd, or Windows launcher example under `deploy/`.
   Copy it to protected service configuration and replace every example path
   and URL. The environment example documents the supported non-secret values.
2. Create the data directory, administrator password file, optional SCIM token
   file, and dedicated service account.
3. Start RepoKarta, then sign in at `/admin` with the bootstrap credentials.
4. Configure Cloudflare Access JWT or SAML and verify a normal user login.
5. In **Repository and artifact access**, assign an owner and explicit user or
   identity-provider group grants. Have the user open `GET /api/whoami` and
   copy its exact `id` or `groups` value. Upgraded and newly discovered
   repositories start private to `local:admin`; no shared user receives
   implicit access.
6. Assign reader, knowledge-maintainer, and administrator roles directly or
   through SCIM/provider groups. Verify `GET /api/whoami` reports the expected
   effective role and permissions.
7. Verify the same identity can see only the expected repositories in Search,
   Maps, Wiki, Chat, exports, and `/api/repositories`.

`shared` visibility is an explicit instance-wide grant. Prefer private
visibility plus group grants for team repositories.

## Reverse proxy

Preserve the original `Host`, scheme, and client origin. Do not expose the
private listener directly. The proxy must reject arbitrary `Host` values and
must not log assertion tokens or cookies. For Cloudflare Access, forward
`Cf-Access-Jwt-Assertion`. For SAML, preserve secure cookies and all `/saml/`
paths.

If SCIM is enabled, expose `/scim/v2/*` only through the same trusted HTTPS
origin, forward its `Authorization` header, and suppress that header in proxy
logs. Restrict the SCIM token to the provisioning client. See
`docs/enterprise-administration.md` for supported resources and operations.

The unauthenticated health probe is:

```text
GET /healthz
```

It returns only status and application version. Use an authenticated
`GET /api/repositories` as the post-deploy authorization smoke test.

## Backup and restore

Stop the service before a filesystem-level backup. Back up the complete
RepoKarta data directory as one unit: SQLite, indexes, maps, Wiki files,
conversation attachments, and the SAML identity files. Back up the SAML
private key as a secret. Repository worktrees are external read-only inputs and
need their own backup policy.

To restore, keep the service stopped, restore into an empty directory with the
same ownership, then start the same or newer RepoKarta version. Confirm the
database migration, authentication provider, repository policies, and one
commit-pinned source read before opening traffic.

## Upgrade and rollback

1. Download the platform archive and verify its adjacent SHA-256 checksum.
2. Stop RepoKarta and take a complete data-directory backup.
3. Replace the binary and packaged operational templates, leaving the data
   directory untouched.
4. Start the service and check `/healthz`, admin storage diagnostics, user
   authentication, effective roles, audit recording, repository isolation,
   Search, one Map, one Wiki page, and a SCIM list call when configured.

Database migrations are forward-only. Rollback requires restoring the
pre-upgrade data-directory backup together with the previous binary.

## Deprovisioning

Deprovision users through SCIM before or together with disabling them upstream.
RepoKarta suspends the identity and denies its next request even when an
upstream browser session still exists. Remove obsolete repository grants and
provider-group role mappings. Existing conversations remain attributed to
their stable author but are not available to another user. To retire the
deployment, stop the service, revoke the IdP application and SCIM token,
securely remove the bootstrap password, SCIM token, and SAML key, then remove
only the explicitly configured RepoKarta data directory. Never remove the
external repository root as part of RepoKarta deprovisioning.
