//go:build !screamtelem

package main

// Stub half of the receiver telemetry build; screamtelem.go (built with -tags
// screamtelem) holds the real thing.
func screamTelem() {}

func telemWindow() {}
