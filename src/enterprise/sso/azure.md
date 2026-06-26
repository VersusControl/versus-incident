# SSO — Microsoft Entra ID (Azure AD)

_Enterprise_

Set up single sign-on with **Microsoft Entra ID** (formerly Azure Active
Directory). The connection type is `azure`; its OIDC issuer is derived from your
**directory (tenant) ID**:

```text
https://login.microsoftonline.com/<tenant>/v2.0
```

Leave the tenant **blank** to use the multi-tenant `common` authority. For a
single-organization sign-in, supply your directory (tenant) ID.

> New to enterprise SSO? Read the [overview](./overview.md) first for the
> bootstrap flow, roles, and shared concepts. You add this connection while
> signed in as the built-in **default admin** (owner).

## What you'll need

| Item | Where it comes from |
|---|---|
| Access to **Microsoft Entra admin center** | [entra.microsoft.com](https://entra.microsoft.com) (or the Azure portal) |
| The Versus **callback URL** | `https://<your-versus-host>/enterprise/api/sso/<org>/callback` (use `default` for a single-org deployment) |
| Your **email domain(s)** | e.g. `acme.com` — the allow-list of who may sign in |

## Step 1 — Register the application in Entra ID

1. In the Entra admin center, open **Identity → Applications → App registrations**
   and click **New registration**.
2. Give it a name (e.g. `Versus SRE Agent`) and choose the **supported account
   types** appropriate for your tenant.
3. Under **Redirect URI**, choose platform **Web** and enter the Versus callback
   URL exactly:

   ```text
   https://<your-versus-host>/enterprise/api/sso/<org>/callback
   ```

4. Click **Register**. On the app **Overview**, copy the **Application (client)
   ID** and the **Directory (tenant) ID**.
5. Open **Certificates & secrets → Client secrets → New client secret**. Copy the
   secret **Value** (not the secret ID) immediately — it is shown only once.

## Step 2 — Add the connection in Versus

In the console open the **Agent** page → **Single Sign-On (SSO)** panel → **Add
identity provider**, then fill in:

| Field | Value |
|---|---|
| **Provider** | `Microsoft Entra (Azure AD)` |
| **Connection ID** | A slug such as `entra` (lowercase; used in the login URL). |
| **Display name** | e.g. `Microsoft` — the label on the sign-in button. |
| **Directory (tenant) ID** | The **Directory (tenant) ID** from Step 1. Blank ⇒ the multi-tenant `common` authority. |
| **Client ID** | The **Application (client) ID** from Step 1. |
| **Client secret** | The client secret **Value** from Step 1 (sealed on save). |
| **Redirect URL (callback)** | The same callback URL you registered in Step 1. |
| **Scopes** | `email, profile` (`openid` is always implied). |
| **Allowed email domains** | Your email domain(s), e.g. `acme.com`. **Required.** |
| **Enabled** | Turn on to show the “Sign in with Microsoft” button. |

The issuer is derived from the tenant — there is no issuer field for Entra.

Click **Save**. Versus seals the secret immediately and confirms with a masked
summary (whether a secret is stored and its last four characters — never the
secret itself).

## Step 3 — Test the login

Once the connection is **enabled**, the console sign-in screen shows a **“Sign in
with Microsoft”** button. Click it (or visit the login URL directly):

```text
https://<your-versus-host>/enterprise/api/sso/<org>/login/<connection-id>
```

You'll be redirected to Microsoft, sign in, and land back on Versus with a
session. A user whose email domain isn't on your allow-list is refused — that's
expected.

The first SSO user signs in as **viewer**. Promote one to `admin`/`owner` from
the **Members** panel, then [disable the built-in default admin](./overview.md#the-bootstrap-flow-who-creates-the-first-connection).

## Editing or rotating later

Re-open the connection any time to change the allowed domains, scopes, or display
name. To **rotate** the client secret, create a new secret under **Certificates &
secrets** in Entra and paste its **Value** into the **Client secret** field. Leave
that field **blank** to keep the stored secret unchanged.
