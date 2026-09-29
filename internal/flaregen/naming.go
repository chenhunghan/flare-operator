package flaregen

import (
	"strings"
	"unicode"
)

// initialisms are upper-cased whole when they form a word of a JSON name
// (queue_id → QueueID, url → URL), following Go and Kubernetes conventions.
var initialisms = map[string]string{
	"acl": "ACL", "api": "API", "asn": "ASN", "cpu": "CPU", "cidr": "CIDR", "dns": "DNS", "dnssec": "DNSSEC",
	"eu": "EU", "fqdn": "FQDN", "html": "HTML", "http": "HTTP", "https": "HTTPS", "id": "ID", "ids": "IDs",
	"ip": "IP", "ips": "IPs", "ipv4": "IPv4", "ipv6": "IPv6", "json": "JSON", "jwt": "JWT", "kv": "KV",
	"mtls": "MTLS", "sql": "SQL", "ssh": "SSH", "ssl": "SSL", "tcp": "TCP", "tls": "TLS", "ttl": "TTL",
	"udp": "UDP", "uri": "URI", "url": "URL", "urls": "URLs", "uuid": "UUID", "vpc": "VPC", "d1": "D1",
	"r2": "R2", "waf": "WAF", "cname": "CNAME", "mx": "MX", "srv": "SRV", "ssh2": "SSH2",
}

// splitWords splits an API name (snake_case, kebab-case, camelCase, dotted)
// into lower-case words.
func splitWords(s string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r):
			// Break before an upper-case letter that follows a lower-case letter or digit,
			// or that starts a new word after an acronym ("HTTPServer" → http, server).
			if len(cur) > 0 {
				prev := rs[i-1]
				nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
				if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
					flush()
				}
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return words
}

// GoName converts an API field name to an exported Go identifier.
func GoName(s string) string {
	var b strings.Builder
	for _, w := range splitWords(s) {
		if up, ok := initialisms[w]; ok {
			b.WriteString(up)
			continue
		}
		b.WriteString(strings.ToUpper(w[:1]) + w[1:])
	}
	out := b.String()
	if out == "" {
		return "Field"
	}
	if unicode.IsDigit(rune(out[0])) {
		out = "X" + out
	}
	return out
}

// singular turns a plural resource segment into its singular form. It covers
// the plural shapes used by Cloudflare's fern group names.
func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 3:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "shes"), strings.HasSuffix(s, "ches"), strings.HasSuffix(s, "xes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "ss"), strings.HasSuffix(s, "us"), strings.HasSuffix(s, "is"):
		return s
	case strings.HasSuffix(s, "s") && len(s) > 1:
		return s[:len(s)-1]
	}
	return s
}

// plural lower-cases a Kind and pluralises it for the CRD resource name.
func plural(kind string) string {
	s := strings.ToLower(kind)
	switch {
	case strings.HasSuffix(s, "ings"), strings.HasSuffix(s, "data"):
		return s // settings, bindings, metadata: already plural
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsRune("aeiou", rune(s[len(s)-2])):
		return s[:len(s)-1] + "ies"
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	}
	return s + "s"
}

// productOf returns the API group product for a fern group name: its first
// dotted segment with "_" and "-" removed (api/common/v1alpha1 conventions).
func productOf(fernGroup string) string {
	first, _, _ := strings.Cut(fernGroup, ".")
	return strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(first))
}

// defaultKind derives a Kind from the last fern group segment ("kv.namespaces" → "Namespace").
func defaultKind(fernGroup string) string {
	segs := strings.Split(fernGroup, ".")
	return GoName(singular(segs[len(segs)-1]))
}
