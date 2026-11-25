#!/usr/bin/env python3
"""Register oakestra-addon.json at a marketplace and install it, then wait until it is active.

Does what a real Oakestra install does (marketplace-manager, then addons-manager), so the
dashboard dev stack's mock addons manager sees the real manifest, ui block included. Stdlib only.

    MARKETPLACE_URL=http://localhost:11102 ADDONS_MANAGER_URL=http://localhost:11101 \
        deploy/register-addon.py oakestra-addon.json

The mock serves both APIs on one port (11101). Running it again is safe: an approved definition
with the same name and an existing installation are reused.
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

MARKETPLACE = os.environ.get("MARKETPLACE_URL", "http://localhost:11102").rstrip("/")
MANAGER = os.environ.get("ADDONS_MANAGER_URL", "http://localhost:11101").rstrip("/")
TIMEOUT = float(os.environ.get("REGISTER_TIMEOUT_SECONDS", "180"))


def call(method, url, body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            raw = resp.read()
            return resp.status, json.loads(raw) if raw else None
    except urllib.error.HTTPError as err:
        raw = err.read()
        try:
            return err.code, json.loads(raw)
        except ValueError:
            return err.code, raw.decode(errors="replace")


def wait(what, check):
    # Both managers move addons through intermediate states (under_review, installing), and the
    # stack may still be starting, so every step polls instead of failing on the first answer.
    deadline = time.monotonic() + TIMEOUT
    last = None
    while time.monotonic() < deadline:
        try:
            done, last = check()
        except (urllib.error.URLError, OSError) as err:
            done, last = False, f"unreachable: {err}"
        if done:
            return last
        time.sleep(2)
    sys.exit(f"register-addon: timed out waiting for {what} (last: {last})")


def definition_id(name, manifest):
    def listed():
        status, body = call("GET", f"{MARKETPLACE}/api/v1/marketplace/addons")
        if status != 200:
            return False, f"list marketplace: {status} {body}"
        return True, next((d["_id"] for d in body if d.get("name") == name), None)

    existing = wait("the marketplace", listed)
    if existing:
        print(f"register-addon: {name} already in the marketplace ({existing})")
        return existing

    status, body = call("POST", f"{MARKETPLACE}/api/v1/marketplace/addons", manifest)
    if status not in (200, 201):
        sys.exit(f"register-addon: marketplace rejected {name}: {status} {json.dumps(body)}")
    new_id = body["_id"]
    print(f"register-addon: registered {name} ({new_id}), waiting for review")

    def reviewed():
        status, body = call("GET", f"{MARKETPLACE}/api/v1/marketplace/addons/{new_id}")
        state = body.get("status") if isinstance(body, dict) else status
        if state == "verification_failed":
            sys.exit(f"register-addon: verification failed: {json.dumps(body)}")
        return state == "approved", state

    wait("approval", reviewed)
    return new_id


def install(name, marketplace_id):
    status, body = call("GET", f"{MANAGER}/api/v1/addons")
    if status != 200:
        sys.exit(f"register-addon: list installed addons: {status} {body}")
    installed = next((a for a in body if a.get("marketplace_id") == marketplace_id or a.get("name") == name), None)
    if installed is None:
        status, body = call("POST", f"{MANAGER}/api/v1/addons", {"marketplace_id": marketplace_id})
        if status not in (200, 201):
            sys.exit(f"register-addon: install failed: {status} {json.dumps(body)}")
        installed = body
        print(f"register-addon: installing {name} ({installed['_id']})")
    elif installed.get("status") in ("disabled", "failed"):
        sys.exit(
            f"register-addon: {name} is installed but {installed['status']}; "
            "enable or delete it in the dashboard's addon settings, then run this again"
        )

    def active():
        status, body = call("GET", f"{MANAGER}/api/v1/addons/{installed['_id']}")
        state = body.get("status") if isinstance(body, dict) else status
        if state == "failed":
            sys.exit(f"register-addon: install failed: {json.dumps(body)}")
        return state == "active", state

    wait("the addon to become active", active)
    print(f"register-addon: {name} is active")


def main():
    parser = argparse.ArgumentParser(description="Register a manifest at the marketplace and install it.")
    parser.add_argument("manifest", nargs="?", default="oakestra-addon.json", help="path of the addon manifest")
    path = parser.parse_args().manifest
    with open(path, encoding="utf-8") as fh:
        manifest = json.load(fh)
    name = manifest["name"]
    install(name, definition_id(name, manifest))


if __name__ == "__main__":
    main()
