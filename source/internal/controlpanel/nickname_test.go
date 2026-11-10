package controlpanel

import (
	"errors"
	"testing"
)

func TestNicknameJournalRecoversAcrossRestart(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "registration-write-failed", true: "policy-finalize-failed"}[applied], func(t *testing.T) {
			dir := t.TempDir()
			s, err := OpenSQLite(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if err = s.Setup(testPassword); err != nil {
				t.Fatal(err)
			}
			if err = s.CreateProject("test"); err != nil {
				t.Fatal(err)
			}
			raw, err := s.IssueToken("test", "before")
			if err != nil {
				t.Fatal(err)
			}
			g, _, ok := s.authorize("test", raw)
			if !ok || g.Claim("identity") != nil {
				t.Fatal("cannot claim fixture")
			}
			write := s.write
			n := 0
			s.write = func(data []byte) error {
				n++
				if n == 2 {
					return errors.New("policy disk unavailable")
				}
				return write(data)
			}
			err = g.Rename("identity", "before", "after", func() error {
				if !applied {
					return errors.New("registration disk unavailable")
				}
				return nil
			})
			if err == nil {
				t.Fatal("injected failure hidden")
			}
			s.write = write
			if _, err = s.IssueToken("test", "after"); err == nil {
				t.Fatal("pending name released before reconciliation")
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = OpenSQLite(dir)
			if err != nil {
				t.Fatal(err)
			}
			g, _, ok = s.authorize("test", raw)
			if !ok {
				t.Fatal("credential lost on restart")
			}
			current, free := "before", "after"
			if applied {
				current, free = "after", "before"
			}
			if err = g.CheckName("intruder", current); err == nil {
				t.Fatal("other identity recovered journal")
			}
			if err = g.CheckName("identity", current); err != nil {
				t.Fatal(err)
			}
			if _, err = s.IssueToken("test", free); err != nil {
				t.Fatalf("unused name stayed reserved: %v", err)
			}
			if err = g.CheckName("identity", free); err == nil {
				t.Fatal("old name retained authorization")
			}
		})
	}
}

func TestNicknameReservationFailureDoesNotApply(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateProject("test"); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueToken("test", "before")
	if err != nil {
		t.Fatal(err)
	}
	g, _, _ := s.authorize("test", raw)
	if err = g.Claim("identity"); err != nil {
		t.Fatal(err)
	}
	write := s.write
	s.write = func([]byte) error { return errors.New("disk unavailable") }
	applied := false
	if err = g.Rename("identity", "before", "after", func() error { applied = true; return nil }); err == nil || applied {
		t.Fatal("registration changed without durable reservation")
	}
	s.write = write
	if err = g.CheckName("identity", "before"); err != nil {
		t.Fatal(err)
	}
}
