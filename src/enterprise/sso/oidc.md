# SSO with Generic OIDC

_Enterprise_

## Concept

Use any OpenID Connect provider: Okta, Auth0, Keycloak, Ping or your own. You
create an app at the provider and give Versus its **issuer URL**, client ID and
secret. Versus reads the provider's endpoints from
`<issuer>/.well-known/openid-configuration`.

## Example

Acme uses Okta at `https://acme.okta.com`. After this setup, the Versus login
screen shows **Sign in with Okta**. `maria@acme.com` signs in and lands as a
**viewer**. An account outside `acme.com` is refused.

## What you need

| Item | Where it comes from |
|---|---|
| An OIDC web app at your provider | Your provider's admin console |
| The issuer URL | Your provider. `<issuer>/.well-known/openid-configuration` must open in a browser. |
| The Versus redirect URL | **Admin → Identity providers → Add identity provider**, field **Redirect URL (callback)** |
| Your email domain | For example `acme.com` |

## 1. Create the app at your provider

1. Create an **OpenID Connect web application** that uses the authorization
   code flow.
2. Set its redirect URI (also called callback or sign-in redirect) to the Versus
   redirect URL exactly:

   ```text
   https://<your-versus-host>/enterprise/api/sso/<org>/callback
   ```

3. Note the **client ID** and **client secret**.
4. Find the **issuer URL**. It must be exactly the issuer your provider puts in
   its tokens.

## 2. Add the provider in Versus

In **Admin → Identity providers**, click **Add identity provider**:

| Field | Value |
|---|---|
| **Provider** | Generic OIDC (issuer URL) |
| **Connection ID** | `okta` |
| **Display name** | `Okta` (the button reads **Sign in with Okta**) |
| **Issuer (IdP discovery URL)** | `https://acme.okta.com` |
| **Client ID** | From step 1 |
| **Client secret** | From step 1 |
| **Redirect URL (callback)** | Keep the prefilled value; it must match step 1 |
| **Scopes** | `email, profile` |
| **Allowed email domains** | `acme.com` (required) |
| **Enabled** | On |

Click **Add provider**. Versus shows only whether a secret is stored and its
last four characters.

## 3. Test it

Sign out. The login screen now shows **Sign in with** and your display name.
Sign in; you land in Versus as a **viewer**. Promote someone to **admin** or
**owner** in **Admin → Members & roles**, then disable the built-in admin (see
[SSO overview](./overview.md#turn-it-on)).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Save fails, or the login does not start | Wrong issuer URL. Check that `<issuer>/.well-known/openid-configuration` opens. |
| You sign in at the provider but Versus refuses you | Your email domain is not allowed, or the issuer you entered differs from the one in the provider's tokens. |
| The provider rejects the redirect | The redirect URI at the provider differs from the Versus one. They must match exactly. |

## Change it later

Open the provider again to change the issuer, allowed domains, scopes or
display name. To rotate the secret, create a new one at your provider and paste
it into **Client secret**. Leave the field blank to keep the stored secret.

See the [SSO overview](./overview.md#reference) for roles, URLs and login
enforcement.
