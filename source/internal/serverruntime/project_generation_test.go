package serverruntime

import (
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
	"path/filepath"
	"testing"
)

func TestProjectGenerationReportStampUsesAuthenticatedServerContext(t *testing.T) {
	r, parent := newDeviceRuntime(t)
	request := protectedRequest(977, "reporter")
	credential := r.authorize(parent, request, "reporter")
	registered := managedTool[broker.RegisterResult](t, r.bound(credential), "register_session", request)
	if registered.Code != "" {
		t.Fatal("fixture registration failed")
	}
	result := managedTool[struct{ ID string }](t, r.bound(credential), "report_bug", bugreport.Input{Title: "generation stamp", Summary: "fixture"})
	if r.close() != nil {
		t.Fatal("fixture close failed")
	}
	control, err := controlpanel.OpenSQLite(filepath.Join(r.dir, "control"))
	if err != nil {
		t.Fatal(err)
	}
	generation := control.ProjectGeneration("test")
	control.Close()
	db, err := sqlitestore.Open(filepath.Join(r.dir, "bugreports"), "bugreports.db")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := db.Get(result.ID)
	if err != nil {
		t.Fatal("report record missing")
	}
	db.Close()
	var report bugreport.Report
	if json.Unmarshal(raw, &report) != nil || generation == "" || report.Reporter.ProjectGenerationID != generation || report.Reporter.Project != "test" {
		t.Fatal("authenticated project lifetime not stamped")
	}
	r.start()
}
