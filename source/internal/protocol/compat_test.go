package protocol

import (
	"strings"
	"testing"
)

func TestCompatibilityManifestStrict(t *testing.T) {
	good := `{"schema":2,"servers":[{"line":"0.17","minClient":"0.12.0"}],"clients":[{"line":"0.17","minServer":"0.12.0"}]}`
	if _, e := ParseCompatibility([]byte(good)); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{strings.Replace(good, `"schema":2`, `"schema":1`, 1), strings.Replace(good, `"schema":2`, `"schema":2,"extra":true`, 1), strings.Replace(good, `0.12.0`, `01.12.0`, 1), strings.Replace(good, `"line":"0.17"`, `"line":"0.17","extra":true`, 1), strings.Replace(good, `"clients":[`, `"clients":[{"line":"0.17","minServer":"0.12.0"},`, 1), good + `{}`} {
		if _, e := ParseCompatibility([]byte(bad)); e == nil {
			t.Fatalf("accepted malformed manifest: %s", bad)
		}
	}
}
func TestCompatibilityPolicySelectionAndSemver(t *testing.T) {
	for _, version := range []string{"dev", "dev-abcd", "not-a-version"} {
		p, ok := ServerPolicy(version)
		if !ok || p.Line != "1.1" {
			t.Fatal("policy selection", version)
		}
	}
	for _, tc := range []struct{ version, line string }{{"0.99.0", "0.20"}, {"1.0.0", "1.0"}, {"1.1.0", "1.1"}, {"0.20.0", "0.20"}, {"0.19.0", "0.20"}, {"0.17.2", "0.20"}, {"dev", "1.1"}} {
		if p, ok := ServerPolicy(tc.version); !ok || p.Line != tc.line {
			t.Fatal("fallback", tc)
		}
	}
	for _, tc := range []struct {
		a, b string
		cmp  int
	}{{"0.12.0", "0.12.0", 0}, {"0.11.9", "0.12.0", -1}, {"0.12.0-rc.1", "0.12.0", -1}, {"0.12.0-rc.10", "0.12.0-rc.2", 1}, {"0.12.0+meta", "0.12.0", 0}, {"999999999999999999999999.1.0", "1.0.0", 1}} {
		got, ok := CompareVersions(tc.a, tc.b)
		if !ok || (got < 0) != (tc.cmp < 0) || (got > 0) != (tc.cmp > 0) {
			t.Fatal(tc, got, ok)
		}
	}
	if _, ok := CompareVersions("0.12.0-01", "0.12.0"); ok {
		t.Fatal("invalid numeric prerelease")
	}
}

func TestMinimumClientRCAndStable(t *testing.T) {
	for _, v := range []string{"0.20.0", "1.0.0"} {
		p, ok := ServerPolicy(v)
		if !ok || p.MinClient != "0.17.3" {
			t.Error("minimum client", v)
		}
		for _, old := range []string{"0.1.0", "0.17.2", "0.17.3-rc.1", "bad", "", "dev", "dev-injected"} {
			if p.Accepts(old) {
				t.Errorf("accepted old client %q", old)
			}
		}
		if !p.Accepts("0.17.3") {
			t.Error("rejected supported client")
		}
		c, ok := ClientPolicy(v)
		if !ok || c.MinServer != "0.17.3" {
			t.Error("minimum server", v)
		}
	}
}
