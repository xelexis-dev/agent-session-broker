package codexcontrol

import "testing"

func TestQueueRecoveryParser(t *testing.T) {
	good := []byte(`{"data":[{"clientUserMessageId":"xb:R:E:1-1","input":[{"type":"text","text":"fixture"}]}],"nextCursor":null}`)
	p, e := ParseQueuePage(good)
	if e != nil || !p.Complete || p.Count != 1 {
		t.Fatal("matching queue entry unavailable")
	}
	p, e = ParseQueuePage([]byte(`{"data":[{"clientUserMessageId":"xb:R:E:1-1"}],"nextCursor":null}`))
	if e != nil || p.Count != 1 {
		t.Fatal("missing content became matchable")
	}
	for _, raw := range []string{`{}`, `{"data":[],"nextCursor":""}`, `{"data":[]}`, `{"data":[],"nextCursor":3}`, `{"data":null,"nextCursor":null}`} {
		if _, e = ParseQueuePage([]byte(raw)); e == nil {
			t.Fatal("incomplete queue accepted")
		}
	}
	p, e = ParseQueuePage([]byte(`{"data":[],"nextCursor":"more"}`))
	if e != nil || p.Complete || p.Cursor != "more" {
		t.Fatal("empty continuation ended queue")
	}
}
