package broker

import (
	"encoding/json"
	"io/fs"
)

// The service lock covers both the record copies and the store write.
func (s *RemoteService) flushLastSeenLocked() error {
	records := make([]registrationRecord, 0)
	for _, e := range s.byRegistration {
		if e.dirty {
			records = append(records, *e.record)
		}
	}
	if len(records) > 0 && s.store != nil {
		if err := s.store.saveBatch(records); err != nil {
			return err
		}
	}
	for _, r := range records {
		s.byRegistration[r.RegistrationID].dirty = false
	}
	s.lastFlush = s.now()
	return nil
}

func (st *registrationStore) saveBatch(records []registrationRecord) error {
	if st.closed {
		return fs.ErrClosed
	}
	if st.database == nil || st.writeFile != nil {
		for i := range records {
			if err := st.save(&records[i]); err != nil {
				return err
			}
		}
		return nil
	}
	puts := make(map[string][]byte, len(records))
	for _, r := range records {
		data, err := json.MarshalIndent(r, "", " ")
		if err != nil {
			return err
		}
		puts[r.RegistrationID] = data
	}
	if st.writeBatch != nil {
		return st.writeBatch(puts)
	}
	return st.database.Batch(puts, nil)
}
