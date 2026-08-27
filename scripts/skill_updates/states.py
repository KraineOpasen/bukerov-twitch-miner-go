"""The candidate state machine.

Every provider check ends in exactly one state, and the states exist to make one distinction
impossible to blur: **automation can reach `PREPARED_AUDIT_REQUIRED`, and nothing else that
resembles approval.**

    NO_DRIFT                 the reviewed ref resolves to the pinned commit; nothing to do
    PREPARED_AUDIT_REQUIRED  a mechanically clean candidate exists and needs a human audit
    BLOCKED                  a judgement call is required; no candidate was produced

G1.1 deliberately owns no audited state at all.  A later, separately commissioned control plane
may consume a prepared artifact, but this package neither names nor transitions to an approval
state.  This is stronger than making approval "human only": there is no G1.1 authority surface
on which an approval claim can be represented.

`DRIFT_DETECTED` is an intermediate: it is what an analysis holds between "the ref moved" and
"we finished classifying". A run that ends there is a bug in the classifier, not a legitimate
outcome, and `terminal()` says so.
"""

NO_DRIFT = "NO_DRIFT"
DRIFT_DETECTED = "DRIFT_DETECTED"
PREPARED_AUDIT_REQUIRED = "PREPARED_AUDIT_REQUIRED"
BLOCKED = "BLOCKED"

#: The complete externally reportable G1.1 state set, in deterministic report order.
PRODUCTION_STATES = (NO_DRIFT, BLOCKED, PREPARED_AUDIT_REQUIRED)
ALL_STATES = PRODUCTION_STATES

#: States an automated run may legitimately end in.
TERMINAL_STATES = frozenset({NO_DRIFT, PREPARED_AUDIT_REQUIRED, BLOCKED})

#: The ONLY state automation may create. Not a set -- a single value, so that widening it is a
#: visible, deliberate edit rather than an item quietly appended to a collection.
AUTOMATION_CREATABLE = PREPARED_AUDIT_REQUIRED

#: Legal detector/classifier transitions.  `DRIFT_DETECTED` is internal and cannot be emitted
#: by a completed run.  There is intentionally no approval/later-stage transition.
TRANSITIONS = {
    NO_DRIFT: frozenset({DRIFT_DETECTED}),
    DRIFT_DETECTED: frozenset({PREPARED_AUDIT_REQUIRED, BLOCKED, NO_DRIFT}),
    PREPARED_AUDIT_REQUIRED: frozenset({BLOCKED, DRIFT_DETECTED}),
    BLOCKED: frozenset({DRIFT_DETECTED, NO_DRIFT}),
}


class StateError(Exception):
    """An illegal state was requested, or an illegal transition attempted."""


def is_state(value):
    return value in PRODUCTION_STATES or value == DRIFT_DETECTED


def terminal(state):
    """True when `state` is a legitimate end point for an automated run."""
    return state in TERMINAL_STATES


def automation_may_set(state):
    """True only for `PREPARED_AUDIT_REQUIRED`.

    `NO_DRIFT` and `BLOCKED` are *observations* -- the bot reports them, it does not write them
    into a manifest -- so neither is 'creatable' in the sense that matters here. The only state
    automation ever writes down is the candidate one.
    """
    return state == AUTOMATION_CREATABLE


def assert_automation_may_set(state):
    """Raise unless automation is allowed to create `state`."""
    if not is_state(state):
        raise StateError("unknown candidate state %r" % (state,))
    if not automation_may_set(state):
        raise StateError(
            "G1.1 automation may only create %s, never %s; audit and promotion authority are "
            "not commissioned in this stage." % (AUTOMATION_CREATABLE, state))
    return state


def assert_transition(current, nxt):
    """Raise unless `current -> nxt` is a legal transition."""
    if not is_state(current) or not is_state(nxt):
        raise StateError("unknown state in transition %r -> %r" % (current, nxt))
    if nxt not in TRANSITIONS[current]:
        raise StateError("illegal transition %s -> %s" % (current, nxt))
    return nxt


def classify(drifted, blocked, prepared):
    """The state an analysis has reached, from three facts about it.

    Deliberately total and order-sensitive, and `BLOCKED` is tested FIRST -- ahead of both
    `PREPARED_AUDIT_REQUIRED` and `NO_DRIFT`.

    Ahead of `prepared`, so a provider that produced some file verdicts before hitting a
    blocking condition can never be reported as a usable candidate.

    Ahead of `not drifted`, because a provider whose ref could not be resolved has NO target
    commit and therefore reads as un-drifted while being blocked. Reporting that as `NO_DRIFT`
    would be the one wrong answer that produces silence instead of a report -- exactly what
    `Analysis.drifted`'s own docstring promises never happens ("'unknown' is never reported as
    'no drift'").
    """
    if blocked:
        return BLOCKED
    if not drifted:
        return NO_DRIFT
    if prepared:
        return PREPARED_AUDIT_REQUIRED
    return DRIFT_DETECTED
