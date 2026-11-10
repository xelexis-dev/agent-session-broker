package controlpanel

// commitGuard runs with Store.mu held, just before a write; a non-nil error aborts the write.
type commitGuard func() error
