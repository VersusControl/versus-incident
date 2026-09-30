#!/usr/bin/env python3
"""Create SigNoz's first admin account and mint the query API key Versus uses.

SigNoz GENERATES the API key value server-side, so unlike every other credential
in this example it cannot be a `${VAR:-default}` in the compose file. This
one-shot therefore does what a human would otherwise click through in the UI —
register the first user, create a service account, grant it the viewer role,
and mint a key — then writes the key to a shared volume that the versus
container reads at startup.

Set SIGNOZ_API_KEY in the environment to skip minting entirely and use your own.

The endpoints below were read off a running SigNoz v0.129.0. They are NOT a
stable public contract: SigNoz moved auth to `/api/v2/sessions` and replaced
personal access tokens with service accounts, so a different SigNoz version may
need this script adjusted.

Python 3 stdlib only.
"""

from __future__ import annotations

import json
import os
import sys
import tempfile
import time
import urllib.error
import urllib.request

ADDRESS = os.environ.get("SIGNOZ_ADDRESS", "http://signoz:8080").rstrip("/")
EMAIL = os.environ.get("SIGNOZ_ADMIN_EMAIL", "admin@versus.local")
PASSWORD = os.environ.get("SIGNOZ_ADMIN_PASSWORD", "Versus-Dev-12345")
ORG_NAME = os.environ.get("SIGNOZ_ORG_NAME", "versus-demo")
KEY_FILE = os.environ.get("SIGNOZ_API_KEY_FILE", "/bootstrap/signoz-api-key")
# SigNoz validates this: lowercase letters, digits and hyphens only.
ACCOUNT_NAME = os.environ.get("SIGNOZ_SERVICE_ACCOUNT", "versus-agent")
# Versus only ever READS from SigNoz, so the key gets the viewer role.
ROLE_NAME = os.environ.get("SIGNOZ_ROLE", "signoz-viewer")


def reset_instruction() -> str:
    command = os.environ.get("SIGNOZ_RESET_COMMAND", "").strip()
    if command:
        return "reset the SigNoz installation with `%s` and start again." % command
    return "reset the SigNoz installation and start again."


def call(path, body=None, token=None, method=None, api_key=None):
    """One JSON request. Returns (status, decoded-or-raw-text)."""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        ADDRESS + path,
        data=data,
        method=method or ("POST" if data is not None else "GET"),
        headers={"Content-Type": "application/json"},
    )
    if token:
        req.add_header("Authorization", "Bearer " + token)
    if api_key:
        req.add_header("SIGNOZ-API-KEY", api_key)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read().decode("utf-8", "replace")
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        status = e.code
    except (urllib.error.URLError, TimeoutError) as e:
        return 0, str(e)
    try:
        return status, json.loads(raw)
    except ValueError:
        return status, raw


def wait_ready(deadline_s: float = 300.0) -> dict:
    """Block until SigNoz answers /api/v1/version, then return that payload."""
    end = time.time() + deadline_s
    while time.time() < end:
        status, body = call("/api/v1/version")
        if status == 200 and isinstance(body, dict):
            return body
        time.sleep(3)
    sys.exit("signoz did not answer /api/v1/version within %.0fs" % deadline_s)


def register() -> str:
    """Create the first user and return its org id.

    SigNoz allows this exactly once: a second call answers
    "self-registration is disabled".
    """
    status, body = call(
        "/api/v1/register",
        {"name": "versus", "orgName": ORG_NAME, "email": EMAIL, "password": PASSWORD},
    )
    if status < 300 and isinstance(body, dict):
        org = (body.get("data") or {}).get("orgId")
        if org:
            print("registered first admin user %s (org %s)" % (EMAIL, org))
            return org
    sys.exit(
        "could not register the first SigNoz user (HTTP %d); %s"
        % (status, reset_instruction())
    )


def login(org: str) -> str:
    """Exchange the admin credentials for a short-lived access token."""
    for attempt in range(10):
        status, body = call(
            "/api/v2/sessions/email_password",
            {"orgID": org, "email": EMAIL, "password": PASSWORD},
        )
        if status < 300 and isinstance(body, dict):
            token = (body.get("data") or {}).get("accessToken")
            if token:
                return token
        print("login attempt %d returned HTTP %d" % (attempt + 1, status))
        time.sleep(3)
    sys.exit("could not log in to SigNoz as %s" % EMAIL)


def service_account(token: str) -> str:
    """Return the id of our service account, creating it when absent."""
    status, body = call("/api/v1/service_accounts", token=token)
    if status < 300 and isinstance(body, dict):
        for item in body.get("data") or []:
            if isinstance(item, dict) and item.get("name") == ACCOUNT_NAME:
                return item["id"]
    status, body = call("/api/v1/service_accounts", {"name": ACCOUNT_NAME}, token=token)
    if status < 300 and isinstance(body, dict):
        sid = (body.get("data") or {}).get("id")
        if sid:
            return sid
    sys.exit("could not create the SigNoz service account (HTTP %d)" % status)


def grant_role(token: str, sid: str) -> None:
    """Attach the viewer role.

    Without a role the key authenticates but every query comes back
    `authz_forbidden` — which reads like a bad key and is not.
    """
    status, body = call("/api/v1/roles", token=token)
    role_id = None
    if status < 300 and isinstance(body, dict):
        for role in body.get("data") or []:
            if isinstance(role, dict) and role.get("name") == ROLE_NAME:
                role_id = role["id"]
                break
    if not role_id:
        sys.exit("SigNoz has no role named %r (HTTP %d)" % (ROLE_NAME, status))
    status, body = call(
        "/api/v1/service_accounts/%s/roles" % sid, {"id": role_id}, token=token
    )
    if status >= 300:
        sys.exit("could not grant %s to the service account (HTTP %d)"
                 % (ROLE_NAME, status))
    print("granted %s to service account %s" % (ROLE_NAME, ACCOUNT_NAME))


def create_key(token: str, sid: str) -> str:
    """Mint a non-expiring key. SigNoz returns the value ONCE, here."""
    status, body = call(
        "/api/v1/service_accounts/%s/keys" % sid,
        {"name": ACCOUNT_NAME + "-key", "expiresAt": 0},
        token=token,
    )
    if status < 300 and isinstance(body, dict):
        key = (body.get("data") or {}).get("key")
        if key:
            return key
    sys.exit("could not create a SigNoz API key (HTTP %d)" % status)


def main() -> int:
    supplied = os.environ.get("SIGNOZ_API_KEY", "").strip()
    if supplied:
        print("SIGNOZ_API_KEY supplied by the operator; not minting one.")
        return 0

    version = wait_ready()
    print("signoz %s is up (setupCompleted=%s)"
          % (version.get("version"), version.get("setupCompleted")))

    if os.path.exists(KEY_FILE) and os.path.getsize(KEY_FILE) > 0:
        with open(KEY_FILE, encoding="utf-8") as fp:
            saved_key = fp.read().strip()
        if saved_key:
            status, body = call("/api/v1/service_accounts/me", api_key=saved_key)
            if status == 200 and isinstance(body, dict):
                print("saved SigNoz API key is valid; nothing to do.")
                return 0
            if status not in (401, 403):
                print("could not verify saved SigNoz API key (HTTP %d); retry/check SigNoz health or API compatibility; preserve state." % status)
                return 1
        print("saved SigNoz API key is invalid; " + reset_instruction())
        return 1

    if version.get("setupCompleted") is not False:
        print("SigNoz is already initialized but has no saved API key; " + reset_instruction())
        return 1

    org = register()
    token = login(org)
    sid = service_account(token)
    grant_role(token, sid)
    key = create_key(token, sid)

    os.makedirs(os.path.dirname(KEY_FILE), exist_ok=True)
    key_directory = os.path.dirname(KEY_FILE)
    previous = os.stat(KEY_FILE, follow_symlinks=False) if os.path.lexists(KEY_FILE) else None
    fd, temporary = tempfile.mkstemp(dir=key_directory)
    try:
        if previous is not None:
            current = os.fstat(fd)
            if (current.st_uid, current.st_gid) != (previous.st_uid, previous.st_gid):
                os.fchown(fd, previous.st_uid, previous.st_gid)
        with os.fdopen(fd, "w", encoding="utf-8") as fp:
            fd = -1
            fp.write(key)
            fp.flush()
        os.replace(temporary, KEY_FILE)
    finally:
        if fd >= 0:
            os.close(fd)
        if os.path.exists(temporary):
            os.unlink(temporary)
    # Never print the key itself — this log is what an operator pastes into a
    # bug report.
    print("wrote SigNoz API key (%d chars) to %s" % (len(key), KEY_FILE))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
