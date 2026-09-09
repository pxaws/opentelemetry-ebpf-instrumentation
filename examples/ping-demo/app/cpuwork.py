#!/usr/bin/env python3
"""Real CPU work for the ping demo's response time.

The demo used to shape its latency with `time.sleep` alone. A sleeping thread
holds no CPU, so CPU pressure on a host could not move that host's measured
latency and a CPU fault had nothing to show. This module spends a requested
number of CPU-seconds on work that actually occupies a core, and computes how a
target response duration divides into compute and sleep.

Two properties drive the design.

The compute is bounded by CPU time consumed, never by wall clock. A thread
accrues CPU time only while it is scheduled, so the same budget takes
proportionally longer once something else wants the core -- which is the signal
the demo is trying to produce. A wall-clock deadline does the opposite: the loop
completes fewer rounds under contention and still exits at the same instant,
holding the duration fixed and hiding what is being measured. Bounding by CPU
time also needs no calibration, so there is no sampled work rate to get wrong on
an already-busy host.

The work releases the GIL for its whole call. `hashlib.pbkdf2_hmac` does --
CPython's `_hashopenssl.c` drops the GIL around the derivation regardless of
input size. The app serves on gthread workers, so several requests share one
process's GIL; a primitive that held it would make them stretch with request
concurrency instead of with CPU contention. Hashing a small string is exactly
that trap: CPython only releases the GIL for buffers over 2048 bytes.

Standard library only, because the app's image installs just flask, gunicorn and
requests -- and because this module must stay testable in a virtualenv that has
none of them, which is why the logic lives here rather than in server.py.
"""
import hashlib
import math
import time
from typing import NamedTuple

# pbkdf2 iterations per pass through the burn loop. This is the loop's
# granularity: it can overshoot the CPU budget by at most the chunk it was
# inside when the budget ran out. The app's fastest profile asks for 5 ms
# responses, so a chunk has to cost a small fraction of a millisecond to leave
# the smallest budgets undistorted. 512 sha256 iterations measure ~0.1 ms on the
# demo's instance type. Going much smaller would spend a visible share of the
# time in the loop's own clock reads instead of on work.
CHUNK_ITERATIONS = 512

_HASH_NAME = "sha256"
_PASSWORD = b"obi-ping-demo-cpu-work"
_SALT = b"obi-ping-demo-salt"

# Wall-clock ceiling on one compute. Past the callers' 5 s HTTP timeout the
# fault stops behaving monotonically: the caller abandons the request and
# retries a healthy host, the abandoned request is recorded non-2xx, and it
# leaves the successful-response percentile the demo measures -- so a stronger
# fault would report a *smaller* p99, and anyone tuning the weight upward would
# see the number fall. The cap must also stay well above any target duration, or
# it quietly becomes the wall-clock deadline this module exists to avoid.
# Raising it is a correctness change, not a safety relaxation.
DEFAULT_WALL_CAP_SECONDS = 3.0


class CpuSpend(NamedTuple):
    cpu_seconds: float
    wall_seconds: float
    capped: bool


class DurationSplit(NamedTuple):
    cpu_budget_seconds: float
    sleep_seconds: float


def burn_chunk():
    """One fixed quantity of GIL-releasing CPU work.

    Named rather than inlined because it is the single place that decides what
    "CPU work" means here: swap the body for a small-string hash and the
    concurrency test must fail.
    """
    hashlib.pbkdf2_hmac(_HASH_NAME, _PASSWORD, _SALT, CHUNK_ITERATIONS)


def spend_cpu(budget_seconds, wall_cap_seconds=DEFAULT_WALL_CAP_SECONDS):
    """Occupy a core until this thread has consumed `budget_seconds` of CPU.

    Returns the CPU time actually consumed, the wall time that took, and whether
    the wall cap ended it early. On an idle host the two clocks agree; the gap
    between them is the CPU contention the demo wants to expose.
    """
    if budget_seconds <= 0:
        return CpuSpend(0.0, 0.0, False)

    cpu_start = time.thread_time()
    wall_start = time.perf_counter()

    while True:
        cpu_elapsed = time.thread_time() - cpu_start
        wall_elapsed = time.perf_counter() - wall_start

        if cpu_elapsed >= budget_seconds:
            return CpuSpend(cpu_elapsed, wall_elapsed, False)

        if wall_elapsed >= wall_cap_seconds:
            return CpuSpend(cpu_elapsed, wall_elapsed, True)

        burn_chunk()


def split_duration(target_seconds, cpu_share=0.0):
    """Divide a target response duration into a CPU budget and a sleep.

    Both halves are fixed proportions of the target. The sleep is deliberately
    not the remainder after the compute finishes: sleeping `target - elapsed`
    would re-impose a wall-clock deadline through the back door and cancel out
    exactly the contention the compute picked up. That is also why this function
    takes no elapsed time -- there is nothing to pass it.
    """
    target = max(0.0, target_seconds)
    share = resolve_share(cpu_share)

    return DurationSplit(target * share, target * (1.0 - share))


def resolve_share(cpu_share):
    """Coerce an operator-supplied share to a fraction of 0 through 1.

    Public because the caller has to report the EFFECTIVE share, not the string it read: a typo
    that resolves to zero would otherwise be indistinguishable from a correct deployment.

    The share arrives from the environment, so anything unparseable resolves to
    zero: a typo must leave the host behaving as it did before rather than
    crashing it. NaN and infinities are rejected explicitly because min/max
    would otherwise clamp them to a full share -- the least safe answer.
    """
    try:
        share = float(cpu_share)
    except (TypeError, ValueError):
        return 0.0

    if not math.isfinite(share):
        return 0.0

    return min(1.0, max(0.0, share))
