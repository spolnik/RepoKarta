# Enterprise identity and audit operations

RepoKarta evaluates enterprise authorization on every application request.
Provider authentication proves identity; RepoKarta then resolves the current
direct role, SCIM group membership, and exact provider-group mappings from
SQLite. This makes role changes, suspension, and deprovisioning effective on
the next request without erasing historical authorship.

## Provisioning boundary

Create a random bearer token of at least 24 characters in a file readable only
by the RepoKarta service account, then start the service with:

```text
-scim-token-file /etc/repokarta/scim-token
```

The SCIM 2.0 base URL is `/scim/v2`. RepoKarta supports:

- `GET`, `POST` on `/Users` and `/Groups`;
- `GET`, `PUT`, `PATCH`, `DELETE` on `/Users/{id}` and `/Groups/{id}`;
- bounded `startIndex`, `count`, and equality filters;
- service-provider configuration, resource types, and schemas;
- stable `externalId`, idempotent replacement, membership updates,
  suspension, and deprovisioning.

`DELETE /Users/{id}` suspends the identity instead of deleting its row.
Existing conversations keep their author ID. A later request using an existing
SAML or Cloudflare Access session is denied.

Do not place the bearer token itself in an environment file, command line,
diagnostic bundle, reverse-proxy log, or backup manifest. Reverse proxies must
forward `Authorization` on `/scim/v2/*` without logging it.

## Role and permission matrix

`reader` can read repositories allowed by repository policy, read shared
artifacts, manage only its own conversations, and export artifacts it may
read.

`knowledge-maintainer` adds AI chat and Wiki generation.

`administrator` adds cross-author conversation access, repository refresh and
acquisition, security configuration, direct and group role management, audit
search/export/retention, and destructive administration of RepoKarta-owned
data.

Loopback-local mode always resolves its single `local:admin` identity as
administrator and does not require SCIM configuration.

Unknown SAML or Cloudflare identities are observed as readers. An exact direct
assignment, SCIM group role, or configured provider-group mapping may elevate
them. Effective role is the highest current assignment. Unknown and removed
groups provide no elevation.

Bootstrap administrators can manage these settings at `/admin`. Application
administrators can use:

```text
GET    /api/admin/identities
PATCH  /api/admin/identities/{id}/role
PATCH  /api/admin/groups/{id}/role
GET    /api/admin/role-mappings
POST   /api/admin/role-mappings
DELETE /api/admin/role-mappings/{id}
GET    /api/admin/security
PUT    /api/admin/security
GET    /api/admin/audit
GET    /api/admin/audit/export
GET    /api/admin/audit/retention
PUT    /api/admin/audit/retention
```

## Audit evidence

Audit events contain actor, action, target, outcome, authentication provider,
request correlation ID, timestamp, and a small metadata object. Metadata keys
that can contain credentials, tokens, cookies, assertions, prompts, or
repository source content are dropped before persistence. Values and search
pages are bounded.

Search supports text, actor, action, outcome, RFC3339 time bounds, and
descending event-ID pagination. JSON and CSV exports are capped at 50,000
events and explicitly report truncation. Every result includes the configured
retention limits and the oldest retained timestamp; evidence before that
boundary is not claimed complete.

Retention defaults to 365 days and 100,000 events. The administrator may set
1–3,650 days and 100–10,000,000 events. Applying retention is the only
application path that deletes audit rows, and the change records a new audit
event after the purge.

Back up the SQLite database before upgrades. Audit evidence, identities,
groups, role mappings, and repository access policy are all part of that
database and must be restored as one consistent unit.
