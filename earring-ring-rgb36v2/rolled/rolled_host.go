//go:build !baremetal

package rolled

// Loop does nothing on the host; see rolled.go.
func Loop() {}
