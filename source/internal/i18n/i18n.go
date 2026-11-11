// Package i18n holds the control panel's English and Korean message catalogs (1.1.0).
//
// Catalogs are embedded JSON files, one per language and surface (en/<surface>.json, ko/<surface>.json). A value is
// plain text with named {placeholders}, or {"one": ..., "other": ...} when a count changes the wording. Values never
// contain markup: templates own the markup and html/template escapes the result. Placeholders are filled in a single
// pass, so text inside an argument (a display name such as "{n}") is never interpreted.
package i18n

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
)

const (
	English = "en"
	Korean  = "ko"
)

// Languages lists the supported languages; English is the default and the fallback.
var Languages = []string{English, Korean}

// Normalize returns a supported language code or "".
func Normalize(language string) string {
	if language == English || language == Korean {
		return language
	}
	return ""
}

// Entry is one message. Other is always set; One is the singular form used when the count argument "n" is 1.
type Entry struct {
	One, Other string
}

//go:embed en/*.json ko/*.json
var files embed.FS

var catalogs = mustLoad(files)

func mustLoad(fsys fs.FS) map[string]map[string]Entry {
	out := map[string]map[string]Entry{}
	for _, language := range Languages {
		catalog, err := load(fsys, language)
		if err != nil {
			panic("i18n: " + err.Error()) // embedded data; the package tests fail first
		}
		out[language] = catalog
	}
	return out
}

func load(fsys fs.FS, language string) (map[string]Entry, error) {
	names, err := fs.Glob(fsys, language+"/*.json")
	if err != nil || len(names) == 0 {
		return nil, fmt.Errorf("%s: no catalog files", language)
	}
	out := map[string]Entry{}
	for _, name := range names {
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		if err := decode(raw, func(key string, entry Entry) error {
			if _, dup := out[key]; dup {
				return fmt.Errorf("%s: duplicate key %q", name, key)
			}
			out[key] = entry
			return nil
		}); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return out, nil
}

// decode reads one flat object strictly: unique keys, string or {"one","other"} values, nothing after the object.
func decode(raw []byte, add func(string, Entry) error) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return fmt.Errorf("not an object")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || key == "" || seen[key] {
			return fmt.Errorf("invalid or duplicate key %v", token)
		}
		seen[key] = true
		var entry Entry
		switch value, err := d.Token(); {
		case err != nil:
			return err
		case value == json.Delim('{'):
			forms := map[string]bool{}
			for d.More() {
				name, err := d.Token()
				var text string
				if err != nil || (name != "one" && name != "other") || forms[name.(string)] || d.Decode(&text) != nil {
					return fmt.Errorf("%s: invalid plural form", key)
				}
				forms[name.(string)] = true
				if name == "one" {
					entry.One = text
				} else {
					entry.Other = text
				}
			}
			if _, err := d.Token(); err != nil || !forms["other"] {
				return fmt.Errorf("%s: plural needs other", key)
			}
		default:
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("%s: value is not text", key)
			}
			entry.Other = text
		}
		if err := add(key, entry); err != nil {
			return err
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("data after the object")
	}
	return nil
}

// T returns the message for key in language, with name/value argument pairs. A key missing in the language falls
// back to English and then to the key itself; the catalog tests keep both cases out of releases.
func T(language, key string, args ...any) string {
	entry, ok := catalogs[language][key]
	if !ok {
		entry, ok = catalogs[English][key]
	}
	if !ok {
		return key
	}
	return entry.format(args)
}

func (e Entry) format(args []any) string {
	text := e.Other
	if e.One != "" {
		if n, ok := count(args); ok && n == 1 {
			text = e.One
		}
	}
	return substitute(text, args)
}

func count(args []any) (int64, bool) {
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] != "n" {
			continue
		}
		switch v := args[i+1].(type) {
		case int:
			return int64(v), true
		case int64:
			return v, true
		case uint64:
			return int64(v), true
		}
	}
	return 0, false
}

// substitute fills {name} placeholders in one pass; argument values are copied, never scanned again.
func substitute(text string, args []any) string {
	if !strings.Contains(text, "{") {
		return text
	}
	values := map[string]string{}
	for i := 0; i+1 < len(args); i += 2 {
		if name, ok := args[i].(string); ok {
			values[name] = fmt.Sprint(args[i+1])
		}
	}
	var b strings.Builder
	for {
		start := strings.IndexByte(text, '{')
		if start < 0 {
			b.WriteString(text)
			return b.String()
		}
		end := strings.IndexByte(text[start:], '}')
		name := ""
		if end > 0 {
			name = text[start+1 : start+end]
		}
		value, known := values[name]
		if end < 0 || !validName(name) || !known {
			b.WriteString(text[:start+1])
			text = text[start+1:]
			continue
		}
		b.WriteString(text[:start])
		b.WriteString(value)
		text = text[start+end+1:]
	}
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// Placeholders lists the placeholder names of a text (for the catalog tests).
func Placeholders(text string) []string {
	var names []string
	for {
		start := strings.IndexByte(text, '{')
		if start < 0 {
			sort.Strings(names)
			return names
		}
		end := strings.IndexByte(text[start:], '}')
		if end < 0 {
			sort.Strings(names)
			return names
		}
		if name := text[start+1 : start+end]; validName(name) {
			names = append(names, name)
		}
		text = text[start+end+1:]
	}
}

// Message is a key with arguments, translated when the request language is known.
type Message struct {
	Key  string
	Args []any
}

// M builds a Message.
func M(key string, args ...any) Message { return Message{Key: key, Args: args} }

// In translates the message.
func (m Message) In(language string) string { return T(language, m.Key, m.Args...) }

// Has reports whether the English catalog defines key.
func Has(key string) bool {
	_, ok := catalogs[English][key]
	return ok
}

// Lookup returns the raw entry of key in language (for tests).
func Lookup(language, key string) (Entry, bool) {
	entry, ok := catalogs[language][key]
	return entry, ok
}

// Keys returns the sorted keys of a language's catalog.
func Keys(language string) []string {
	keys := make([]string, 0, len(catalogs[language]))
	for key := range catalogs[language] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// JS returns the js.* messages of a language (English for any missing key) for the page's JSON data block.
// A counted message becomes {"one": ..., "other": ...}.
func JS(language string) map[string]any {
	out := map[string]any{}
	for key, entry := range catalogs[English] {
		if !strings.HasPrefix(key, "js.") {
			continue
		}
		if own, ok := catalogs[language][key]; ok {
			entry = own
		}
		if entry.One != "" {
			out[key] = map[string]string{"one": entry.One, "other": entry.Other}
		} else {
			out[key] = entry.Other
		}
	}
	return out
}
