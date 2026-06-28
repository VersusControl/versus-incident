# Single Sign-On (SSO)

_Enterprise_

Let your team sign in to the Enterprise SRE Agent with your own identity
provider (IdP). SSO is configured **per organization, from the admin UI** —
there is nothing to set in a config file and no SSO secret in the environment.

An org can register **several** named IdP connections at once. Each connection
becomes one **“Sign in with …”** button on the console login screen and runs the
OpenID Connect **Authorization Code flow with PKCE**, fully validating the IdP's
ID token (signature, issuer, audience, expiry, nonce) before it trusts a login.

## Supported providers

Pick the page that matches your IdP:

| Provider | Connection type | When to use it |
|---|---|---|
| [Google Workspace](./google.md) | `google` | Google Workspace / Google accounts. Issuer is fixed (`accounts.google.com`). |
| [Microsoft Entra ID (Azure AD)](./azure.md) | `azure` | Microsoft Entra ID / Azure AD. Issuer is derived from your **directory (tenant) ID**. |
| [Generic OIDC](./oidc.md) | `oidc` | Any standards-compliant OpenID Connect issuer (Okta, Auth0, Keycloak, …). You supply the **issuer (discovery) URL**. |

These are the only three connection types the agent supports. Every provider
above is one of them — there is no separate SAML or LDAP path.

## Before you start

| You need | Why |
|---|---|
| A **Versus Enterprise license** | SSO is an enterprise feature. Without a license the SSO panel stays locked (Enterprise upsell). |
| The **built-in default admin** signed in | A brand-new deployment has no SSO connection yet, so you add the first one while logged in as the built-in `admin` (owner). See [the bootstrap flow](#the-bootstrap-flow-who-creates-the-first-connection) below. |
| An **OAuth / OIDC application** registered in your IdP | You'll copy its **client ID** and **client secret** into Versus and register Versus's **callback URL** back in the IdP. |

## The bootstrap flow (who creates the first connection)

The admin UI that manages SSO is itself gated on a signed-in session, and a
fresh deployment has no SSO connection — so the **first** connection is created
by the built-in default admin, a non-SSO root account.

There is **no bootstrap env of any kind**. The flow is:

1. **First licensed boot** prints a generated password for the built-in `admin`
   **once** to the container log:

   ```text
   ================================================================================
     DEFAULT ADMIN CREDENTIALS (shown once — copy them now)
   --------------------------------------------------------------------------------
     Username: admin
     Password: <generated>
   --------------------------------------------------------------------------------
     Change or disable this account after your first login.
   ================================================================================
   ```

   The password is generated with `crypto/rand`, stored only as a bcrypt hash,
   and **never re-printed or regenerated** on reboot. Copy it from the log.

2. **Log in as the built-in admin** at the console's local login (username
   `admin` and the printed password). Behind the UI this is
   `POST /enterprise/api/auth/local/login`, which mints the org-bound session
   cookie. `admin` holds the **owner** role, so it can manage SSO.

3. **Add your first SSO connection** in the UI — follow the
   [provider page](#supported-providers) for your IdP.

4. **Your team signs in over SSO.** A brand-new SSO user lands as the read-only
   **viewer** role (least privilege).

5. **Promote** one SSO user to **admin** or **owner** from the **Members** panel.

6. **Disable the built-in admin** from the **Members** panel once a real
   admin/owner exists. The server **refuses** while `admin` is the only
   administrator (a no-lockout guard), and disabling it immediately invalidates
   its session. You can re-enable it later for break-glass recovery.

> See [Getting Started](../getting-started.md) for the full first-boot
> walkthrough.

## Shared concepts

These apply to every provider page.

### Deployment org

This binary serves a single organization, derived from your `LICENSE_KEY`. The
admin UI resolves it for you, so you never type it. A standard single-org
deployment uses `default`. The org appears in the **redirect (callback) URL**
you register at the IdP:

```text
https://<your-versus-host>/enterprise/api/sso/<org>/callback
```

All connections in an org **share** this one callback URL. The per-connection
**login URL** (the button target) is:

```text
https://<your-versus-host>/enterprise/api/sso/<org>/login/<connection-id>
```

### Allowed email domains (fail closed)

Every connection carries an **allow-list of email domains** (e.g. `acme.com`). A
login only succeeds if the user's email domain is on the list — so a leaked link
can't admit an outside account. The list is **required**: if it is empty, **every**
login through that connection is rejected (fail closed).

### Roles

| Role | What it can do |
|---|---|
| `viewer` (default) | Read-only. **Every new SSO user starts here.** |
| `responder` | Read-only for admin controls (operational role). |
| `admin` / `owner` | Manage SSO connections, runtime mode, AI settings, members. |

Promote a viewer to `admin`/`owner` from the **Members** panel; the built-in
`admin` is `owner` and is used only to bootstrap (then disabled).

### Client-secret handling

When you save a connection the client secret is **sealed at rest** (AES-256-GCM)
the moment it is saved. Versus never shows it again, never logs it, and never
returns it from the API — the masked view only shows whether a secret is stored
and its **last four characters**. When editing a connection later, leave the
client secret **blank** to keep the stored one; type a new value only when you
rotate it at the IdP.

## Where to configure it in the UI

In the console, open the **Agent** page and find the **Single Sign-On (SSO)**
panel (the *identity providers* control). It is editable only when you're signed
in with `admin`/`owner`; a read-only role shows a **requires the admin role**
notice.

Click **Add identity provider** and you'll fill these fields (each provider page
spells out the exact values):

| Field | Notes |
|---|---|
| **Provider** | `Google`, `Microsoft Entra (Azure AD)`, or `Generic OIDC (issuer URL)`. Fixed after creation. |
| **Connection ID** | A lowercase slug (`[a-z0-9-_]`, 1–64 chars) used in the login URL. |
| **Display name** | The label on the “Sign in with …” button. |
| **Directory (tenant) ID** | Microsoft Entra only — see the [Azure page](./azure.md). |
| **Issuer (IdP discovery URL)** | Generic OIDC only — see the [OIDC page](./oidc.md). |
| **Client ID** | From your IdP application. |
| **Client secret** | From your IdP application — sealed on save; leave blank when editing to keep it. |
| **Redirect URL (callback)** | The shared callback URL above. Must match what you register at the IdP. |
| **Scopes** | Comma/space separated. `openid` is always implied; `email` and `profile` are recommended. |
| **Allowed email domains** | Comma/space separated. Required — empty rejects all logins. |
| **Enabled** | Show this provider's button on the login screen. |

## Enforcing SSO (optional)

Once at least one connection is enabled, an owner can make SSO **mandatory** for
the org from the same panel:

| Setting | Effect |
|---|---|
| `require_sso` | Drops the non-SSO (gateway-secret) console login fallback — the provider button becomes the only way in. |
| `require_mfa` | Records that the org expects multi-factor logins (from the IdP's `amr` claim). Only meaningful under `require_sso`. |

You cannot turn on `require_sso` for an org that has no enabled connection — the
request is rejected so you can't lock yourself out. Until SSO is enforced, the
gateway-secret login stays available as a break-glass fallback. Every policy
change and every blocked non-SSO attempt is written to the audit log.

## Good to know

- **No secrets in env.** The client secret lives only in Versus's encrypted
  store, protected by an enterprise key generated automatically on first
  licensed boot. There is **no** SSO secret env var.
- **Per-organization.** Each org has its own connections, login URLs, and
  callback URL. One org's login can never complete against another org's.
- **Fail-closed by design.** A missing, disabled, or corrupt connection disables
  **login only** — it never leaves the agent in a broken-auth state. While
  `require_sso` is off, the gateway-secret login keeps console access available.
- **Short-lived sessions.** A signed-in session is bound to the org, stored in a
  signed `HttpOnly` cookie, and expires automatically. Users can log out; owners
  can force-revoke every session for the org at once.
