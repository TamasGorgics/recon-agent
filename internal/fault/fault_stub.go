//go:build !faultinject

package fault

// Hit is a no-op outside the faultinject build.
func Hit(_ string) {}
