package controlpanel

import "testing"

func TestSuspensionWatchesReleaseReferencesAndKeepNewGeneration(t *testing.T) {
	p := &panel{}
	first, releaseFirst := p.watchSuspension("one", "shared-name")
	second, releaseSecond := p.watchSuspension("one", "shared-name")
	other, releaseOther := p.watchSuspension("two", "shared-name")
	releaseFirst()
	if len(p.suspensionWatches) != 2 {
		t.Fatal("first completion removed another active receiver's watch")
	}
	p.cancelSuspendedRequests("one", "shared-name")
	for _, stopped := range []<-chan struct{}{first, second} {
		select {
		case <-stopped:
		default:
			t.Fatal("matching receiver not notified")
		}
	}
	select {
	case <-other:
		t.Fatal("another project's receiver was cancelled")
	default:
	}
	fresh, releaseFresh := p.watchSuspension("one", "shared-name")
	releaseSecond()
	if len(p.suspensionWatches) != 2 {
		t.Fatal("old receiver completion removed new generation")
	}
	select {
	case <-fresh:
		t.Fatal("new request inherited old suspension")
	default:
	}
	p.cancelSuspendedRequests("one", "shared-name")
	select {
	case <-fresh:
	default:
		t.Fatal("replacement watch was not retained")
	}
	releaseFresh()
	releaseOther()
	if len(p.suspensionWatches) != 0 {
		t.Fatal("idle suspension watches leaked")
	}
	unnamed, releaseUnnamed := p.watchSuspension("one", "")
	releaseUnnamed()
	if unnamed != nil || len(p.suspensionWatches) != 0 {
		t.Fatal("unbound request allocated a suspension watch")
	}
}
