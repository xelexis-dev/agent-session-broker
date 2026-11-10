package controlpanel

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if passwordKDF != productionPasswordKDF {
		panic("password KDF default differs from production")
	}
	passwordKDF = passwordKDFParameters{time: 1, memory: 64, threads: 1, keyLen: 32}
	os.Exit(m.Run())
}

func TestProductionPasswordKDFCompatibility(t *testing.T) {
	want := passwordKDFParameters{time: 2, memory: 19 * 1024, threads: 1, keyLen: 32}
	if productionPasswordKDF != want {
		t.Fatalf("stored-password KDF changed: got %+v, want %+v", productionPasswordKDF, want)
	}
}
