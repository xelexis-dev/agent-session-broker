package controlpanel

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestPanelGzipAssetsAndLiveJSON(t *testing.T) {
	h := New(userStore(t), &liveFixtureBackend{rows: []Session{{ID: "fixture", Name: "worker", Status: "alive", Ready: true}}}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "owner")
	for _, path := range []string{"/assets/panel.css", "/assets/panel.js", "/assets/tokens.css", "/projects/sessions/live?project=demo"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://broker"+path, nil)
			req.AddCookie(c.cookie)
			req.Header.Set("Accept-Encoding", "br, gzip")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" {
				t.Fatalf("status=%d encoding=%q, want gzip", w.Code, w.Header().Get("Content-Encoding"))
			}
			if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
				t.Fatal("missing Vary")
			}
			z, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(z)
			z.Close()
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(path, "/assets/") {
				plain := c.call("GET", path, nil)
				if !bytes.Equal(data, plain.Body.Bytes()) {
					t.Fatal("gzip changes asset body")
				}
			} else {
				var value struct{ Rows []liveSessionRow }
				if json.Unmarshal(data, &value) != nil || len(value.Rows) != 1 {
					t.Fatal("invalid live rows")
				}
				if w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("live JSON cache policy changed")
				}
			}
			if n := w.Header().Get("Content-Length"); n != "" && n != strconv.Itoa(w.Body.Len()) {
				t.Fatal("stale content length")
			}
		})
	}
}

func TestPanelGzipNegotiationAndExclusions(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	for _, tc := range []struct {
		encoding string
		gzip     bool
	}{{"", false}, {"br", false}, {"gzip", true}, {"GZip; q=0.5", true}, {"gzip;q=0", false}, {"*;q=1, gzip;q=0", false}, {"*;q=0.5", true}, {"gzip;q=bogus", false}, {"gzip;q=2", false}, {"gzip;q=0.0, br", false}} {
		t.Run(tc.encoding, func(t *testing.T) {
			for _, method := range []string{"GET", "HEAD"} {
				req := httptest.NewRequest(method, "http://broker/assets/panel.css", nil)
				req.Header.Set("Accept-Encoding", tc.encoding)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if got := w.Header().Get("Content-Encoding") == "gzip"; got != tc.gzip {
					t.Errorf("%s gzip=%v want%v", method, got, tc.gzip)
				}
				if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
					t.Error("identity must vary too")
				}
				if method == "HEAD" && w.Body.Len() != 0 {
					t.Fatal("HEAD has body")
				}
			}
		})
	}
	font := ""
	for path, a := range designAssets {
		if strings.Contains(a.ContentType, "font") {
			font = path
			break
		}
	}
	if font == "" {
		t.Fatal("font fixture missing")
	}
	for _, path := range []string{"/login", "/assets/icons/lucide-activity.svg", font, "/assets/missing.css"} {
		req := httptest.NewRequest("GET", "http://broker"+path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Header().Get("Content-Encoding") != "" {
			t.Errorf("compressed excluded %s", path)
		}
	}
}

func TestPanelTextNotModifiedHasNoBody(t *testing.T) {
	req := httptest.NewRequest("GET", "http://broker/assets/panel.css", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	w.Header().Set("Content-Length", "999")
	w.Header().Set("Vary", "Origin")
	writePanelText(w, req, http.StatusNotModified, []byte("ignored"))
	if w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "" || w.Header().Get("Content-Encoding") != "" {
		t.Fatal("304 has a body or entity encoding/length")
	}
	if strings.Join(w.Header().Values("Vary"), ",") != "Origin,Accept-Encoding" {
		t.Fatal("existing Vary lost")
	}
}
