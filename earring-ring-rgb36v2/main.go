package main

// State: current mode, sync red/blue swap, variant per mode, 4 hash bytes
//
//go:align 4 // flashWrite reinterprets this as words
var storedState [32]uint8

const (
	stateOffsetMode       = 0
	stateOffsetSyncSwapRB = 1 // nonzero: sync mode shows red and blue swapped
	stateOffsetVariants   = 2
)

// Saved variant for each mode (so that the variant is kept between mode
// switches).
var modeVariants = storedState[stateOffsetVariants : stateOffsetVariants+modeLast]

func main() {
	initHardware()
	screamDump()      // -tags screamdump only: never returns
	screamFeed()      // -tags screamfeed only: never returns
	screamTelem()     // -tags screamtelem only: never returns
	syncBoot()        // -tags syncboot only: start in sync mode
	syncShareReset(0) // the sync data block reads "not running" until sync starts

	// Zero all LEDs.
	clearLEDs()

	index := 0 // 0..11, group of 3 LEDs that will be updated together
	frame := 0
	mode := int(storedState[stateOffsetMode])
	// State saved by older firmware may contain a now invalid mode (modes
	// have been removed and added).
	if mode >= modeLast && mode != modeTest && mode != modeQA {
		mode = 0
		storedState[stateOffsetMode] = 0
	}
	variant := 0
	if mode < len(modeVariants) {
		variant = int(modeVariants[mode])
	}
	startModeAudio(mode)
	initMode(mode)
	modeFramesPressed := 0
	previousMode := 0
	variantBtnFramesPressed := 0
	for {
		led0 := animate(mode, variant, index+0, frame)
		led1 := animate(mode, variant, index+12, frame)
		led2 := animate(mode, variant, index+24, frame)

		// Read the ADC, intentionally done in between so that the LED power
		// won't interfere too much with the microphone.
		if syncRunning {
			syncPoll()
		} else if animationNeedsMic(mode) {
			updateMic(index)
		}

		// Update 3 LEDs at a time, since that's convenient for the
		// RGB-to-bitplane conversion, and bitbang all of them.
		refreshLEDs(index, uint32(led0), uint32(led1), uint32(led2))
		benchPass() // -tags passcount only

		index++
		if index == 12 {
			index = 0
			if syncRunning && syncAnimFrame>>1 == syncMainFrame {
				// At 16 MHz twelve passes take ~5 ms instead of ~50: in
				// sync mode a frame only ends with every other analysis
				// frame (32 per second, near the other modes' 20-27), or a
				// 150 ms press would be a long one and switch the earring
				// off.
				continue
			}
			syncMainFrame = syncAnimFrame >> 1
			frame++

			// Do per animation process, such as processing audio samples
			// collected during the previous frame.
			newFrame(mode, variant, frame)

			// Read the mode button every frame update.
			modePressed := button1Pressed()
			if modeFramesPressed == 30 {
				// Always disable the microphone when sleeping.
				disableMic()
				stopSync()

				turnOffAnimation(mode, variant, frame)

				// Sleep until the mode button is pressed.
				sleepUntilButtonPress()

				// To continue the startup animation, set the mode to "power
				// on".
				previousMode = mode
				mode = modePowerOn
				frame = 0
				modeFramesPressed = -0x8000_0000 // don't switch to the next animation on button release
			}
			if mode == modePowerOn {
				if frame == numLEDs/2 {
					mode = previousMode

					// Woke up again, so start up interfaces.
					startModeAudio(mode)
				}
			} else {
				if !modePressed && modeFramesPressed > 0 {
					// Move to the next mode.
					mode++
					if mode >= modeLast {
						// Last, so wrap around.
						mode = 0
					}
					variant = int(modeVariants[mode])

					// Save the current state to flash.
					storedState[stateOffsetMode] = uint8(mode)
					saveState()

					// Clear LEDs before moving on to the next mode.
					clearLEDs()

					// Leave whatever audio setup the previous mode had, then
					// configure for the new mode. Disabling both (each is a
					// no-op unless active) keeps this correct for every
					// transition, including sync <-> mic modes: the mic modes
					// leave the ADC on PCLK with low-frequency mode set,
					// which would silently corrupt the sync capture config.
					stopSync()
					disableMic()

					// Only enable the microphone when needed.
					startModeAudio(mode)

					initMode(mode)
				}

				// Switch to the next animation variant when the variant button
				// is released.
				variantBtnPressed := button2Pressed()
				if !variantBtnPressed && variantBtnFramesPressed > 0 {
					newVariant := animationNextVariant(mode, variant)
					if newVariant != variant {
						variant = newVariant
						if mode < len(modeVariants) {
							modeVariants[mode] = uint8(variant)
						}
						saveState()
					}
				}
				if variantBtnPressed {
					variantBtnFramesPressed++
				} else {
					variantBtnFramesPressed = 0
				}
				if variantBtnFramesPressed == 30 {
					slot := mode - modeCustom0
					if slot >= 0 && slot < 3 {
						// Long press is to receive data, not to switch to the
						// next variant.
						variantBtnFramesPressed = 0

						// Receive the data, and write it to flash.
						dataRecv(slot)

						// Immediately start running this pattern.
						customLoadPattern(slot, false)

						// Reset variant to 0, since the newly received one
						// might have fewer variants than the one previously
						// loaded.
						variant = 0
						modeVariants[mode] = uint8(variant)
						saveState()
					} else if mode == modeSync {
						// Long press in sync mode swaps red and blue.
						storedState[stateOffsetSyncSwapRB] ^= 1
						saveState()

						// Still held: no second toggle at 60 frames, and no
						// variant step on release.
						variantBtnFramesPressed = -0x8000_0000
					}
				}
			}
			if modePressed {
				modeFramesPressed++
			} else {
				modeFramesPressed = 0
			}
		}
	}
}

// startModeAudio starts whatever audio input mode needs: the sync pipeline or
// the microphone (or nothing).
//
//go:noinline
func startModeAudio(mode int) {
	if mode == modeSync {
		startSync()
	} else if animationNeedsMic(mode) {
		enableMic()
	}
}

// Show an animation during shutdown. It keeps the current animation, but
// freezes it in place, and turns off LEDs in sequence.
func turnOffAnimation(mode, variant, frame int) {
	for i := range 36 / 3 {
		for index := range 12 {
			// Shut down LEDs in groups of 3.
			// We have to calculate the animation for two reasons:
			//  1. The animation may change in brightness if we don't call
			//     animate() to slow it down the same way as the normal
			//     animation.
			//  2. The 3 LEDs we can update at a time are spread over the ring
			//     of LEDs, not sequentially next to each other.
			led0 := animate(mode, variant, index+0, frame)
			if index+0 < i*3 {
				led0 = 0
			}
			led1 := animate(mode, variant, index+12, frame)
			if index+12 < i*3 {
				led1 = 0
			}
			led2 := animate(mode, variant, index+24, frame)
			if index+24 < i*3 {
				led2 = 0
			}
			refreshLEDs(index, uint32(led0), uint32(led1), uint32(led2))
		}
	}
}
