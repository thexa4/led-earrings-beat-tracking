package syncdsp

// Golden reference values, produced by the host test
// (UPDATE_GOLDEN=1 go test ./syncdsp -run TestGolden) and checked:
//
//   - by that test, to catch accidental changes to the DSP or the hash;
//   - by the device boot selftest, to verify the pipeline running on the
//     STM32L031 produces exactly what the host produces.
//
// The pipeline is plain integer Go with no platform-dependent arithmetic (see
// the package comment), so this is a genuine end-to-end check of the device
// build: the same source compiles both sides, and a mismatch means the device's
// view of the world differs - a wrong filter coefficient, a different ADC range,
// or an int width surprise.
//
// Input: the deterministic Synth() stimulus from sample 0, undistorted, into a
// fresh Pipeline.

var GoldenHashes = [64]uint32{
	0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff,
	0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff,
	0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff,
	0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff,
	0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff,
	0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002700a0ff, 0x002300a0ff,
	0x002700a0ff, 0x002300a0ff, 0x002300a0ff, 0x002300a0ff, 0x002300a0ff, 0x002300a0ff, 0x002300a0ff, 0x002300a0ff,
	0x002300a0ff, 0x002300a0ff, 0x002300a0ff, 0x002300a0ff, 0x002300a0ff, 0x002300a0ff, 0x002300a0ff, 0x002300a0ff,
}
var GoldenAnchors uint64 = 0x0000000000000000
var GoldenPatterns = [64]int8{
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
}
