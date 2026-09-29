#!/usr/bin/env python3
"""Classify every Cloudflare OpenAPI path group into a product category and operator tier.

Usage: hack/classify_api.py path/to/openapi.json > docs/cloudflare-api-coverage.md
Fails (exit 1) if the spec contains a group that is not classified, so re-running it
against a newer spec shows exactly which new API surfaces need a decision.
"""
import collections
import json
import sys

# tier: T1 = hand-written core CRD, T2 = generated CRD (declarative long tail),
#       T3 = imperative/one-shot (Job-style CRD or not at all),
#       RO = read-only/observability, X = out of scope (billing, internal, deprecated, org/user admin)
C = {
    # --- Compute ---------------------------------------------------------------
    "accounts/workers": ("Compute", "T1", "Workers scripts, versions, deployments, routes, domains, DO namespaces, WfP dispatch, observability"),
    "zones/workers": ("Compute", "T1", "Worker routes (zone-scoped)"),
    "accounts/triggers": ("Compute", "T1", "Worker triggers (cron/queue/etc.) per script"),
    "accounts/containers": ("Compute", "T1", "Container applications, rollouts, versions, registries"),
    "accounts/workflows": ("Compute", "T1", "Workflows + instances"),
    "accounts/builds": ("Compute", "T2", "Workers Builds (CI triggers, deploy hooks, previews)"),
    "workers": ("Compute", "T3", "Trigger Workers Builds deploy hook"),
    "accounts/pages": ("Compute", "T2", "Pages projects/deployments (maintenance mode; prefer Workers)"),
    "pages": ("Compute", "T3", "Pages asset upload helpers"),
    "accounts/browser-rendering": ("Compute", "T3", "Browser Run (imperative actions/sessions)"),
    "accounts/flagship": ("Compute", "T2", "Flagship feature-flag apps + flags"),
    # --- Storage / data --------------------------------------------------------
    "accounts/r2": ("Storage", "T1", "R2 buckets, CORS, lifecycle, lock, domains, sippy"),
    "accounts/event_notifications": ("Storage", "T1", "R2 bucket event notifications -> Queue"),
    "accounts/storage": ("Storage", "T1", "Workers KV namespaces (+ key/value data plane)"),
    "accounts/d1": ("Storage", "T1", "D1 databases (+ query/import/export/time travel)"),
    "accounts/queues": ("Storage", "T1", "Queues + consumers (+ message data plane)"),
    "accounts/hyperdrive": ("Storage", "T1", "Hyperdrive configs"),
    "accounts/secrets_store": ("Storage", "T1", "Secrets Store stores + secrets"),
    "accounts/vectorize": ("Storage", "T2", "Vectorize indexes"),
    "accounts/pipelines": ("Storage", "T2", "Pipelines streams/sinks/pipelines"),
    "accounts/basin-catalog": ("Storage", "T2", "R2 Data Catalog (Iceberg)"),
    "accounts/r2-catalog": ("Storage", "X", "R2 Data Catalog legacy path (deprecated)"),
    "accounts/slurper": ("Storage", "T3", "Super Slurper migration jobs"),
    "accounts/artifacts": ("Storage", "T2", "Artifacts namespaces/repos (git-compatible FS)"),
    "accounts/agent-memory": ("Storage", "T2", "Agent Memory namespaces"),
    "accounts/analytics_engine": ("Storage", "RO", "Analytics Engine SQL (datasets implicit)"),
    "accounts/event_subscriptions": ("Storage", "T2", "Event subscriptions (platform events -> Queue)"),
    # --- AI / media ------------------------------------------------------------
    "accounts/ai": ("AI & Media", "T3", "Workers AI run/finetunes/models"),
    "accounts/ai-gateway": ("AI & Media", "T2", "AI Gateway gateways, provider configs, routes"),
    "accounts/ai-search": ("AI & Media", "T2", "AI Search namespaces/instances"),
    "accounts/autorag": ("AI & Media", "X", "AutoRAG legacy (renamed AI Search)"),
    "accounts/images": ("AI & Media", "T2", "Images, variants, signing keys"),
    "zones/images": ("AI & Media", "T2", "Image transformation zone settings"),
    "accounts/stream": ("AI & Media", "T2", "Stream videos, live inputs, watermarks, webhooks"),
    "accounts/calls": ("AI & Media", "T2", "Realtime SFU apps + TURN keys"),
    "accounts/realtime": ("AI & Media", "T2", "RealtimeKit apps/presets/webhooks"),
    "accounts/moq": ("AI & Media", "T2", "MoQ relays"),
    "accounts/settings": ("AI & Media", "T2", "Account image transformation / billing settings"),
    # --- DNS / zones / domains -------------------------------------------------
    "zones": ("DNS & Zones", "T1", "Zones (create/adopt), zone holds"),
    "zones/dns_records": ("DNS & Zones", "T1", "DNS records (+ batch/import/export)"),
    "zones/dns_settings": ("DNS & Zones", "T2", "Zone DNS settings"),
    "accounts/dns_settings": ("DNS & Zones", "T2", "Account DNS settings, internal DNS views, NS sets"),
    "zones/dnssec": ("DNS & Zones", "T2", "DNSSEC"),
    "zones/secondary_dns": ("DNS & Zones", "T2", "Secondary DNS incoming/outgoing"),
    "accounts/secondary_dns": ("DNS & Zones", "T2", "Secondary DNS peers/TSIG/ACLs"),
    "accounts/dns_firewall": ("DNS & Zones", "T2", "DNS Firewall clusters"),
    "accounts/custom_ns": ("DNS & Zones", "T2", "Account custom nameservers"),
    "zones/custom_ns": ("DNS & Zones", "T2", "Zone custom nameservers"),
    "accounts/registrar": ("DNS & Zones", "T3", "Registrar (domain purchase costs money)"),
    "accounts/registrar-sandbox": ("DNS & Zones", "X", "Registrar sandbox"),
    "zones/hold": ("DNS & Zones", "T2", "Zone hold"),
    "accounts/dns_records": ("DNS & Zones", "RO", "DNS record usage"),
    "zones/dns_analytics": ("DNS & Zones", "RO", "DNS analytics"),
    "zones/activation_check": ("DNS & Zones", "T3", "Re-run zone activation check"),
    "zones/web3": ("DNS & Zones", "T2", "Web3 gateways (IPFS/Ethereum hostnames)"),
    # --- TLS / SaaS ------------------------------------------------------------
    "zones/ssl": ("TLS", "T2", "Edge certificate packs, universal SSL, verification"),
    "zones/acm": ("TLS", "T2", "Advanced Certificate Manager, Total TLS, custom trust store"),
    "zones/custom_certificates": ("TLS", "T2", "Uploaded custom certificates"),
    "zones/custom_csrs": ("TLS", "T2", "Custom CSRs (zone)"),
    "accounts/custom_csrs": ("TLS", "T2", "Custom CSRs (account)"),
    "certificates": ("TLS", "T2", "Origin CA certificates"),
    "zones/client_certificates": ("TLS", "T2", "API Shield / mTLS client certificates"),
    "zones/origin_tls_client_auth": ("TLS", "T2", "Authenticated Origin Pulls"),
    "accounts/mtls_certificates": ("TLS", "T2", "Account mTLS certificates"),
    "zones/keyless_certificates": ("TLS", "T2", "Keyless SSL"),
    "zones/hostnames": ("TLS", "T2", "Per-hostname TLS settings"),
    "zones/certificate_authorities": ("TLS", "T2", "Hostname-associated client CAs"),
    "zones/custom_hostnames": ("TLS", "T1", "Custom Hostnames (Cloudflare for SaaS)"),
    "zones/dcv_delegation": ("TLS", "RO", "DCV delegation UUID"),
    "zones/ct": ("TLS", "T2", "Certificate Transparency monitoring"),
    # --- Traffic / performance -------------------------------------------------
    "zones/load_balancers": ("Traffic", "T1", "Load balancers"),
    "accounts/load_balancers": ("Traffic", "T1", "LB pools, monitors, monitor groups"),
    "zones/healthchecks": ("Traffic", "T2", "Standalone health checks"),
    "zones/spectrum": ("Traffic", "T2", "Spectrum L4 apps"),
    "zones/argo": ("Traffic", "T2", "Argo Smart Routing / tiered caching"),
    "zones/smart_shield": ("Traffic", "T2", "Smart Shield"),
    "zones/cache": ("Traffic", "T2", "Cache settings, Cache Reserve, tiered cache"),
    "zones/purge_cache": ("Traffic", "T3", "Cache purge"),
    "zones/invalidate_cache": ("Traffic", "T3", "Cache invalidation"),
    "zones/waiting_rooms": ("Traffic", "T2", "Waiting rooms"),
    "accounts/waiting_rooms": ("Traffic", "RO", "Waiting rooms (account list)"),
    "zones/origin": ("Traffic", "T2", "Origin cloud-region mappings"),
    "zones/settings": ("Traffic", "T2", "Zone settings (per-setting endpoints)"),
    "zones/url_normalization": ("Traffic", "T2", "URL normalization"),
    "zones/managed_headers": ("Traffic", "T2", "Managed transforms"),
    "zones/speed_api": ("Traffic", "RO", "Observatory / speed tests"),
    "accounts/rum": ("Traffic", "T2", "Web Analytics sites"),
    "zones/environments": ("Traffic", "X", "Version Management (incompatible with IaC)"),
    "zones/observability": ("Traffic", "T2", "Zone tracing rules/settings"),
    "zones/precursor": ("Traffic", "T2", "Zone precursor config"),
    # --- Rules / app security --------------------------------------------------
    "zones/rulesets": ("Rules & Security", "T1", "Zone rulesets (transform/redirect/origin/cache/WAF/rate-limit phases)"),
    "accounts/rulesets": ("Rules & Security", "T2", "Account rulesets (WAF, Network Firewall, DDoS overrides)"),
    "accounts/rules": ("Rules & Security", "T2", "Lists (IP/ASN/hostname/redirect)"),
    "zones/snippets": ("Rules & Security", "T2", "Snippets + snippet rules"),
    "zones/cloud_connector": ("Rules & Security", "T2", "Cloud Connector rules"),
    "zones/pagerules": ("Rules & Security", "X", "Page Rules (deprecated)"),
    "zones/filters": ("Rules & Security", "X", "Firewall filters (deprecated)"),
    "zones/rate_limits": ("Rules & Security", "X", "Legacy rate limits (deprecated)"),
    "zones/rate_limit_analytics": ("Rules & Security", "RO", "Rate limit analytics"),
    "zones/firewall": ("Rules & Security", "T2", "IP access rules, UA blocks, lockdowns (legacy rules deprecated)"),
    "accounts/firewall": ("Rules & Security", "T2", "Account IP access rules"),
    "zones/leaked-credential-checks": ("Rules & Security", "T2", "Leaked credential checks"),
    "zones/content-upload-scan": ("Rules & Security", "T2", "Malicious upload scanning"),
    "zones/ai-security": ("Rules & Security", "T2", "AI security for apps (firewall for AI)"),
    "zones/fraud_detection": ("Rules & Security", "T2", "Fraud detection settings"),
    "zones/bot_management": ("Rules & Security", "T2", "Bot management"),
    "zones/ai-audit": ("Rules & Security", "T2", "AI Crawl Control"),
    "zones/pay-per-crawl": ("Rules & Security", "T2", "Pay per crawl (zone)"),
    "accounts/pay-per-crawl": ("Rules & Security", "T2", "Pay per crawl (account)"),
    "accounts/pay-per-use": ("Rules & Security", "X", "Pay per use marketplace"),
    "zones/pay-per-use": ("Rules & Security", "X", "Pay per use marketplace (zone)"),
    "zones/api_gateway": ("Rules & Security", "T2", "API Shield operations/discovery/settings"),
    "zones/schema_validation": ("Rules & Security", "T2", "API schema validation"),
    "zones/token_validation": ("Rules & Security", "T2", "JWT/token validation"),
    "zones/page_shield": ("Rules & Security", "T2", "Client-side security (Page Shield)"),
    "accounts/challenges": ("Rules & Security", "T2", "Turnstile widgets"),
    "zones/custom_pages": ("Rules & Security", "T2", "Custom error/challenge pages"),
    "accounts/custom_pages": ("Rules & Security", "T2", "Account custom pages"),
    "zones/security-center": ("Rules & Security", "RO", "Security Center insights"),
    "accounts/security-center": ("Rules & Security", "RO", "Security Center insights"),
    "accounts/field_extractors": ("Rules & Security", "T2", "Field extractors"),
    # --- Tunnel / private networking ------------------------------------------
    "accounts/cfd_tunnel": ("Tunnel & Private Net", "T1", "Cloudflare Tunnels + remote config + token"),
    "accounts/tunnels": ("Tunnel & Private Net", "RO", "List all tunnel types"),
    "accounts/teamnet": ("Tunnel & Private Net", "T1", "Private network routes + virtual networks"),
    "accounts/zerotrust": ("Tunnel & Private Net", "T2", "Hostname routes, subnets, connectivity settings"),
    "accounts/connectivity": ("Tunnel & Private Net", "T1", "Workers VPC connectivity services"),
    "accounts/warp_connector": ("Tunnel & Private Net", "T2", "Cloudflare Mesh (ex-WARP Connector)"),
    "accounts/infrastructure": ("Tunnel & Private Net", "T2", "Access for Infrastructure targets"),
    # --- Zero Trust ------------------------------------------------------------
    "accounts/access": ("Zero Trust", "T1", "Access apps, policies, groups, service tokens, IdPs, MCP portals"),
    "zones/access": ("Zero Trust", "T2", "Zone-scoped Access (prefer account-scoped)"),
    "accounts/gateway": ("Zero Trust", "T2", "Gateway DNS/HTTP/network policies, lists, locations"),
    "accounts/devices": ("Zero Trust", "T2", "Device profiles, posture, networks"),
    "zones/devices": ("Zero Trust", "T2", "Device cert provisioning (zone)"),
    "accounts/dlp": ("Zero Trust", "T2", "DLP profiles, entries, datasets"),
    "accounts/data-security": ("Zero Trust", "T2", "Data security posture policies/webhooks (findings are RO)"),
    "accounts/one": ("Zero Trust", "T2", "CASB integrations / app catalog"),
    "accounts/resource-library": ("Zero Trust", "T2", "Resource library applications"),
    "accounts/browser-extension": ("Zero Trust", "T2", "Browser extension config"),
    "accounts/dex": ("Zero Trust", "T2", "Digital Experience Monitoring tests"),
    "accounts/zt_risk_scoring": ("Zero Trust", "T2", "Risk scoring behaviours/integrations"),
    "accounts/email-security": ("Zero Trust", "T3", "Email security (mostly operational)"),
    "accounts/scim": ("Zero Trust", "X", "SCIM provisioning endpoint (IdP-driven)"),
    "accounts/sso_connectors": ("Zero Trust", "T2", "Dashboard SSO connectors"),
    "accounts/dls": ("Zero Trust", "T2", "Data Localization Suite"),
    # --- Email -----------------------------------------------------------------
    "zones/email": ("Email", "T2", "Email Routing rules + Email Sending subdomains"),
    "accounts/email": ("Email", "T2", "Email Routing destination addresses, sending"),
    # --- Network services (L3) -------------------------------------------------
    "accounts/magic": ("Network Services", "T3", "Magic Transit / Cloudflare WAN / Network Firewall / MCN (L3, guarded)"),
    "accounts/mnm": ("Network Services", "T2", "Network Flow"),
    "accounts/cni": ("Network Services", "T3", "Network Interconnect (physical)"),
    "accounts/addressing": ("Network Services", "T3", "BYOIP prefixes, address maps (BGP events)"),
    "zones/addressing": ("Network Services", "T2", "Regional hostnames"),
    "accounts/pcaps": ("Network Services", "T3", "Packet captures"),
    # --- Platform / account / IAM ---------------------------------------------
    "accounts/tokens": ("Platform & IAM", "T1", "Account-owned API tokens (operator credential minting)"),
    "accounts/tags": ("Platform & IAM", "T1", "Resource tagging (ownership labels)"),
    "zones/tags": ("Platform & IAM", "T1", "Zone resource tags"),
    "accounts/iam": ("Platform & IAM", "T2", "Permission groups, resource groups, user groups"),
    "accounts/members": ("Platform & IAM", "T2", "Account members"),
    "accounts/roles": ("Platform & IAM", "X", "Legacy roles (deprecated)"),
    "accounts/shares": ("Platform & IAM", "T2", "Resource sharing"),
    "accounts/oauth_clients": ("Platform & IAM", "T2", "OAuth clients"),
    "accounts/alerting": ("Platform & IAM", "T2", "Notification policies + destinations"),
    "accounts/logpush": ("Platform & IAM", "T2", "Logpush jobs (account)"),
    "zones/logpush": ("Platform & IAM", "T2", "Logpush jobs (zone)"),
    "accounts/logs": ("Platform & IAM", "RO", "Audit logs v2, Log Explorer, retention"),
    "zones/logs": ("Platform & IAM", "RO", "Logpull / received logs"),
    "accounts/audit_logs": ("Platform & IAM", "RO", "Audit logs v1"),
    "accounts": ("Platform & IAM", "X", "Account CRUD (tenant/org admin)"),
    "accounts/move": ("Platform & IAM", "X", "Move account to organization"),
    "accounts/organizations": ("Platform & IAM", "X", "Account organization link"),
    "accounts/profile": ("Platform & IAM", "X", "Account profile"),
    "organizations": ("Platform & IAM", "X", "Organizations"),
    "organizations/accounts": ("Platform & IAM", "X", "Organizations"),
    "organizations/billable": ("Platform & IAM", "X", "Organizations"),
    "organizations/invites": ("Platform & IAM", "X", "Organizations"),
    "organizations/logs": ("Platform & IAM", "X", "Organizations"),
    "organizations/members": ("Platform & IAM", "X", "Organizations"),
    "organizations/members:batchCreate": ("Platform & IAM", "X", "Organizations"),
    "organizations/profile": ("Platform & IAM", "X", "Organizations"),
    "organizations/shares": ("Platform & IAM", "X", "Organizations"),
    "tenants": ("Platform & IAM", "X", "Tenant API"),
    "tenants/account_types": ("Platform & IAM", "X", "Tenant API"),
    "tenants/accounts": ("Platform & IAM", "X", "Tenant API"),
    "tenants/custom_ns": ("Platform & IAM", "X", "Tenant API"),
    "tenants/entitlements": ("Platform & IAM", "X", "Tenant API"),
    "tenants/memberships": ("Platform & IAM", "X", "Tenant API"),
    "memberships": ("Platform & IAM", "X", "User memberships"),
    "user": ("Platform & IAM", "X", "User-level (user tokens, invites, tenants)"),
    "oauth": ("Platform & IAM", "X", "OAuth scopes"),
    # --- Billing / commercial --------------------------------------------------
    **{k: ("Billing", "X", "Billing / subscriptions / payments") for k in [
        "accounts/billable", "accounts/billable-usage", "accounts/billing", "accounts/invoices",
        "accounts/pay-bad-debt", "accounts/pay-invoice", "accounts/payment-methods", "accounts/receipts",
        "accounts/subscriptions", "accounts/bulk", "accounts/client-secret", "accounts/entitlements",
        "zones/subscription", "zones/subscriptions", "zones/available_plans", "zones/available_rate_plans",
        "zones/entitlements", "billing", "accounts/media", "zones/media", "zones/stream"]},
    # --- Intel / analytics / other read-only -----------------------------------
    **{k: ("Intel & Analytics", "RO", "Threat intel / analytics / scanners (read-only or operational)") for k in [
        "accounts/cloudforce-one", "accounts/intel", "zones/intel", "accounts/brand-protection",
        "accounts/urlscanner", "accounts/vuln_scanner", "accounts/managed-defense", "accounts/botnet_feed",
        "accounts/abuse-reports", "accounts/analytics", "zones/analytics", "analytics", "radar",
        "accounts/reporting", "accounts/request-tracer", "accounts/diagnostics", "ips"]},
    # --- Internal / health ------------------------------------------------------
    **{k: ("Internal", "X", "Health/internal test routes") for k in ["api", "internal", "live", "ready", "signed-url"]},
}

TIER_ORDER = ["T1", "T2", "T3", "RO", "X"]


def group_of(path):
    segs = path.strip("/").split("/")
    if len(segs) > 2 and segs[1].startswith("{"):
        return f"{segs[0]}/{segs[2]}"
    return segs[0]


def main():
    spec = json.load(open(sys.argv[1]))
    ops = collections.Counter()
    for p, item in spec["paths"].items():
        ops[group_of(p)] += sum(1 for m in item if m in ("get", "post", "put", "patch", "delete"))
    missing = sorted(set(ops) - set(C))
    if missing:
        print("UNCLASSIFIED groups:", *missing, sep="\n  ", file=sys.stderr)
        sys.exit(1)

    total = sum(ops.values())
    by_tier = collections.Counter()
    for g, n in ops.items():
        by_tier[C[g][1]] += n
    print("# Cloudflare API coverage map\n")
    print("<!-- generated by hack/classify_api.py; do not edit by hand -->\n")
    print(f"Spec: `cloudflare/api-schemas` openapi.json, {len(spec['paths'])} paths, "
          f"{total} operations, {len(ops)} path groups. Every group is classified.\n")
    print("| Tier | Meaning | Operations |\n|---|---|---|")
    meaning = {"T1": "Hand-written core CRD", "T2": "Generated CRD (declarative long tail)",
               "T3": "Imperative / one-shot (Job-style CRD or opt-in only)",
               "RO": "Read-only / observability (status, metrics, or ignore)",
               "X": "Out of scope (billing, org admin, deprecated, internal)"}
    for t in TIER_ORDER:
        print(f"| {t} | {meaning[t]} | {by_tier[t]} ({by_tier[t] * 100 // total}%) |")
    print()
    cats = collections.defaultdict(list)
    for g in ops:
        cats[C[g][0]].append(g)
    for cat in sorted(cats):
        print(f"## {cat}\n\n| Tier | API group | Ops | What it is |\n|---|---|---|---|")
        for g in sorted(cats[cat], key=lambda g: (TIER_ORDER.index(C[g][1]), g)):
            print(f"| {C[g][1]} | `{g}` | {ops[g]} | {C[g][2]} |")
        print()


if __name__ == "__main__":
    main()
