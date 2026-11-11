package controlpanel

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
)

func TestInstanceResultsTemplateLabelsAndSummary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		complete bool
		message  string
	}{
		{"none", false, ""}, {"message", false, "공지 접수 처리 중입니다."}, {"empty", true, ""}, {"complete", true, "접수 처리 완료"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			v := page{Form: url.Values{}, AnnouncementMessage: tc.message, AnnouncementComplete: tc.complete, AnnouncementAccepted: 10, AnnouncementFailed: 2}
			if err := pages.ExecuteTemplate(&out, "admin-settings", v); err != nil {
				t.Fatal(err)
			}
			body := out.String()
			if tc.complete {
				for _, text := range []string{`aria-labelledby="instance-results-title"`, `id="instance-results-title"`, `<td colspan="3">` + en("admin.announce.no_targets") + `</td>`} {
					if !strings.Contains(body, text) {
						t.Fatalf("missing result markup %s", text)
					}
				}
				if strings.Count(body, en("admin.announce.counts", "accepted", 10, "failed", 2)) != 1 {
					t.Fatal("result counts must render exactly once")
				}
			} else if tc.message != "" {
				if !strings.Contains(body, `aria-label="`+en("admin.announce.results_aria")+`"`) || strings.Contains(body, `aria-labelledby="instance-results-title"`) {
					t.Fatal("message-only accessible label missing or dangling")
				}
			} else if strings.Contains(body, `class="card instance-results"`) {
				t.Fatal("empty results card rendered")
			}
		})
	}
}
