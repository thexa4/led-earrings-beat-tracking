//go:build !syncboot

package main

// Stub half of the sync bench build; syncboot.go (-tags syncboot) holds it.
func syncBoot() {}

func syncProbe() {}
