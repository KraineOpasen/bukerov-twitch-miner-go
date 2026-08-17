"""Tests for the candidate state machine.

The single property worth most of this file: **automation cannot reach `AUDITED`.** It is
asserted from several directions, because a state machine that only enforces its rule in the one
place the tests happen to look is not enforcing anything.
"""

import unittest

from .. import states
from ..states import StateError


class TestStateSet(unittest.TestCase):
    def test_all_five_states_exist(self):
        self.assertEqual(set(states.ALL_STATES), {
            "NO_DRIFT", "DRIFT_DETECTED", "PREPARED_AUDIT_REQUIRED", "BLOCKED", "AUDITED"})

    def test_terminal_states_exclude_the_intermediate(self):
        self.assertNotIn(states.DRIFT_DETECTED, states.TERMINAL_STATES)
        self.assertNotIn(states.AUDITED, states.TERMINAL_STATES)
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

    def test_automation_cannot_set_audited(self):
        with self.assertRaises(StateError) as caught:
            states.assert_automation_may_set(states.AUDITED)
        self.assertIn("AUDITED", str(caught.exception))
        self.assertIn("human", str(caught.exception))

    def test_automation_cannot_set_any_non_candidate_state(self):
        for state in (states.NO_DRIFT, states.BLOCKED, states.DRIFT_DETECTED, states.AUDITED):
            with self.assertRaises(StateError, msg=state):
                states.assert_automation_may_set(state)

    def test_unknown_state_is_rejected(self):
        for bogus in ("audited", "AUDITED ", "", None, "APPROVED", 1):
            with self.assertRaises(StateError, msg=repr(bogus)):
                states.assert_automation_may_set(bogus)

    def test_audited_is_declared_human_only(self):
        self.assertIn(states.AUDITED, states.HUMAN_ONLY_STATES)


class TestTransitions(unittest.TestCase):
    def test_prepared_may_become_audited_by_a_human(self):
        self.assertEqual(states.assert_transition(states.PREPARED_AUDIT_REQUIRED,
                                                  states.AUDITED), states.AUDITED)

    def test_blocked_may_not_jump_straight_to_audited(self):
        with self.assertRaises(StateError):
            states.assert_transition(states.BLOCKED, states.AUDITED)

    def test_no_drift_may_not_jump_to_audited(self):
        with self.assertRaises(StateError):
            states.assert_transition(states.NO_DRIFT, states.AUDITED)

    def test_every_transition_target_is_a_real_state(self):
        for source, targets in states.TRANSITIONS.items():
            self.assertIn(source, states.ALL_STATES)
            for target in targets:
                self.assertIn(target, states.ALL_STATES, "%s -> %s" % (source, target))

    def test_audited_is_reachable_from_exactly_one_state(self):
        sources = [s for s, t in states.TRANSITIONS.items() if states.AUDITED in t]
        self.assertEqual(sources, [states.PREPARED_AUDIT_REQUIRED])


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
