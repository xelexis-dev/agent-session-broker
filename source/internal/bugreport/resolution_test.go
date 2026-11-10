package bugreport

import (
	"encoding/json"
	"testing"
)

func TestResolutionStoredWithLegacyClosed(t *testing.T) {
	s, _ := fixtureStore(t)
	r, err := s.Create(fixtureInput(), reporter(1), "test")
	if err != nil {
		t.Fatal(err)
	}
	s.SetProjectPolicy(func(actor string) (map[string]string, func(), error) {
		return map[string]string{"project": reporter(1).ProjectGenerationID}, func() {}, nil
	})
	if _, _, err = s.SetResolution(r.ID, "resolved", "", Actor{UserID: "admin", Source: "panel"}, r.ExpectedVersion); err != nil {
		t.Fatal(err)
	}
	raw, err := s.db.Get(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		Status, Resolution string
		StatusChanges      []json.RawMessage
	}
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields.Status != "closed" || fields.Resolution != "resolved" || len(fields.StatusChanges) != 1 {
		t.Fatalf("resolved must persist legacy closed + resolution + one history entry; got %s/%s/%d", fields.Status, fields.Resolution, len(fields.StatusChanges))
	}
	var old struct {
		Status string `json:"status"`
	}
	if err = json.Unmarshal(raw, &old); err != nil || old.Status != "closed" {
		t.Fatal("0.17.2 reader must see closed")
	}
}
