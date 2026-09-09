#!/usr/bin/env python3
"""Unit tests for cpuwork.py.

Standard library only, like the module under test. The dev virtualenv has no
Flask, so nothing here may reach server.py.

Timing tests run on a shared machine that may itself be busy. Every tolerance
below states what it is protecting against; none of them is a performance
target, and a bound wide enough to hide a real regression is a bug in the test.

Run: python3 examples/ping-demo/app/test_cpuwork.py
"""

import importlib.util
import inspect
import threading
import time
import unittest
from pathlib import Path

_SPEC = importlib.util.spec_from_file_location(
    "cpuwork", Path(__file__).resolve().parent / "cpuwork.py"
)
cpuwork = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(cpuwork)

# server.py PROFILES: the fastest response any profile asks for, and the
# slowest. Several bounds below only mean something relative to these.
FASTEST_TARGET_SECONDS = 0.005
SLOWEST_TARGET_SECONDS = 2.00

# The callers' requests.post(timeout=...) in server.py.
CALLER_TIMEOUT_SECONDS = 5.0


def measure_chunk_cpu_seconds(rounds=200):
    """CPU cost of one loop chunk on this machine.

    Tolerances derived from a measurement beat a hardcoded millisecond count,
    which would be wrong on the next machine. See the guard test below for why
    this cannot silently widen the assertions that use it.
    """
    cpuwork.burn_chunk()  # first call pays for OpenSSL setup

    start = time.thread_time()
    for _ in range(rounds):
        cpuwork.burn_chunk()
    return (time.thread_time() - start) / rounds


class SpendCpuTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.chunk_cpu = measure_chunk_cpu_seconds()

    def test_chunk_is_a_small_fraction_of_the_smallest_real_budget(self):
        # Tests below express their tolerance in chunks, so a chunk that grew
        # large would widen all of them at once. It also has to stay well under
        # the fastest response the app asks for, or its overshoot alone would
        # distort the smallest budgets.
        self.assertLess(self.chunk_cpu, FASTEST_TARGET_SECONDS / 10)

    def test_consumed_cpu_tracks_a_50ms_budget(self):
        budget = 0.05
        spend = cpuwork.spend_cpu(budget)

        self.assertFalse(spend.capped)
        self.assertGreaterEqual(spend.cpu_seconds, budget)
        # The loop can only overshoot by the chunk it was inside when the budget
        # ran out. 3x that absorbs a chunk running slow under cache pressure.
        self.assertLess(spend.cpu_seconds, budget + 3 * self.chunk_cpu)

    def test_zero_budget_does_nothing(self):
        spend = cpuwork.spend_cpu(0.0)

        self.assertEqual(spend.cpu_seconds, 0.0)
        self.assertEqual(spend.wall_seconds, 0.0)
        self.assertFalse(spend.capped)

    def test_negative_budget_does_nothing(self):
        spend = cpuwork.spend_cpu(-1.0)

        self.assertEqual(spend.cpu_seconds, 0.0)
        self.assertEqual(spend.wall_seconds, 0.0)
        self.assertFalse(spend.capped)

    def test_wall_time_tracks_cpu_time_when_uncontended(self):
        spend = cpuwork.spend_cpu(0.1)

        # R1 rests on this. If the compute burned wall time it had not spent on
        # CPU, adding a share would lengthen an uncontended response instead of
        # leaving it alone. The bound is loose because a busy machine steals
        # wall time from this thread; a primitive that slept or blocked on a
        # lock would still blow far past it.
        self.assertLess(spend.wall_seconds, spend.cpu_seconds * 1.5 + 0.01)
        self.assertGreater(spend.wall_seconds, spend.cpu_seconds * 0.9)

    def test_wall_cap_stops_a_budget_it_cannot_reach(self):
        cap = 0.05
        unreachable_budget = 10.0

        spend = cpuwork.spend_cpu(unreachable_budget, wall_cap_seconds=cap)

        self.assertTrue(spend.capped)
        self.assertLess(spend.cpu_seconds, unreachable_budget)
        self.assertGreaterEqual(spend.wall_seconds, cap)
        # One chunk of overshoot, plus room for the scheduler to take the thread
        # off CPU in the middle of that chunk.
        self.assertLess(spend.wall_seconds, cap + 0.05)

    def test_two_threads_do_not_serialise(self):
        budget = 0.15

        single = cpuwork.spend_cpu(budget)
        self.assertFalse(single.capped)

        spends = {}

        def run(key):
            spends[key] = cpuwork.spend_cpu(budget)

        threads = [threading.Thread(target=run, args=(i,)) for i in range(2)]
        start = time.perf_counter()
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        concurrent_wall = time.perf_counter() - start

        # Without this, a thread that gave up early would make the timing pass
        # for the wrong reason.
        for spend in spends.values():
            self.assertFalse(spend.capped)
            self.assertGreaterEqual(spend.cpu_seconds, budget)

        # R3: the primitive releases the GIL, so two threads run on two cores
        # and finish in about one thread's wall time. A GIL-holding burn takes
        # ~2x. The 1.6x line sits between those two outcomes -- it is a
        # discriminator, not a performance target.
        self.assertLess(concurrent_wall, single.wall_seconds * 1.6)

    def test_overshoot_does_not_grow_with_the_budget(self):
        budget = 0.5

        spend = cpuwork.spend_cpu(budget)

        self.assertGreaterEqual(spend.cpu_seconds, budget)
        self.assertLess(spend.cpu_seconds, budget * 1.02)
        # The real invariant: overshoot is bounded by one chunk, so a 10x larger
        # budget must not overshoot 10x more. Same absolute bound as the 50 ms
        # case above.
        self.assertLess(spend.cpu_seconds - budget, 3 * self.chunk_cpu)

    def test_spend_reports_both_clocks(self):
        # A later unit logs both numbers per request; their ratio is the
        # contention signal.
        spend = cpuwork.spend_cpu(0.02)

        self.assertGreater(spend.cpu_seconds, 0.0)
        self.assertGreater(spend.wall_seconds, 0.0)

    def test_default_wall_cap_sits_between_the_targets_and_the_caller_timeout(self):
        # A cap near the target band silently becomes the wall-clock deadline
        # this module exists to avoid, cancelling the fault. A cap past the
        # callers' timeout makes the fault non-monotone: abandoned requests are
        # recorded non-2xx and drop out of the successful-response percentile,
        # so a stronger fault would report a smaller p99.
        self.assertGreater(cpuwork.DEFAULT_WALL_CAP_SECONDS, SLOWEST_TARGET_SECONDS)
        self.assertLess(cpuwork.DEFAULT_WALL_CAP_SECONDS, CALLER_TIMEOUT_SECONDS - 1.0)


class SplitDurationTest(unittest.TestCase):
    def test_share_unset_sends_the_whole_target_to_sleep(self):
        # R2: every host that does not set a share behaves exactly as before.
        for split in (cpuwork.split_duration(0.25), cpuwork.split_duration(0.25, None)):
            self.assertEqual(split.cpu_budget_seconds, 0.0)
            self.assertEqual(split.sleep_seconds, 0.25)

    def test_full_share_sends_the_whole_target_to_compute(self):
        split = cpuwork.split_duration(0.25, 1.0)

        self.assertEqual(split.cpu_budget_seconds, 0.25)
        self.assertEqual(split.sleep_seconds, 0.0)

    def test_half_share_splits_evenly(self):
        split = cpuwork.split_duration(0.4, 0.5)

        self.assertAlmostEqual(split.cpu_budget_seconds, 0.2)
        self.assertAlmostEqual(split.sleep_seconds, 0.2)
        self.assertAlmostEqual(split.cpu_budget_seconds + split.sleep_seconds, 0.4)

    def test_split_preserves_the_target_for_every_share(self):
        # R1: an uncontended host's total response duration must not move with
        # the share, so the two halves always add back up to the target.
        for share in (0.0, 0.1, 0.25, 0.5, 0.75, 0.9, 1.0):
            for target in (FASTEST_TARGET_SECONDS, 0.05, 0.4, SLOWEST_TARGET_SECONDS):
                with self.subTest(share=share, target=target):
                    split = cpuwork.split_duration(target, share)
                    self.assertAlmostEqual(
                        split.cpu_budget_seconds + split.sleep_seconds, target
                    )

    def test_share_above_one_clamps_to_full_compute(self):
        split = cpuwork.split_duration(0.2, 1.5)

        self.assertAlmostEqual(split.cpu_budget_seconds, 0.2)
        self.assertEqual(split.sleep_seconds, 0.0)

    def test_junk_and_negative_shares_resolve_to_zero(self):
        # The share reaches the app as an environment variable, so a typo must
        # leave the host as it was -- never crash it, and never send it to full
        # CPU by accident. NaN and inf matter because min/max would silently
        # clamp them to 1.0.
        junk = (-0.5, "abc", "", None, object(), float("nan"), float("inf"),
                float("-inf"), [0.5])

        for value in junk:
            with self.subTest(share=value):
                split = cpuwork.split_duration(0.2, value)
                self.assertEqual(split.cpu_budget_seconds, 0.0)
                self.assertEqual(split.sleep_seconds, 0.2)

    def test_numeric_string_share_is_honoured(self):
        # U2 reads the share from the environment, so it arrives as a string.
        split = cpuwork.split_duration(0.2, "0.5")

        self.assertAlmostEqual(split.cpu_budget_seconds, 0.1)
        self.assertAlmostEqual(split.sleep_seconds, 0.1)

    def test_negative_target_yields_no_work_and_no_sleep(self):
        # time.sleep() raises on a negative argument.
        split = cpuwork.split_duration(-1.0, 0.5)

        self.assertEqual(split.cpu_budget_seconds, 0.0)
        self.assertEqual(split.sleep_seconds, 0.0)

    def test_sleep_cannot_depend_on_how_long_compute_ran(self):
        # Sleeping `target - elapsed` would re-impose a wall-clock deadline
        # through the back door and cancel the fault exactly. Lock the
        # parameter list so no elapsed time can ever be handed in.
        self.assertEqual(
            list(inspect.signature(cpuwork.split_duration).parameters),
            ["target_seconds", "cpu_share"],
        )

        before = cpuwork.split_duration(0.2, 0.5)
        cpuwork.spend_cpu(0.05)
        self.assertEqual(cpuwork.split_duration(0.2, 0.5), before)


if __name__ == "__main__":
    unittest.main()
