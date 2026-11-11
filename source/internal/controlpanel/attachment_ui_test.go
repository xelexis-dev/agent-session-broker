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
			badge, toggle, next := "ON", en("admin.attach.turn_off_aria"), "false"
			if state == "off" {
				badge, toggle, next = "OFF", en("admin.attach.turn_on_aria"), "true"
			}
			for _, want := range []string{`id="attachment-badge"`, `>` + badge + `</span>`, `aria-label="` + toggle + `"`, `name="attachments" value="` + next + `"`, `100 MiB`, en("admin.attach.rule_expiry_4", "n", 30), en("admin.attach.rule_client_1") + `<strong>` + en("admin.attach.rule_client_2") + `</strong>`} {
				if !strings.Contains(body, want) {
					t.Errorf("missing UI contract %s", want)
				}
			}
			want := map[string]string{"off": enHTML("admin.attach.status_off"), "on": enHTML("admin.attach.status_on"), "low": enHTML("admin.attach.status_low"), "unknown": enHTML("admin.attach.status_unknown"), "long": enHTML("admin.attach.status_on")}[state]
			if !strings.Contains(body, want) || strings.Contains(body, "synthetic-private-provider-detail") {
				t.Fatal("status/error disclosure")
			}
			if strings.Contains(body, `type="checkbox" name="attachments"`) {
				t.Fatal("WIP checkbox remains")
			}
			if state == "unknown" {
				if !strings.Contains(body, `id="attachment-free">`+en("admin.attach.disk_unknown")) || !strings.Contains(body, `id="attachment-free-unit" hidden`) {
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
				if !strings.Contains(body, `id="attachment-history">`+en("admin.no_changes")) {
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
			if !strings.Contains(body, enHTML("admin.attach.detail_off_full")) {
				t.Error("OFF disk warning lacks deleted-file copy")
			}
			if strings.Contains(body, enHTML("admin.attach.detail_full")) {
				t.Error("OFF promises retained downloads")
			}
		})
	}
}
