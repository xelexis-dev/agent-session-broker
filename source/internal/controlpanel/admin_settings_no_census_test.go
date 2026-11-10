package controlpanel

import (
	"net/http"
	"strings"
	"testing"
)

func TestAdminSettingsOmitsVersionCensus(t *testing.T) {
	h := New(userStore(t), nil, http.NotFoundHandler())
	response := loginPanelUser(t, h, "admin").call("GET", "/admin/settings", nil)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "client-version-census") || strings.Contains(response.Body.String(), "클라이언트 버전 현황") {
		t.Fatal("admin settings must omit retired version census")
	}
}
