package controlpanel

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

func acceptsGzip(r *http.Request) bool {
	wildcard := false
	for _, coding := range strings.Split(strings.Join(r.Header.Values("Accept-Encoding"), ","), ",") {
		parts := strings.Split(coding, ";")
		name := strings.TrimSpace(parts[0])
		quality := 1.0
		for _, parameter := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || !strings.EqualFold(key, "q") {
				quality = 0
				break
			}
			q, err := strconv.ParseFloat(value, 64)
			if err != nil || !(q >= 0 && q <= 1) {
				quality = 0
				break
			}
			quality = q
		}
		if strings.EqualFold(name, "gzip") {
			return quality > 0
		}
		if name == "*" {
			wildcard = quality > 0
		}
	}
	return wildcard
}

// Only public text assets and the live JSON endpoint use this writer. HTML
// contains CSRF tokens and reflected input and must never be compressed here.
func writePanelText(w http.ResponseWriter, r *http.Request, status int, data []byte) {
	w.Header().Add("Vary", "Accept-Encoding")
	if status == http.StatusNotModified {
		w.Header().Del("Content-Length")
		w.WriteHeader(status)
		return
	}
	if acceptsGzip(r) {
		var compressed bytes.Buffer
		z := gzip.NewWriter(&compressed)
		_, _ = z.Write(data) // bytes.Buffer cannot fail.
		_ = z.Close()
		data = compressed.Bytes()
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
