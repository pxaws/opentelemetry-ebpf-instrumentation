#!/usr/bin/env python3
"""Unit tests for propagate.py.

Standard library only, like the module under test. The dev virtualenv has no Flask, so nothing here
may reach server.py.

Run: python3 examples/ping-demo/app/test_propagate.py
"""

import importlib.util
import unittest
from pathlib import Path

_SPEC = importlib.util.spec_from_file_location(
    "propagate", Path(__file__).resolve().parent / "propagate.py"
)
propagate = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(propagate)

worst_status = propagate.worst_status


class NoFailure(unittest.TestCase):
    def test_no_downstream_calls_adopts_nothing(self):
        self.assertIsNone(worst_status([]))

    def test_every_dependency_healthy_adopts_nothing(self):
        self.assertIsNone(worst_status([200, 200]))

    def test_a_transport_failure_alone_adopts_nothing(self):
        # None means no downstream response existed to propagate. The client metric records the
        # attempt, so the edge still shows it.
        self.assertIsNone(worst_status([None, None]))

    def test_a_redirect_is_not_a_failure(self):
        self.assertIsNone(worst_status([304]))


class FaultWins(unittest.TestCase):
    def test_a_server_fault_becomes_a_fault(self):
        self.assertEqual(worst_status([500, 200]), 500)

    def test_a_service_unavailable_becomes_a_fault(self):
        self.assertEqual(worst_status([None, 503]), 500)

    def test_a_not_found_becomes_a_fault_and_not_an_error(self):
        # A caller that could not read its dependency did not fail because of its own client.
        self.assertEqual(worst_status([404, 200]), 500)

    def test_a_fault_outranks_a_throttle(self):
        self.assertEqual(worst_status([500, 429]), 500)

    def test_a_throttle_mixed_with_another_error_becomes_a_fault(self):
        # Reporting a throttle here would tell the caller's caller to retry something that cannot
        # succeed, because the 404 will not go away.
        self.assertEqual(worst_status([429, 404]), 500)


class ThrottlePassesThrough(unittest.TestCase):
    def test_a_lone_throttle_passes_through(self):
        self.assertEqual(worst_status([429, 200]), 429)

    def test_every_failure_a_throttle_passes_through(self):
        self.assertEqual(worst_status([429, 429]), 429)


if __name__ == "__main__":
    unittest.main(verbosity=2)
