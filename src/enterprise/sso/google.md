# SSO with Google Workspace

_Enterprise_

## Concept

Your team signs in to Versus with their Google Workspace accounts. You create
an OAuth client in Google Cloud and paste its ID and secret into Versus. There
is no issuer to enter: Versus uses Google's (`https://accounts.google.com`).

## Example

Acme's Workspace domain is `acme.com`. After this setup, the Versus login
screen shows **Sign in with Google**. `maria@acme.com` signs in and lands as a
**viewer**. An account outside `acme.com` is refused.

## What you need

| Item | Where it comes from |
|---|---|
| A Google Cloud project | [console.cloud.google.com](https://console.cloud.google.com) |
| The Versus redirect URL | **Admin → Identity providers → Add identity provider**, field **Redirect URL (callback)** |
| Your Workspace domain | For example `acme.com` |

## 1. Create the OAuth client in Google Cloud

1. Open **APIs & Services → Credentials**.
2. If asked, set up the **OAuth consent screen**. **Internal** is fine when only
   your Workspace signs in.
3. Click **Create credentials → OAuth client ID**.
4. Pick **Web application** and give it a name, such as `Versus`.
5. Under **Authorized redirect URIs**, add the Versus redirect URL exactly:

   ```text
   https://<your-versus-host>/enterprise/api/sso/<org>/callback
   ```

6. Click **Create** and keep the **Client ID** and **Client secret**.

## 2. Add the provider in Versus

In **Admin → Identity providers**, click **Add identity provider**:

| Field | Value |
|---|---|
| **Provider** | Google |
| **Connection ID** | `google` |
| **Display name** | `Google` (the button reads **Sign in with Google**) |
| **Client ID** | From step 1 |
| **Client secret** | From step 1 |
| **Redirect URL (callback)** | Keep the prefilled value; it must match step 1 |
| **Scopes** | `email, profile` |
| **Allowed email domains** | `acme.com` (required) |
| **Enabled** | On |

Click **Add provider**. Versus shows only whether a secret is stored and its
last four characters.

## 3. Test it

Sign out. The login screen now shows **Sign in with Google**. Sign in with a
Workspace account; you land in Versus as a **viewer**. Promote someone to
**admin** or **owner** in **Admin → Members & roles**, then disable the
built-in admin (see [SSO overview](./overview.md#turn-it-on)).

## Change it later

Open the provider again to change the allowed domains, scopes or display name.
To rotate the secret, create a new one in Google Cloud and paste it into
**Client secret**. Leave the field blank to keep the stored secret.

See the [SSO overview](./overview.md#reference) for roles, URLs and login
enforcement.
