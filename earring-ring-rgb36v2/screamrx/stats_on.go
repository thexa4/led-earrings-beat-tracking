//go:build !baremetal || screamtelem || screamfeed

package screamrx

// keepStats is whether the decoder counts Stats: in the tests and tools, and
// in the firmware builds that report them. The plain firmware never reads
// them, and the counting costs ~150 B of flash.
const keepStats = true
