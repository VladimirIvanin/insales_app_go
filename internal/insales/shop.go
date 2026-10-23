package insales

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// CanonicalShop normalizes a shop domain to a lowercase host without a scheme.
func CanonicalShop(shop string) (string, error) {
	if shop == "" {
		return "", fmt.Errorf("shop is empty")
	}
	input := strings.TrimSpace(shop)
	if !strings.Contains(input, "://") {
		input = "https://" + input
	}
	u, err := url.Parse(input)
	if err != nil {
		return "", fmt.Errorf("invalid shop URL: %w", err)
	}
	if u.User != nil {
		return "", fmt.Errorf("shop URL contains userinfo")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("shop URL contains path")
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("shop URL contains fragment")
	}
	if u.RawQuery != "" {
		return "", fmt.Errorf("shop URL contains query")
	}
	if u.Port() != "" {
		return "", fmt.Errorf("shop URL contains port")
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return "", fmt.Errorf("invalid shop URL: empty host")
	}
	if net.ParseIP(hostname) != nil {
		return "", fmt.Errorf("shop URL is an IP address, domain required")
	}
	if !strings.Contains(hostname, ".") {
		return "", fmt.Errorf("shop %q is not a valid domain", hostname)
	}
	return hostname, nil
}

// ValidateShopURL checks the shop format and exact membership in allowed.
// allowed entries are canonical hosts. The returned value is https://host.
func ValidateShopURL(shop string, allowed []string) (string, error) {
	hostname, err := CanonicalShop(shop)
	if err != nil {
		return "", err
	}
	if len(allowed) == 0 {
		return "", fmt.Errorf("ALLOWED_SHOPS is empty: at least one shop domain must be configured (security)")
	}
	for _, a := range allowed {
		canonical, err := CanonicalShop(a)
		if err != nil {
			continue
		}
		if hostname == canonical {
			return "https://" + hostname, nil
		}
	}
	return "", fmt.Errorf("shop %q is not in the allowed list", hostname)
}
