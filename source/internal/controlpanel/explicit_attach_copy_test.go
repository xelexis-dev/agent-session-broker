package controlpanel

import (
	"bytes"
	"strings"
	"testing"
)

func TestTokenFormExplainsExplicitConnectionRequest(t *testing.T) {
	var html bytes.Buffer
	if err := pages.ExecuteTemplate(&html, "page", uiCopyPreview("settings")); err != nil {
		t.Fatal(err)
	}
	body := html.String()
	const help = "사용자가 이 대화의 연결을 요청하면 승인 절차 없이 등록합니다. 새 대화가 시작되는 것만으로 등록하지 않습니다."
	if !strings.Contains(body, "<small>"+help+"</small>") {
		t.Error("checkbox help must contain exactly the agreed two sentences")
	}
	for _, want := range []string{"연결 요청 시 승인 없이 등록", "사용자가 이 대화의 연결을 요청하면 승인 절차 없이 등록합니다.", "새 대화가 시작되는 것만으로 등록하지 않습니다.", "session:register:auto · Owner 또는 인스턴스 관리자 전용", "data-token-mode>연결 요청 시 승인 없이 등록"} {
		if !strings.Contains(body, want) {
			t.Errorf("token form missing %q", want)
		}
	}
}
