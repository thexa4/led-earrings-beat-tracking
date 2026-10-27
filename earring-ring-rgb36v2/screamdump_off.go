//go:build !screamdump

package main

// Stub half of the scream recorder; screamdump.go (built with -tags
// screamdump) holds the real thing.
func screamDump() {}
