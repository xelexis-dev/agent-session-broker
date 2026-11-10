package controlpanel

import (
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/attachments"
	"net/http"
	"strings"
	"testing"
	"time"
)

func attachmentUIFixture(t *testing.T, state string) *Store {
	t.Helper()
	s := userStore(t)
	s.now = func() time.Time { return time.Date(2026, 11, 4, 6, 20, 0, 0, time.UTC) }
	actor := "admin"
	if state == "long" {
		actor = "administrator-with-a-long-name"
		if err := s.CreateUser("admin", actor, testPassword, "admin"); err != nil {
			t.Fatal("long actor fixture")
		}
	}
	if state != "off" && !strings.HasPrefix(state, "off-") {
		if err := s.SetAttachmentPolicy(actor, true); err != nil {
			t.Fatal("policy fixture")
		}
	}
	const gib = uint64(1 << 30)
	free, total := (8124*gib+9)/10, uint64(1863)*gib
	if strings.TrimPrefix(state, "off-") == "low" {
		free = (8*gib + 9) / 10
	}
	if state == "long" {
		free = (123456*gib + 9) / 10
		total = (234567*gib + 9) / 10
	}
	s.attachmentDiskCapacity = func() (attachments.Capacity, error) {
		if strings.TrimPrefix(state, "off-") == "unknown" {
			return attachments.Capacity{}, errors.New("synthetic-private-provider-detail")
		}
		return attachments.Capacity{Free: free, Total: total}, nil
	}
	return s
}

func TestAttachmentGiBFormatting(t *testing.T) {
	const gib = uint64(1 << 30)
	for _, tc := range []struct {
		bytes uint64
		want  string
	}{{0, "0.0"}, {999 * gib / 1000, "0.9"}, {gib, "1.0"}, {(123456*gib + 9) / 10, "12,345.6"}, {2*gib - 1, "1.9"}, {^uint64(0), "17,179,869,183.9"}} {
		if got := formatAttachmentGiB(tc.bytes); got != tc.want {
			t.Errorf("format(%d)=%s want%s", tc.bytes, got, tc.want)
		}
	}
}

func TestAttachmentDiskThreshold(t *testing.T) {
	s := userStore(t)
	for _, n := range []uint64{attachments.DiskFloorBytes - 1, attachments.DiskFloorBytes, attachments.DiskFloorBytes + 1} {
		s.attachmentDiskCapacity = func() (attachments.Capacity, error) { return attachments.Capacity{Free: n, Total: 2 << 30}, nil }
		v := s.attachmentDiskView()
		if !v.Known || v.Low != (n < attachments.DiskFloorBytes) {
			t.Fatal("disk threshold uses formatted value")
		}
	}
}

func TestAttachmentUIStates(t *testing.T) {
	for _, state := range []string{"off", "on", "low", "unknown", "long"} {
		t.Run(state, func(t *testing.T) {
			s := attachmentUIFixture(t, state)
			h := New(s, &ownedPanelBackend{}, http.NotFoundHandler())
			admin := loginPanelUser(t, h, "admin")
			w := admin.call("GET", "/admin/settings", nil)
			body := w.Body.String()
			badge, toggle, next := "ON", "끄기", "false"
			if state == "off" {
				badge, toggle, next = "OFF", "켜기", "true"
			}
			for _, want := range []string{`id="attachment-badge"`, `>` + badge + `</span>`, `메시지 파일 첨부 ` + toggle, `name="attachments" value="` + next + `"`, `100 MiB`, `업로드 후 30분`, `클라이언트 <strong>0.18 이상</strong>`} {
				if !strings.Contains(body, want) {
					t.Errorf("missing UI contract %s", want)
				}
			}
			want := map[string]string{"off": "파일 첨부가 꺼져 있습니다.", "on": "새 첨부를 받을 수 있습니다.", "low": "남은 공간이 1 GiB 미만이라 새 첨부를 받지 않습니다.", "unknown": "서버 디스크: 남은 공간을 확인할 수 없어 새 첨부를 받지 않습니다.", "long": "새 첨부를 받을 수 있습니다."}[state]
			if !strings.Contains(body, want) || strings.Contains(body, "synthetic-private-provider-detail") {
				t.Fatal("status/error disclosure")
			}
			if strings.Contains(body, `type="checkbox" name="attachments"`) {
				t.Fatal("WIP checkbox remains")
			}
			if state == "unknown" {
				if !strings.Contains(body, `id="attachment-free">확인 불가`) || !strings.Contains(body, `id="attachment-free-unit" hidden`) {
					t.Fatal("unknown disk units")
				}
			} else {
				free, total := "812.4", "1,863.0"
				if state == "low" {
					free = "0.8"
				}
				if state == "long" {
					free, total = "12,345.6", "23,456.7"
				}
				if !strings.Contains(body, `id="attachment-free">`+free) || !strings.Contains(body, `id="attachment-total">`+total) {
					t.Fatal("disk numbers")
				}
			}
			if state == "off" {
				if !strings.Contains(body, `id="attachment-history">설정 변경 이력이 없습니다.`) {
					t.Fatal("no history")
				}
			} else {
				if !strings.Contains(body, "OFF → ON") {
					t.Fatal("actual toggle missing")
				}
				old := body[strings.Index(body, `id="attachment-history"`):]
				old = old[:strings.Index(old, "</p>")]
				if err := s.SetAttachmentPolicy("admin", true); err != nil {
					t.Fatal("same-value write")
				}
				fresh := admin.call("GET", "/admin/settings", nil).Body.String()
				fresh = fresh[strings.Index(fresh, `id="attachment-history"`):]
				fresh = fresh[:strings.Index(fresh, "</p>")]
				if old != fresh {
					t.Fatal("same-value write invented transition/history")
				}
			}
		})
	}
}

func TestAttachmentOffDiskCopy(t *testing.T) {
	for _, state := range []string{"low", "unknown"} {
		t.Run(state, func(t *testing.T) {
			s := attachmentUIFixture(t, state)
			if err := s.SetAttachmentPolicy("admin", false); err != nil {
				t.Fatal("disable")
			}
			h := New(s, &ownedPanelBackend{}, http.NotFoundHandler())
			admin := loginPanelUser(t, h, "admin")
			body := admin.call("GET", "/admin/settings", nil).Body.String()
			if !strings.Contains(body, "첨부가 꺼져 있어 서버의 첨부 파일은 지워졌습니다. 메시지 전달은 계속됩니다.") {
				t.Error("OFF disk warning lacks deleted-file copy")
			}
			if strings.Contains(body, "이미 보관한 첨부의 다운로드와 메시지 전달은 계속됩니다.") {
				t.Error("OFF promises retained downloads")
			}
		})
	}
}
