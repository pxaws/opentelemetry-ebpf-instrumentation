#!/usr/bin/env python3
"""Which status a caller adopts when one of its dependencies failed.

Standard library only, and its own module for the reason cpuwork.py is: the dev virtualenv has no
Flask, so a decision that lives in server.py cannot be tested there.

The rule exists because a failing dependency used to leave its caller green. That is a service map
that contradicts itself — the edge into the dependency shows faults while the caller's own card
reports no errors, and nothing on the caller explains why. Adopting the failure makes it traceable up
the graph.
"""

# The one 4xx worth passing through unchanged. A throttle tells the caller's own caller to slow down,
# which is still true one hop up.
THROTTLE = 429

# What a caller reports when it could not complete its work.
FAULT = 500

# The status at or above which a downstream response counts as a failure.
FAILURE_THRESHOLD = 400


def worst_status(statuses):
    """The status a caller should adopt, or None when no dependency failed.

    `statuses` holds one entry per downstream call: its status code, or None when there was no call
    to make and when the connection never got a response. None is not a failure here — it produces no
    downstream response to propagate, and the client metric records the attempt regardless.

    A fault outranks a throttle. A throttle wins only when EVERY failure was a throttle: if one
    dependency returned 404 and another returned 429, the caller definitely could not complete, so
    reporting a throttle would tell its caller to retry something that cannot succeed.
    """
    failures = [s for s in statuses if s is not None and s >= FAILURE_THRESHOLD]
    if not failures:
        return None
    return THROTTLE if all(s == THROTTLE for s in failures) else FAULT
