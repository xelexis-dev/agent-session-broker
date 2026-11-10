package serverruntime

import (
	"testing"
)

func TestKimiPanelProjectsReadinessWithoutChangingIngress(t *testing.T) {
	adapter, remote, _ := panelCollisionFixture(t)
	req := protectedRequest(3, "kimi-panel")
	req.Harness = "kimi"
	registered := protectedRegistered(t, remote, req)
	if !registered.AutomaticDeliveryReady {
		t.Fatal("Kimi internal readiness changed")
	}
	for _, live := range []bool{false, true} {
		read := adapter.Sessions
		if live {
			read = adapter.LiveSessions
		}
		rows, err := read(t.Context(), "test")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if row.RegistrationID == registered.RegistrationID {
				found = true
				if row.Ready {
					t.Fatal("panel must not claim Kimi automatic delivery readiness")
				}
			} else if !row.Ready {
				t.Fatal("Kimi projection changed other panel readiness")
			}
		}
		if !found {
			t.Fatal("Kimi panel row missing")
		}
	}
	internal, err := remote.GetSession(t.Context(), registered.RegistrationID)
	if err != nil || !internal.AutomaticDeliveryReady {
		t.Fatal("panel changed internal readiness")
	}
}
