//go:build !screamfeed

package main

// Stub half of the on-target decoder test; screamfeed.go (built with -tags
// screamfeed) holds the real thing.
func screamFeed() {}
