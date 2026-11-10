package broker

import (
	"testing"
	"time"
)

func TestRegistrationStoreCloseRejectsFurtherIO(t *testing.T) {
	service, err := newRemoteService(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.store.save(&registrationRecord{RegistrationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA"}); err == nil {
		t.Fatal("closed store accepted save")
	}
	if _, err := service.store.loadAll(time.Now()); err == nil {
		t.Fatal("closed store accepted load")
	}
}
