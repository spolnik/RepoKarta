# Shared service templates

These systemd, launchd, and Windows foreground-launcher templates are examples,
not installers. Review all paths, the service account, reverse-proxy boundary,
and identity-provider values before enabling them. The full runbook is
`docs/shared-deployment.md`.

The systemd unit expects:

- the binary at `/opt/repokarta/repokarta`;
- repositories beneath `/srv/repositories`;
- RepoKarta-owned state at `/var/lib/repokarta`;
- configuration at `/etc/repokarta/repokarta.env`; and
- a mode-0600 administrator password file referenced by that configuration.
