#!/usr/bin/env python3
"""Averaging that cannot stall, and a test that proves it.

Every offline tool in this directory compares two earrings through some smoothed
quantity, and every one of them implemented a rounded exponential moving average the
same way the firmware does:

    a += (x - a + (1 << (s - 1))) >> s

For a large accumulator that is fine. For a small one it is a deadband: with x - a
equal to 1 and s = 13, the increment is (1 + 4096) >> 13, which is zero. The
accumulator stops. It stops at whatever value it happened to reach, permanently, and
because the same is true slightly below zero, the state is stable in both directions.
The output looks like a heavily smoothed signal. It is a constant.

Two tools were bitten by this in one session. A high-pass filter of a band contrast
produced identical scores for 33 s, 131 s and 524 s time constants - which is not a
finding about music, it is the reference sitting frozen at its seed value. A slow
band average reported 100% agreement with one pattern reached and a hold time longer
than the recording, and that got read as "long smoothing makes the selector latch";
the real cause is the average stopping, so the selector had nothing to select on.
Both conclusions were wrong, and both were plausible enough to ship if nobody had
noticed that three different settings produced one identical number.

The fix is to carry fraction bits, exactly as the firmware now does: accumulate x << 8
so the difference the shift acts on stays large compared with 1, and take the integer
part when reading it out.

TEST: a quantity that creeps by one unit per frame must be tracked. A plain rounded
EMA at shift >= 2 fails this. If EMA_TEST fails, do not trust any number produced by
a tool that imports this file.
"""
import numpy as np

SHIFT = 8  # fraction bits carried by the accumulator


def ema(x, shift):
    """Rounded EMA of x with `shift` as the time constant, fraction bits carried.

    Returns the integer part, which is what the firmware's callers use.
    """
    out = np.empty(len(x), dtype=np.int64)
    a = int(x[0]) << SHIFT
    r = 1 << (shift - 1)
    for i, v in enumerate(x):
        a += ((int(v) << SHIFT) - a + r) >> shift
        out[i] = a >> SHIFT
    return out


def ema_ref(x, shift):
    """The stalling form, kept so the test can demonstrate it and so a future
    reader can see what not to write."""
    out = np.empty(len(x), dtype=np.int64)
    a = int(x[0])
    r = 1 << (shift - 1)
    for i, v in enumerate(x):
        a += (int(v) - a + r) >> shift
        out[i] = a
    return out


def ema_state(x, shift):
    """The accumulator itself, fraction bits included, for high-passing: the
    caller subtracts state >> SHIFT from the current value and keeps full
    resolution on both sides."""
    out = np.empty(len(x), dtype=np.int64)
    a = int(x[0]) << SHIFT
    r = 1 << (shift - 1)
    for i, v in enumerate(x):
        a += ((int(v) << SHIFT) - a + r) >> shift
        out[i] = a
    return out


def hipass(x, shift):
    """x minus its own slow mean, without losing the small differences the mean is
    meant to remove. Subtracting two integer parts throws away exactly the
    information a high-pass filter exists to keep."""
    a = ema_state(x, shift)
    return (np.asarray(x, dtype=np.int64) << SHIFT) - a


def self_test():
    """A step down must be tracked. This is the case that fails.

    A quantity sitting at a few thousand with a reference time constant of shift 11
    has a deadband of 2048 around it: the rounded increment (x - a + 1024) >> 11 is
    zero whenever the gap is under 3072, so the accumulator stops wherever it
    happens to be. Note the asymmetry - a gap upward rounds to +1 and creeps, a gap
    downward rounds to 0 and sticks - so the state is not merely stale, it is biased
    toward the highest value seen. This is the same defect the firmware's centroid
    cascade had and now avoids by carrying eight fraction bits.

    The second half checks the carried form is a correct average and not just a
    moving one, against the exact response to a ramp.
    """
    notes = []
    # Step down by 400 from 4000, with a shift whose deadband is 2048.
    sig = np.concatenate([np.full(3000, 4000),
                          np.full(3000, 3600)]).astype(np.int64)
    for shift in (11, 13):
        # Where an exact average would be after 3000 frames of a 400-unit step:
        # 3600 + 400*(1 - 2**-shift)**3000. Demanding 3600 would fail a correct
        # filter - the first version of this test did exactly that, and the number
        # it complained about was the right answer.
        want = 3600 + 400 * (1.0 - 2.0 ** -shift) ** 3000
        good = ema(sig, shift)[-1]
        bad = ema_ref(sig, shift)[-1]
        if abs(good - want) > 8:
            raise AssertionError(
                f"shift {shift}: carried form settled at {good}, exact {want:.0f}")
        stuck = "FROZEN at the old value" if abs(bad - 4000) < 8 else \
            f"{(4000 - bad) / 400 * 100:.0f}% of the way"
        notes.append(f"shift {shift:2d} step 4000->3600, exact {want:.0f}: "
                     f"plain settles at {bad} ({stuck}), carried at {good}")
    # Correctness: response to a unit ramp. An EMA chases a ramp with a steady lag
    # of (1-alpha)/alpha, so the answer is n - (2**shift - 1)*(1 - (1 - 2**-shift)**n).
    # The obvious-looking n*(1-(1-alpha)**n) is the response to a step, not a ramp,
    # and using it made a correct filter look broken at shift 12 by demanding 19849
    # where 15935 is right.
    n = 20000
    ramp = np.arange(n, dtype=np.int64)
    for shift in (8, 12):
        al = 2.0 ** -shift
        want = n - (1.0 - al) / al * (1.0 - (1.0 - al) ** n)
        got = ema(ramp, shift)[-1]
        if abs(got - want) > 4:
            raise AssertionError(
                f"shift {shift}: ramp response {got}, exact {want:.0f}")
        notes.append(f"shift {shift:2d} ramp of 20000: {got} vs exact "
                     f"{want:.0f} - correct")
    return "\n  ".join(notes)


if __name__ == "__main__":
    print("EMA self-test (step down through the deadband, then a ramp):")
    print("  " + self_test())
