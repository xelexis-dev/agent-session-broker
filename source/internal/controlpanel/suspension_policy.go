package controlpanel

type suspensionKey struct {
	project, nickname string
}

type suspensionWatch struct {
	changed chan struct{}
	refs    int
}

// watchSuspension must run before reading the broker policy so a concurrent
// suspension cannot slip between the policy lookup and request admission.
// Watches are transient request state; the broker owns durable policy.
func (p *panel) watchSuspension(project, nickname string) (<-chan struct{}, func()) {
	if nickname == "" {
		return nil, func() {}
	}
	key := suspensionKey{project, nickname}
	p.mu.Lock()
	if p.suspensionWatches == nil {
		p.suspensionWatches = make(map[suspensionKey]*suspensionWatch)
	}
	watch := p.suspensionWatches[key]
	if watch == nil {
		watch = &suspensionWatch{changed: make(chan struct{})}
		p.suspensionWatches[key] = watch
	}
	watch.refs++
	p.mu.Unlock()
	return watch.changed, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		watch.refs--
		if watch.refs == 0 && p.suspensionWatches[key] == watch {
			delete(p.suspensionWatches, key)
		}
	}
}

func (p *panel) cancelSuspendedRequests(project, nickname string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := suspensionKey{project, nickname}
	if watch := p.suspensionWatches[key]; watch != nil {
		close(watch.changed)
		// New requests acquire their own generation and inspect current broker
		// policy. Releasing an old request cannot remove that newer generation.
		delete(p.suspensionWatches, key)
	}
}
