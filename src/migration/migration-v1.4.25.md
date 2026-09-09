# Migrating to v1.4.25

## Enterprise browser actions can return 403 when `public_host` is missing

v1.4.25 enforces exact-origin CSRF validation for unsafe Enterprise API
requests authenticated by a local-admin or SSO session cookie. After upgrading,
an operator may see this error when changing Alert fatigue or another Enterprise
setting:

```text
403 Forbidden
CSRF validation failed
```

This commonly occurs when Versus runs behind a TLS-terminating or
Host-rewriting reverse proxy and `public_host` is empty. The browser sends the
external origin, such as `https://versus.example.com`, while the application
sees the proxy's internal HTTP origin. The exact-origin check rejects the
mismatch.

Set the root-level `public_host` in `config/config.yaml` to the exact origin
operators use in their browser:

```yaml
public_host: https://versus.example.com
```

For Helm installations, set the equivalent chart value:

```yaml
config:
  publicHost: https://versus.example.com
```

The value must include the browser-visible `http` or `https` scheme, hostname,
and non-default port when applicable. Do not include a path, query, fragment, or
userinfo. For example:

```yaml
public_host: https://versus.example.com:8443
```

After changing the value:

1. Restart every Versus replica so they load the same configuration.
2. Sign out and sign in again with the local admin or SSO account.
3. Retry the Alert fatigue change.

Do not disable CSRF protection or add a gateway secret to work around this
error. Licensed deployments authenticate the admin surface with the Enterprise
session cookie. Forwarded host and protocol headers are not trusted as a
replacement for `public_host`.

Direct deployments that do not terminate TLS or rewrite the `Host` header can
leave `public_host` empty, although setting it explicitly is recommended for a
stable external origin.

## Strict sibling configuration keys

`tools.yaml` and `agent_sources.yaml` now reject unknown keys instead of
silently ignoring them. This is an intentional breaking validation change:
remove misspelled, obsolete, or unsupported keys before restarting. Environment
references are expanded only after YAML parsing and only within decoded string
scalars, so an expanded value cannot inject additional YAML structure.

For compatibility, list fields still accept either YAML lists or comma-separated
strings. For example, both `endpoint_cidrs: [10.0.0.0/8, 192.168.0.0/16]` and
`endpoint_cidrs: 10.0.0.0/8,192.168.0.0/16` remain valid.