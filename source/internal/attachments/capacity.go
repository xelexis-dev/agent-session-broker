package attachments

// Capacity describes space available to this server on the spool volume.
type Capacity struct{ Free, Total uint64 }

func (s *Store) DiskCapacity() (Capacity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.diskCapacityLocked()
}

func (s *Store) diskCapacityLocked() (Capacity, error) {
	if s.closed || s.pin.Validate() != nil {
		return Capacity{}, problem(507, "server-disk-low")
	}
	file, err := s.root.Open(".")
	if err != nil {
		return Capacity{}, problem(507, "server-disk-low")
	}
	defer file.Close()
	capacity, err := s.capacity(file)
	if err != nil || capacity.Free > capacity.Total {
		return Capacity{}, problem(507, "server-disk-low")
	}
	return capacity, nil
}
