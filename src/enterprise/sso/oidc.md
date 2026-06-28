# SSO — Generic OIDC

_Enterprise_

Set up single sign-on with **any standards-compliant OpenID Connect (OIDC)
provider** — Okta, Auth0, Keycloak, Ping, or your own issuer. The connection type
is `oidc`; unlike Google or Microsoft, **you supply the issuer URL** and Versus
discovers the endpoints from it.

## What you'll need

| Item | Where it comes from |
|---|---|
| An **OIDC application** at your IdP | Your provider's admin console |
| The **issuer (discovery) URL** | The base URL whose `<issuer>/.well-known/openid-configuration` resolves, e.g. `https://accounts.example.com` |
| The Versus **callback URL** | `https://<your-versus-host>/enterprise/api/sso/<org>/callback` (use `default` for a single-org deployment) |
| Your **email domain(s)** | e.g. `acme.com` — the allow-list of who may sign in |

## Step 1 — Create the OIDC application at your IdP

1. In your provider's admin console, create a new **OpenID Connect / OAuth 2.0
   web application** (Authorization Code flow).
2. Set its **redirect URI** (sometimes called *callback* or *sign-in redirect*)
   to the Versus callback URL exactly:

   ```text
   https://<your-versus-host>/enterprise/api/sso/<org>/callback
   ```

3. Note the application's **client ID** and **client secret**.
4. Find the provider's **issuer URL**. It must be the value whose discovery
   document is served at `<issuer>/.well-known/openid-configuration`, and it must
   exactly match the `iss` claim in the ID tokens the provider mints.

## Step 2 — Add the connection in Versus

In the console open the **Agent** page → **Single Sign-On (SSO)** panel → **Add
identity provider**, then fill in:

| Field | Value |
|---|---|
| **Provider** | `Generic OIDC (issuer URL)` |
| **Connection ID** | A slug such as `okta` (lowercase; used in the login URL). |
| **Display name** | e.g. `Okta` — the label on the sign-in button. |
| **Issuer (IdP discovery URL)** | The issuer URL from Step 1, e.g. `https://accounts.example.com`. |
| **Client ID** | The client ID from Step 1. |
| **Client secret** | The client secret from Step 1 (sealed on save). |
| **Redirect URL (callback)** | The same callback URL you registered in Step 1. |
| **Scopes** | `email, profile` (`openid` is always implied). |
| **Allowed email domains** | Your email domain(s), e.g. `acme.com`. **Required.** |
| **Enabled** | Turn on to show the sign-in button. |

Click **Save**. Versus fetches the issuer's discovery document, seals the secret
immediately, and confirms with a masked summary (whether a secret is stored and
its last four characters — never the secret itself).

## Step 3 — Test the login

Once the connection is **enabled**, the console sign-in screen shows a **“Sign in
with <your display name>”** button. Click it (or visit the login URL directly):

```text
https://<your-versus-host>/enterprise/api/sso/<org>/login/<connection-id>
```

You'll be redirected to your IdP, sign in, and land back on Versus with a session.
A user whose email domain isn't on your allow-list is refused — that's expected.

The first SSO user signs in as **viewer**. Promote one to `admin`/`owner` from
the **Members** panel, then [disable the built-in default admin](./overview.md#the-bootstrap-flow-who-creates-the-first-connection).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Save fails or login won't start | The **issuer URL** is wrong — confirm `<issuer>/.well-known/openid-configuration` loads in a browser. |
| Login redirects but comes back rejected | The signed-in email's domain isn't in **Allowed email domains**, or the IdP's `iss` claim doesn't exactly match the issuer you entered. |
| IdP rejects the redirect | The **redirect URI** registered at the IdP doesn't byte-for-byte match the Versus callback URL. |

## Editing or rotating later

Re-open the connection any time to change the issuer, allowed domains, scopes, or
display name. To **rotate** the client secret, generate a new one at your IdP and
paste it into the **Client secret** field. Leave that field **blank** to keep the
stored secret unchanged.
