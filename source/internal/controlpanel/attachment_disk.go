package controlpanel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/attachments"
	"strconv"
)

type attachmentDiskView struct {
	Known, Low  bool
	Free, Total string
}

func formatAttachmentGiB(bytes uint64) string {
	const gib = uint64(1 << 30)
	integer := strconv.FormatUint(bytes/gib, 10)
	for i := len(integer) - 3; i > 0; i -= 3 {
		integer = integer[:i] + "," + integer[i:]
	}
	return integer + "." + strconv.FormatUint((bytes%gib)*10/gib, 10)
}

func (s *Store) attachmentDiskView() attachmentDiskView {
	s.mu.RLock()
	provider := s.attachmentDiskCapacity
	s.mu.RUnlock()
	if provider == nil {
		return attachmentDiskView{}
	}
	capacity, err := provider()
	if err != nil || capacity.Free > capacity.Total {
		return attachmentDiskView{}
	}
	return attachmentDiskView{Known: true, Low: capacity.Free < attachments.DiskFloorBytes, Free: formatAttachmentGiB(capacity.Free), Total: formatAttachmentGiB(capacity.Total)}
}
