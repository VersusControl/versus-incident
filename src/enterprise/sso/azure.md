# SSO with Microsoft Entra ID

_Enterprise_

## Concept

Your team signs in to Versus with their Microsoft Entra ID (Azure AD)
accounts. You register an app in Entra and paste its client ID, secret and
directory (tenant) ID into Versus. Versus builds the issuer from the tenant:

```text
https://login.microsoftonline.com/<tenant>/v2.0
```

Leave the tenant blank to use the multi-tenant `common` authority. For sign-in
from your own organization only, enter your tenant ID.

## Example

Acme's Entra tenant holds `acme.com`. After this setup, the Versus login screen
shows **Sign in with Microsoft**. `maria@acme.com` signs in and lands as a
**viewer**. An account outside `acme.com` is refused.

## What you need

| Item | Where it comes from |
|---|---|
| Access to the Microsoft Entra admin center | [entra.microsoft.com](https://entra.microsoft.com) or the Azure portal |
| The Versus redirect URL | **Admin → Identity providers → Add identity provider**, field **Redirect URL (callback)** |
| Your email domain | For example `acme.com` |

## 1. Register the app in Entra ID

1. Open **Identity → Applications → App registrations** and click **New
   registration**.
2. Name it, for example `Versus`, and pick the supported account types for your
   tenant.
3. Under **Redirect URI**, choose **Web** and enter the Versus redirect URL
   exactly:

   ```text
   https://<your-versus-host>/enterprise/api/sso/<org>/callback
   ```

4. Click **Register**. On **Overview**, copy the **Application (client) ID** and
   the **Directory (tenant) ID**.
5. Open **Certificates & secrets → Client secrets → New client secret**. Copy the
   secret **Value** (not the secret ID) right away; it is shown once.

## 2. Add the provider in Versus

In **Admin → Identity providers**, click **Add identity provider**:

| Field | Value |
|---|---|
| **Provider** | Microsoft Entra (Azure AD) |
| **Connection ID** | `entra` |
| **Display name** | `Microsoft` (the button reads **Sign in with Microsoft**) |
| **Directory (tenant) ID** | From step 1. Blank uses `common`. |
| **Client ID** | The **Application (client) ID** from step 1 |
| **Client secret** | The secret **Value** from step 1 |
| **Redirect URL (callback)** | Keep the prefilled value; it must match step 1 |
| **Scopes** | `email, profile` |
| **Allowed email domains** | `acme.com` (required) |
| **Enabled** | On |

Click **Add provider**. Versus shows only whether a secret is stored and its
last four characters.

## 3. Test it

Sign out. The login screen now shows **Sign in with Microsoft**. Sign in; you
land in Versus as a **viewer**. Promote someone to **admin** or **owner** in
**Admin → Members & roles**, then disable the built-in admin (see
[SSO overview](./overview.md#turn-it-on)).

## Change it later

Open the provider again to change the allowed domains, scopes or display name.
To rotate the secret, create a new one under **Certificates & secrets** and
paste its **Value** into **Client secret**. Leave the field blank to keep the
stored secret.

See the [SSO overview](./overview.md#reference) for roles, URLs and login
enforcement.
