package controlpanel

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// Every icon is an unmodified file from a pinned package; nobody draws, traces
// or generates one (user rule, 2026-11-09).
func TestIconAssetsComeFromLicensedPackages(t *testing.T) {
	accepted := map[string]struct{ pkg, license string }{
		"lucide-": {"lucide-static", "ISC"},
		"tabler-": {"@tabler/icons", "MIT"},
	}
	entries, err := assets.ReadDir("icons")
	if err != nil {
		t.Fatal(err)
	}
	svgs := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".svg") {
			continue
		}
		svgs++
		prefix, base, _ := strings.Cut(name, "-")
		rule, ok := accepted[prefix+"-"]
		if !ok {
			t.Errorf("%s: icon is not from an accepted package", name)
			continue
		}
		asset, ok := designAssets["/assets/icons/"+name]
		if !ok {
			t.Errorf("%s: missing from asset-manifest.json", name)
			continue
		}
		version, _ := asset.Source["version"].(string)
		file, _ := asset.Source["packageFile"].(string)
		if asset.License != rule.license || asset.Source["package"] != rule.pkg || version == "" || !strings.HasPrefix(file, "icons/") || !strings.HasSuffix(file, "/"+base) {
			t.Errorf("%s: license %q source %v does not name its package, version and path", name, asset.License, asset.Source)
			continue
		}
		data, err := assets.ReadFile("icons/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if header := "<!-- @license lucide-static v" + version + " - ISC -->"; rule.pkg == "lucide-static" && !strings.HasPrefix(string(data), header) {
			t.Errorf("%s: does not start with the published header %q", name, header)
		}
	}
	if svgs == 0 {
		t.Fatal("no icons found")
	}

	data, err := assets.ReadFile("asset-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		IconNames map[string]string `json:"iconNames"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for name, path := range manifest.IconNames {
		if _, ok := designAssets[path]; !ok || !(strings.HasPrefix(path, "/assets/icons/lucide-") || strings.HasPrefix(path, "/assets/icons/tabler-")) {
			t.Errorf("iconNames[%s] = %q is not an accepted packaged icon", name, path)
		}
	}

	reference := regexp.MustCompile(`/assets/icons/[^"'?) ]+`)
	for _, file := range []string{"templates.html", "design.css", "select.css"} {
		text, err := assets.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range reference.FindAllString(string(text), -1) {
			if _, ok := designAssets[path]; !ok {
				t.Errorf("%s references unlisted icon %s", file, path)
			}
		}
	}
}
