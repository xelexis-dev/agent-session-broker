package serverruntime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestMisuse5Y1Panel300(t *testing.T) {
	root := os.Getenv("ASB_MISUSE_Y_ROOT")
	if root == "" {
		t.Skip("explicit owned temp fixture required")
	}
	start := time.Now()
	h, closeHandler, e := newManagedHandler(root, nil)
	if e != nil {
		t.Fatal("start", e)
	}
	defer closeHandler()
	t.Logf("startup/sweep_ms=%.3f", float64(time.Since(start).Microseconds())/1000)
	r := &deviceRuntime{t: t, h: h, dir: root, project: "demo", password: "misuse5-test-password"}
	r.login(r.password)
	// Real TCP loopback server; browser cookie stays in memory.
	server := httptest.NewServer(h)
	defer server.Close()
	for i := 0; i < 6; i++ {
		req, e := http.NewRequest("GET", server.URL+"/projects/sessions?project=demo", nil)
		if e != nil {
			t.Fatal(e)
		}
		req.AddCookie(r.cookie)
		start = time.Now()
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		body, e := io.ReadAll(resp.Body)
		resp.Body.Close()
		elapsed := time.Since(start)
		if e != nil || resp.StatusCode != 200 {
			t.Fatal("panel", resp.StatusCode, e)
		}
		t.Logf("listing_%d_ms=%.3f bytes=%d", i, float64(elapsed.Microseconds())/1000, len(body))
	}
}
