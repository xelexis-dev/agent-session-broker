package accesslog

import (
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestClientAddressTrustedSuffixBoundaries(t *testing.T) {
	prefixes := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, tc := range []struct {
		name, xff, peer, xfp, want string
		opt                        bool
	}{
		{"off", "203.0.113.7", "10.0.0.1:42", "https", "10.0.0.1", false},
		{"set", "203.0.113.7", "10.0.0.1:42", "https", "203.0.113.7", true},
		{"left-padding", "unknown, , " + strings.Repeat("padding,", 40) + "203.0.113.7,10.0.0.2", "10.0.0.1:42", "https", "203.0.113.7", true},
		{"bad-suffix", "203.0.113.7, unknown, 10.0.0.2", "10.0.0.1:42", "https", "10.0.0.1", true},
		{"32nd", "203.0.113.7," + strings.TrimSuffix(strings.Repeat("10.0.0.2,", 31), ","), "10.0.0.1:42", "http", "203.0.113.7", true},
		{"33rd", "203.0.113.7," + strings.TrimSuffix(strings.Repeat("10.0.0.2,", 32), ","), "10.0.0.1:42", "https", "10.0.0.1", true},
		{"zone", "203.0.113.7", "[fe80::1%fixture]:42", "https", "unknown", true},
		{"port", "203.0.113.7:42", "10.0.0.1:42", "https", "10.0.0.1", true},
		{"missing-proof", "203.0.113.7", "10.0.0.1:42", "", "10.0.0.1", true},
		{"untrusted-peer", "203.0.113.7", "192.0.2.1:42", "https", "192.0.2.1", true},
		{"mapped", "::ffff:203.0.113.7", "[::ffff:10.0.0.1]:42", "https", "203.0.113.7", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("X-Forwarded-For", tc.xff)
			if tc.xfp != "" {
				r.Header.Set("X-Forwarded-Proto", tc.xfp)
			}
			out := ClientAddress(r, tc.opt, prefixes)
			if out.Client != tc.want {
				t.Fatal("trusted suffix selected wrong client")
			}
			if out.Source == "forwarded" && out.Ignored {
				t.Fatal("accepted forwarding marked ignored")
			}
		})
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:42"
	r.Header["X-Forwarded-For"] = []string{"unknown,203.0.113.7", "10.0.0.2"}
	r.Header.Set("X-Forwarded-Proto", "https")
	if ClientAddress(r, true, prefixes).Client != "203.0.113.7" {
		t.Fatal("multiple wire-order headers not handled")
	}
	r.Header.Add("X-Forwarded-Proto", "https")
	if !ClientAddress(r, true, prefixes).Ignored {
		t.Fatal("duplicate scheme proof accepted")
	}
}
func TestMappedProxyNormalizationAndConfigurationBounds(t *testing.T) {
	normalized, err := NormalizeProxies([]netip.Prefix{netip.MustParsePrefix("::ffff:10.3.4.5/104")})
	if err != nil || len(normalized) != 1 || normalized[0].String() != "10.0.0.0/8" {
		t.Fatal("mapped normalization failed")
	}
	if _, err = NormalizeProxies([]netip.Prefix{netip.MustParsePrefix("::ffff:10.0.0.1/95")}); err == nil {
		t.Fatal("ambiguous mapped prefix accepted")
	}
	for _, edit := range []func(*Config){func(c *Config) { c.MaxSizeMiB = 0 }, func(c *Config) { c.MaxFiles = 1001 }, func(c *Config) { c.MaxAgeDays = 3651 }, func(c *Config) { c.MaxSizeMiB = 1024; c.MaxFiles = 10 }, func(c *Config) { c.File = "-" }} {
		c := Defaults()
		c.File = "logs/access.log"
		edit(&c)
		if c.Validate() == nil {
			t.Fatal("invalid retention accepted")
		}
	}
	c := Defaults()
	c.File = "logs/access.log"
	c.MaxSizeMiB = 1024
	c.MaxFiles = 9
	if c.Validate() != nil {
		t.Fatal("exact product bound rejected")
	}
	c.File = ""
	c.RetentionSet = true
	if c.Validate() == nil {
		t.Fatal("off retention option accepted")
	}
}
