// covers: FIN-52
package metrics

import "testing"

// TestIntegrityObserverSeedAndMapping pins the frozen 0/1/2 contract: the
// registry seeds unknown/not-run 0, a passed completed run sets 1, and a failed
// completed run sets 2. The adapter never sets 0 itself.
func TestIntegrityObserverSeedAndMapping(t *testing.T) {
	m := mustNew(t, Options{Component: "worker"})
	observer := NewIntegrityObserver(m)

	family := mustFamily(t, gatherViews(t, m.Registry()), MetricIntegrityJobStatus)
	series := mustSeries(t, family, map[string]string{"job": JobIntegrity})
	if got := gaugeValue(t, series); got != 0 {
		t.Fatalf("seed integrity gauge = %v, want 0 (unknown/not run)", got)
	}

	observer.IntegrityRunCompleted(true)
	family = mustFamily(t, gatherViews(t, m.Registry()), MetricIntegrityJobStatus)
	series = mustSeries(t, family, map[string]string{"job": JobIntegrity})
	if got := gaugeValue(t, series); got != 1 {
		t.Fatalf("passed integrity gauge = %v, want 1", got)
	}

	observer.IntegrityRunCompleted(false)
	family = mustFamily(t, gatherViews(t, m.Registry()), MetricIntegrityJobStatus)
	series = mustSeries(t, family, map[string]string{"job": JobIntegrity})
	if got := gaugeValue(t, series); got != 2 {
		t.Fatalf("failed integrity gauge = %v, want 2", got)
	}
	if got := len(mustFamily(t, gatherViews(t, m.Registry()), MetricIntegrityJobStatus).series); got != 1 {
		t.Fatalf("integrity family series = %d, want only {job=integrity}", got)
	}
}

// TestIntegrityObserverNilSafe proves a nil registry or nil adapter is a no-op,
// so a binary can construct the observer before metrics initialization.
func TestIntegrityObserverNilSafe(t *testing.T) {
	(*IntegrityObserver)(nil).IntegrityRunCompleted(true)
	NewIntegrityObserver(nil).IntegrityRunCompleted(false)
}
