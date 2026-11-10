package protocol

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

//go:embed compat.json
var compatibilityJSON []byte

type ServerCompatibility struct {
	Line      string `json:"line"`
	MinClient string `json:"minClient"`
}
type ClientCompatibility struct {
	Line      string `json:"line"`
	MinServer string `json:"minServer"`
}
type CompatibilityManifest struct {
	Schema  int                   `json:"schema"`
	Servers []ServerCompatibility `json:"servers"`
	Clients []ClientCompatibility `json:"clients"`
}

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)
var linePattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func versionParts(v string) []string {
	p := versionPattern.FindStringSubmatch(v)
	if p == nil {
		return nil
	}
	for _, id := range strings.Split(p[4], ".") {
		if numeric(id) && len(id) > 1 && id[0] == '0' {
			return nil
		}
	}
	return p
}
func numeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func numberCompare(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

// CompareVersions uses SemVer precedence, including prerelease identifiers.
func CompareVersions(a, b string) (int, bool) {
	x, y := versionParts(a), versionParts(b)
	if x == nil || y == nil {
		return 0, false
	}
	for i := 1; i <= 3; i++ {
		if c := numberCompare(x[i], y[i]); c != 0 {
			return c, true
		}
	}
	if x[4] == y[4] {
		return 0, true
	}
	if x[4] == "" {
		return 1, true
	}
	if y[4] == "" {
		return -1, true
	}
	xs, ys := strings.Split(x[4], "."), strings.Split(y[4], ".")
	for i := 0; i < len(xs) && i < len(ys); i++ {
		a, b := xs[i], ys[i]
		c := strings.Compare(a, b)
		if numeric(a) && numeric(b) {
			c = numberCompare(a, b)
		} else if numeric(a) {
			c = -1
		} else if numeric(b) {
			c = 1
		}
		if c != 0 {
			return c, true
		}
	}
	if len(xs) < len(ys) {
		return -1, true
	}
	return 1, true
}
func VersionLine(v string) string {
	p := versionParts(v)
	if p == nil {
		return ""
	}
	return p[1] + "." + p[2]
}
func ParseCompatibility(data []byte) (CompatibilityManifest, error) {
	var m CompatibilityManifest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, err
	}
	if d.Decode(new(any)) != io.EOF {
		return m, errors.New("compatibility trailing JSON")
	}
	if m.Schema != 2 || len(m.Servers) == 0 || len(m.Clients) == 0 {
		return m, errors.New("invalid compatibility schema")
	}
	seen := map[string]bool{}
	for _, s := range m.Servers {
		if !linePattern.MatchString(s.Line) || versionParts(s.MinClient) == nil || seen[s.Line] {
			return m, errors.New("invalid server compatibility entry")
		}
		seen[s.Line] = true
	}
	seen = map[string]bool{}
	for _, c := range m.Clients {
		if !linePattern.MatchString(c.Line) || versionParts(c.MinServer) == nil || seen[c.Line] {
			return m, errors.New("invalid client compatibility entry")
		}
		seen[c.Line] = true
	}
	return m, nil
}

var compatibility = func() CompatibilityManifest {
	m, e := ParseCompatibility(compatibilityJSON)
	if e != nil {
		panic(e)
	}
	return m
}()

// Unlisted server versions inherit the nearest preceding line, or the oldest
// entry if they predate the manifest. Dev builds use the newest line. The release
// guard, rather than an indiscriminate denial, requires an exact release entry.
func (m CompatibilityManifest) Server(version string) (ServerCompatibility, bool) {
	line := VersionLine(version)
	var oldest, newest, preceding ServerCompatibility
	for _, e := range m.Servers {
		if e.Line == line {
			return e, true
		}
		if oldest.Line == "" {
			oldest = e
			newest = e
		}
		if c, _ := CompareVersions(e.Line+".0", oldest.Line+".0"); c < 0 {
			oldest = e
		}
		if c, _ := CompareVersions(e.Line+".0", newest.Line+".0"); c > 0 {
			newest = e
		}
		if line != "" {
			if c, _ := CompareVersions(e.Line+".0", line+".0"); c <= 0 {
				if preceding.Line == "" {
					preceding = e
				} else if c, _ := CompareVersions(e.Line+".0", preceding.Line+".0"); c > 0 {
					preceding = e
				}
			}
		}
	}
	if line == "" {
		return newest, true
	}
	if preceding.Line != "" {
		return preceding, true
	}
	return oldest, oldest.Line != ""
}
func (m CompatibilityManifest) Client(version string) (ClientCompatibility, bool) {
	line := VersionLine(version)
	var newest ClientCompatibility
	for _, e := range m.Clients {
		if e.Line == line {
			return e, true
		}
		if newest.Line == "" {
			newest = e
		} else if c, _ := CompareVersions(e.Line+".0", newest.Line+".0"); c > 0 {
			newest = e
		}
	}
	return newest, line == ""
}
func ServerPolicy(version string) (ServerCompatibility, bool) { return compatibility.Server(version) }
func ClientPolicy(version string) (ClientCompatibility, bool) { return compatibility.Client(version) }
func (s ServerCompatibility) Accepts(version string) bool {
	c, ok := CompareVersions(version, s.MinClient)
	return ok && c >= 0
}
func CompatibilityGuidance(code, client, server, minimum string) string {
	if client == "0.1.0" {
		client = "<=0.14"
	}
	// Caller-supplied arbitrary strings must never be echoed into user messages.
	safe := func(v string) string {
		if versionParts(v) != nil || v == "<=0.14" {
			return v
		}
		return "dev/unknown"
	}
	return fmt.Sprintf("%s: client %s; server %s; required minimum %s. Run npm install -g @xelexis/x-broker@latest, then reconnect (Claude: /mcp; Codex: restart the app).", code, safe(client), safe(server), safe(minimum))
}
