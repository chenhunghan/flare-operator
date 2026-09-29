#!/usr/bin/env python3
"""Sanitize spike recordings before they are committed.

Usage: hack/sanitize_recordings.py SRC_DIR DST_DIR [--subs ~/.config/flare-operator/sanitize.json]

The substitution list (account IDs, emails, subdomains, IPs, hostnames) lives OUTSIDE the repo so
personal values are never committed. On top of it the script always:
  * redacts tunnel tokens/credentials, tail/live-tail capability URLs and CF-Ray headers,
  * replaces request geolocation (cf.* fields) and client-IP headers with placeholders,
and finally scans the output and exits 1 if anything that looks sensitive remains.
"""
import argparse
import glob
import json
import os
import re
import sys

GEO = {"latitude": "0.00000", "longitude": "0.00000", "city": "Example City", "postalCode": "00000",
       "region": "Example Region", "regionCode": "XX", "metroCode": "000", "timezone": "Etc/UTC",
       "asOrganization": "Example ISP", "country": "XX", "asn": 64496, "clientTcpRtt": 10}
IP_HEADERS = {"cf-connecting-ip", "x-real-ip", "x-forwarded-for", "true-client-ip"}
SECRET_KEYS = {"token", "credentials_file", "tunnel_secret"}
USER_ID_KEYS = {"userId", "author_id", "user_id", "userTag", "user_tag"}
URL_PATTERNS = [
    (re.compile(r"wss://tail\.developers\.workers\.dev/[0-9a-f]{32}"), "wss://tail.developers.workers.dev/REDACTED"),
    (re.compile(r"(wss://live-tail\.observability\.cloudflare\.com/connect\?)[^\"\s]+"), r"\1REDACTED"),
]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("src")
    ap.add_argument("dst")
    ap.add_argument("--subs", default=os.path.expanduser("~/.config/flare-operator/sanitize.json"))
    ap.add_argument("--skip", nargs="*", default=["alerts-available", "billing-check", "subscriptions-get", "alert-policies"])
    a = ap.parse_args()
    subs = [(re.compile(p), r) for p, r in json.load(open(a.subs))["replace"]]

    def scrub_str(s):
        for p, r in subs:
            s = p.sub(r, s)
        for p, r in URL_PATTERNS:
            s = p.sub(r, s)
        if s[:1] in "{[":
            try:
                return json.dumps(walk(json.loads(s)))
            except ValueError:
                pass
        return s

    def walk(o, parent_key=None):
        if isinstance(o, dict):
            out = {}
            for k, v in o.items():
                lk = k.lower()
                if k in GEO and not isinstance(v, (dict, list)):
                    out[k] = GEO[k]
                elif lk in IP_HEADERS and isinstance(v, str):
                    out[k] = "203.0.113.10"
                elif lk == "cf-ray":
                    out[k] = "REDACTED"
                elif k in USER_ID_KEYS and isinstance(v, str) and v:
                    out[k] = "USER_ID"  # Cloudflare user tags (telemetry userId, version author_id)
                elif k in SECRET_KEYS and v not in (None, "", "REDACTED"):
                    out[k] = "REDACTED"
                else:
                    out[k] = walk(v, k)
            return out
        if isinstance(o, list):
            return [walk(x, parent_key) for x in o]
        if isinstance(o, str):
            return scrub_str(o)
        return o

    os.makedirs(a.dst, exist_ok=True)
    n = 0
    for f in sorted(glob.glob(os.path.join(a.src, "*.json"))):
        base = os.path.basename(f)
        if any(s in base for s in a.skip):
            continue
        rec = walk(json.load(open(f)))
        # GET /cfd_tunnel/{id}/token returns the bare token string as result.
        if rec.get("path", "").endswith("/token") and isinstance(rec.get("response_body"), dict):
            rec["response_body"]["result"] = "REDACTED"
        json.dump(rec, open(os.path.join(a.dst, base), "w"), indent=1)
        n += 1

    leaks = []
    raw_needles = [p.pattern for p, _ in subs]
    for f in glob.glob(os.path.join(a.dst, "*.json")):
        t = open(f).read()
        for needle in raw_needles:
            if re.search(needle, t):
                leaks.append((os.path.basename(f), "substitution:" + needle[:6] + "…"))
        for pat in (r"eyJ[A-Za-z0-9_-]{40,}", r"tail\.developers\.workers\.dev/[0-9a-f]{32}", r"connect\?(?!REDACTED)"):
            if re.search(pat, t):
                leaks.append((os.path.basename(f), pat))
    print(f"sanitized {n} recordings into {a.dst}")
    if leaks:
        print("POSSIBLE LEAKS:", leaks, file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
