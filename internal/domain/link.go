package domain

import (
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Link is the central entity: a short code that redirects to a target URL.
type Link struct {
	ID        int64  `json:"-"`
	Code      string `json:"code"`
	TargetURL string `json:"target_url"`
	OwnerID   int64  `json:"-"`
	// DomainID is the custom domain this link is served on, or nil for the
	// operator's own domain. It is stamped when the link is created and never
	// changes, so a link already shared keeps the hostname it was shared with.
	DomainID *int64 `json:"-"`
	// Host is DomainID resolved to a hostname, carried alongside the link so
	// the redirect path needs no second lookup. Empty means the default domain.
	Host      string     `json:"-"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// IsExpired reports whether the link should no longer redirect. Links without
// an expiry never expire.
func (l *Link) IsExpired(now time.Time) bool {
	return l.ExpiresAt != nil && !now.Before(*l.ExpiresAt)
}

// aliasPattern constrains user supplied codes to URL-safe characters. The lower
// bound of 3 keeps aliases from colliding with the short reserved words below,
// the upper bound of 32 keeps the index small.
var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)

// reservedAliases are path segments the router serves itself. Without this
// check a user could claim "api" and shadow the management endpoints, because
// the redirect handler is mounted on the root path.
var reservedAliases = map[string]struct{}{
	"api": {}, "healthz": {}, "readyz": {}, "metrics": {}, "docs": {},
	"openapi.yaml": {}, "favicon.ico": {}, "robots.txt": {}, "static": {},
	"admin": {}, "login": {}, "signup": {}, "internal": {},
}

// ValidateAlias checks a user supplied short code.
func ValidateAlias(alias string) error {
	if !aliasPattern.MatchString(alias) {
		return invalid("alias", "must be 3-32 characters of [A-Za-z0-9_-]")
	}
	if _, ok := reservedAliases[strings.ToLower(alias)]; ok {
		return invalid("alias", "this alias is reserved")
	}
	return nil
}

// codePattern matches anything that could be stored in links.code: generated
// base62 codes and user aliases alike.
var codePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)

// IsPossibleCode reports whether s is shaped like a short code. The redirect
// handler uses it to reject junk paths before they reach Redis or Postgres,
// which turns a scan for /wp-login.php into a few nanoseconds of regexp.
func IsPossibleCode(s string) bool { return codePattern.MatchString(s) }

const maxTargetURLLength = 2048

// NormalizeTargetURL validates a redirect target and returns its canonical
// form. Besides the obvious syntax checks it refuses to point at private
// network space: an open redirector into a VPC is a classic SSRF pivot, and a
// shortener is exactly the kind of service that gets abused for it.
func NormalizeTargetURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", invalid("target_url", "must not be empty")
	}
	if len(raw) > maxTargetURLLength {
		return "", invalid("target_url", "must be at most 2048 characters")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", invalid("target_url", "is not a valid URL")
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", invalid("target_url", "must use the http or https scheme")
	}

	host := u.Hostname()
	if host == "" {
		return "", invalid("target_url", "must contain a host")
	}
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return "", invalid("target_url", "must not point at a private or loopback address")
	}
	if isLocalHostname(host) {
		return "", invalid("target_url", "must not point at a local address")
	}

	// url.String() re-encodes the URL, which drops sloppy input such as spaces.
	return u.String(), nil
}

func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	// 100.64.0.0/10, the carrier-grade NAT range, is not covered by IsPrivate.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	return true
}

func isLocalHostname(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local")
}

// hostnamePattern is deliberately stricter than the DNS grammar: labels of
// letters, digits and inner hyphens, at least two of them. It is not trying to
// be a complete implementation of RFC 1035, only to reject the shapes that
// would be pointless or dangerous to hand to a certificate authority.
var hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// NormalizeHostname validates a customer-supplied domain and returns it in the
// canonical form stored in custom_domains.hostname: lower case, no trailing
// dot, no scheme or path.
//
// The checks matter more than usual here, because this value ends up in a
// certificate request. A wildcard, an IP literal or a local name would either
// be refused by the certificate authority or, worse, quietly burn one of the
// account's rate-limited orders.
func NormalizeHostname(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(raw))
	h = strings.TrimSuffix(h, ".")

	// Accept a pasted URL, since that is what people reach for.
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	h, _, _ = strings.Cut(h, "/")
	h, _, _ = strings.Cut(h, ":")

	switch {
	case h == "":
		return "", invalid("domain", "must not be empty")
	case len(h) > 253:
		return "", invalid("domain", "must be at most 253 characters")
	case strings.HasPrefix(h, "*"):
		return "", invalid("domain", "wildcard domains are not supported")
	case net.ParseIP(h) != nil:
		return "", invalid("domain", "must be a domain name, not an IP address")
	case isLocalHostname(h):
		return "", invalid("domain", "must be a public domain name")
	case !hostnamePattern.MatchString(h):
		return "", invalid("domain", "must look like example.com or links.example.com")
	}
	return h, nil
}
