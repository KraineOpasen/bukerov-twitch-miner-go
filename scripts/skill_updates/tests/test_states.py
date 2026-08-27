"""Tests for the candidate state machine.

The single property worth most of this file: G1.1 exposes exactly three production outcomes and
owns no approval transition.
"""

import unittest

from .. import states
from ..states import StateError


class TestStateSet(unittest.TestCase):
    def test_exact_three_production_states_exist(self):
        self.assertEqual(states.ALL_STATES,
                         ("NO_DRIFT", "BLOCKED", "PREPARED_AUDIT_REQUIRED"))

    def test_terminal_states_exclude_the_intermediate(self):
        self.assertNotIn(states.DRIFT_DETECTED, states.TERMINAL_STATES)
        for state in (states.NO_DRIFT, states.PREPARED_AUDIT_REQUIRED, states.BLOCKED):
            self.assertTrue(states.terminal(state), state)


class TestAutomationBoundary(unittest.TestCase):
    def test_only_prepared_is_automation_creatable(self):
        self.assertEqual(states.AUTOMATION_CREATABLE, states.PREPARED_AUDIT_REQUIRED)
        self.assertTrue(states.automation_may_set(states.PREPARED_AUDIT_REQUIRED))
        for state in states.ALL_STATES:
            if state == states.PREPARED_AUDIT_REQUIRED:
                continue
            self.assertFalse(states.automation_may_set(state), state)

    def test_automation_cannot_set_a_later_audit_claim(self):
        with self.assertRaises(StateError) as caught:
            states.assert_automation_may_set("AUDITED")
        self.assertIn("AUDITED", str(caught.exception))

    def test_automation_cannot_set_any_non_candidate_state(self):
        for state in (states.NO_DRIFT, states.BLOCKED, states.DRIFT_DETECTED):
            with self.assertRaises(StateError, msg=state):
                states.assert_automation_may_set(state)

    def test_unknown_state_is_rejected(self):
        for bogus in ("audited", "AUDITED ", "", None, "APPROVED", 1):
            with self.assertRaises(StateError, msg=repr(bogus)):
                states.assert_automation_may_set(bogus)

class TestTransitions(unittest.TestCase):
    def test_prepared_has_no_audit_or_promotion_transition(self):
        for forbidden in ("AUDITED", "READY", "ARMED", "MERGED"):
            with self.assertRaises(StateError, msg=forbidden):
                states.assert_transition(states.PREPARED_AUDIT_REQUIRED, forbidden)

    def test_blocked_may_not_jump_to_a_later_stage(self):
        with self.assertRaises(StateError):
            states.assert_transition(states.BLOCKED, "AUDITED")

    def test_no_drift_may_not_jump_to_a_later_stage(self):
        with self.assertRaises(StateError):
            states.assert_transition(states.NO_DRIFT, "AUDITED")

    def test_every_transition_target_is_a_real_state(self):
        for source, targets in states.TRANSITIONS.items():
            self.assertTrue(states.is_state(source))
            for target in targets:
                self.assertTrue(states.is_state(target), "%s -> %s" % (source, target))

    def test_no_later_authority_is_reachable(self):
        targets = {target for values in states.TRANSITIONS.values() for target in values}
        self.assertTrue(targets.isdisjoint({"AUDITED", "READY", "ARMED", "MERGED"}))


class TestClassify(unittest.TestCase):
    def test_no_drift(self):
        self.assertEqual(states.classify(False, [], False), states.NO_DRIFT)

    def test_blocked_beats_prepared(self):
        """A provider that produced verdicts before blocking must never look usable."""
        self.assertEqual(states.classify(True, ["x"], True), states.BLOCKED)

    def test_blocked_beats_not_drifted(self):
        """An unresolvable ref is blocked with no target commit, so it reads as un-drifted.

        Reporting it as NO_DRIFT would turn an unreachable or renamed upstream into silence --
        the one wrong answer that produces no report at all.
        """
        self.assertEqual(states.classify(False, ["unprovable"], False), states.BLOCKED)
        self.assertEqual(states.classify(False, ["unprovable"], True), states.BLOCKED)

    def test_prepared(self):
        self.assertEqual(states.classify(True, [], True), states.PREPARED_AUDIT_REQUIRED)

    def test_drift_without_classification_is_the_intermediate(self):
        self.assertEqual(states.classify(True, [], False), states.DRIFT_DETECTED)
        self.assertFalse(states.terminal(states.classify(True, [], False)))


if __name__ == "__main__":
    unittest.main()
