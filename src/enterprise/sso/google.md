# SSO — Google Workspace

_Enterprise_

Set up single sign-on with **Google Workspace** (or Google accounts). The
connection type is `google`; its OIDC issuer is fixed to
`https://accounts.google.com`, so there is **no issuer field to fill** — you only
register an OAuth client and paste its credentials.

> New to enterprise SSO? Read the [overview](./overview.md) first for the
> bootstrap flow, roles, and shared concepts. You add this connection while
> signed in as the built-in **default admin** (owner).

## What you'll need

| Item | Where it comes from |
|---|---|
| A **Google Cloud project** | [console.cloud.google.com](https://console.cloud.google.com) |
| The Versus **callback URL** | `https://<your-versus-host>/enterprise/api/sso/<org>/callback` (use `default` for a single-org deployment) |
| Your **Workspace domain(s)** | e.g. `acme.com` — the allow-list of who may sign in |

## Step 1 — Create the OAuth client in Google Cloud

1. In the Google Cloud Console, open **APIs & Services → Credentials**.
2. If prompted, configure the **OAuth consent screen** (Internal is fine for a
   Workspace-only audience).
3. Click **Create credentials → OAuth client ID**.
4. Choose **Application type: Web application** and give it a name (e.g.
   `Versus SRE Agent`).
5. Under **Authorized redirect URIs**, add the Versus callback URL exactly:

   ```text
   https://<your-versus-host>/enterprise/api/sso/<org>/callback
   ```

6. Click **Create**. Google shows your **Client ID** and **Client secret** —
   keep them for Step 2.

## Step 2 — Add the connection in Versus

In the console open the **Agent** page → **Single Sign-On (SSO)** panel → **Add
identity provider**, then fill in:

| Field | Value |
|---|---|
| **Provider** | `Google` |
| **Connection ID** | A slug such as `google` (lowercase; used in the login URL). |
| **Display name** | e.g. `Google` — the label on the sign-in button. |
| **Client ID** | The OAuth **Client ID** from Step 1. |
| **Client secret** | The OAuth **Client secret** from Step 1 (sealed on save). |
| **Redirect URL (callback)** | The same callback URL you registered in Step 1. |
| **Scopes** | `email, profile` (`openid` is always implied). |
| **Allowed email domains** | Your Workspace domain(s), e.g. `acme.com`. **Required.** |
| **Enabled** | Turn on to show the “Sign in with Google” button. |

There is **no issuer or tenant field** for Google — it is derived automatically
(`accounts.google.com`).

Click **Save**. Versus seals the secret immediately and confirms with a masked
summary (whether a secret is stored and its last four characters — never the
secret itself).

## Step 3 — Test the login

Once the connection is **enabled**, the console sign-in screen shows a **“Sign in
with Google”** button. Click it (or visit the login URL directly):

```text
https://<your-versus-host>/enterprise/api/sso/<org>/login/<connection-id>
```

You'll be redirected to Google, sign in, and land back on Versus with a session.
A user whose email domain isn't on your allow-list is refused — that's expected.

The first SSO user signs in as **viewer**. Promote one to `admin`/`owner` from
the **Members** panel, then [disable the built-in default admin](./overview.md#the-bootstrap-flow-who-creates-the-first-connection).

## Editing or rotating later

Re-open the connection any time to change the allowed domains, scopes, or display
name. To **rotate** the client secret, create a new secret in Google Cloud and
paste it into the **Client secret** field. Leave that field **blank** to keep the
stored secret unchanged.
