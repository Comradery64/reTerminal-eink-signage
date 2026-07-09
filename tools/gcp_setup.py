#!/usr/bin/env python3
"""Verify keyless (WIF) free/busy access to a room calendar.

Project/SA/API creation is handled by gcp-seeder; Workload Identity Federation wiring and room
sharing are gcloud/admin steps (see docs/DEPLOY.md Parts A & D). This tool just proves the end
result: that the service account can read a room's free/busy via the external-account credentials —
NO key, NO impersonation.

Because it uses the external-account cred config (which reads a *projected k8s token*), run it where
that token exists — i.e. in-cluster:

  kubectl -n meeting-displays exec deploy/broker -- /verify ...        # if bundled, or:
  kubectl -n meeting-displays run verify --rm -it --restart=Never \\
    --image=<broker-image> --overrides='{...mount cred-config+token...}' -- ...

Locally it only works if GOOGLE creds resolve (e.g. you exported a token file the cred-config points at).

Usage:
  pip install -r tools/requirements.txt
  python tools/gcp_setup.py verify --cred-config /etc/gcp/cred-config.json --room aspen@example.com
"""
import argparse, sys, time

try:
    from google.auth import load_credentials_from_file
    import google.auth.transport.requests as greq
    _HAVE_DEPS = True
except ImportError:
    _HAVE_DEPS = False

FREEBUSY_SCOPE = "https://www.googleapis.com/auth/calendar.freebusy"
FREEBUSY_URL   = "https://www.googleapis.com/calendar/v3/freeBusy"


def cmd_verify(args):
    creds, _ = load_credentials_from_file(args.cred_config, scopes=[FREEBUSY_SCOPE])
    sess = greq.AuthorizedSession(creds)
    now = time.gmtime()
    body = {
        "timeMin": time.strftime("%Y-%m-%dT%H:%M:%SZ", now),
        "timeMax": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.mktime(now) + 86400)),
        "items": [{"id": args.room}],
    }
    r = sess.post(FREEBUSY_URL, json=body, timeout=20)
    if r.status_code != 200:
        sys.exit(f"\nFAILED (HTTP {r.status_code}): {r.text}\n"
                 "Common causes: WIF pool/provider/binding wrong (Part D), or the SA lacks "
                 "freeBusyReader on this room (Part A).")
    cal = r.json().get("calendars", {}).get(args.room, {})
    if cal.get("errors"):
        sys.exit(f"\nFAILED: calendar error for {args.room}: {cal['errors']}\n"
                 "→ the service account hasn't been granted freeBusyReader on this room yet.")
    busy = cal.get("busy", [])
    print(f"OK — keyless free/busy works. {args.room} has {len(busy)} busy block(s) in the next 24h:")
    for b in busy:
        print(f"  {b['start']}  →  {b['end']}")
    print("\nNo key, no impersonation, freebusy scope only. Ready to deploy.")


def main():
    ap = argparse.ArgumentParser(description="Verify keyless WIF free/busy access to a room.")
    sub = ap.add_subparsers(dest="cmd", required=True)
    v = sub.add_parser("verify", help="read a room's free/busy via the external-account cred config")
    v.add_argument("--cred-config", default="/etc/gcp/cred-config.json",
                   help="external-account (WIF) credential config JSON")
    v.add_argument("--room", required=True, help="room calendar id / email to read")
    v.set_defaults(func=cmd_verify)

    args = ap.parse_args()
    if not _HAVE_DEPS:
        sys.exit("Missing deps. Run: pip install -r tools/requirements.txt")
    args.func(args)


if __name__ == "__main__":
    main()
