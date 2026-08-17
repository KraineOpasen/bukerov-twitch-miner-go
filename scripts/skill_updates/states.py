"""The candidate state machine.

Every provider check ends in exactly one state, and the states exist to make one distinction
impossible to blur: **automation can reach `PREPARED_AUDIT_REQUIRED`, and nothing else that
resembles approval.**

    NO_DRIFT                 the reviewed ref resolves to the pinned commit; nothing to do
    DRIFT_DETECTED           the ref moved; classification has not finished yet
    PREPARED_AUDIT_REQUIRED  a mechanically clean candidate exists and needs a human audit
    BLOCKED                  a judgement call is required; no candidate was produced
    AUDITED                  a human (or an agent under a task contract) established trust

`AUDITED` is deliberately unreachable from this package. `automation_may_set()` returns False for
it, `assert_automation_may_set()` raises on any attempt, and `scripts/validate-agent-governance.py`
independently fails on a manifest that claims it while an `automated_candidate` block is present.
Three separate mechanisms, because this is the one property whose failure would silently convert
"reviewed at a specific SHA" into "whatever the bot produced".

`DRIFT_DETECTED` is an intermediate: it is what an analysis holds between "the ref moved" and
"we finished classifying". A run that ends there is a bug in the classifier, not a legitimate
outcome, and `terminal()` says so.
"""

NO_DRIFT = "NO_DRIFT"
DRIFT_DETECTED = "DRIFT_DETECTED"
PREPARED_AUDIT_REQUIRED = "PREPARED_AUDIT_REQUIRED"
BLOCKED = "BLOCKED"
AUDITED = "AUDITED"

#: Every legal state, in lifecycle order. Order is used for deterministic reporting.
ALL_STATES = (NO_DRIFT, DRIFT_DETECTED, PREPARED_AUDIT_REQUIRED, BLOCKED, AUDITED)

#: States an automated run may legitimately end in.
TERMINAL_STATES = frozenset({NO_DRIFT, PREPARED_AUDIT_REQUIRED, BLOCKED})

#: The ONLY state automation may create. Not a set -- a single value, so that widening it is a
#: visible, deliberate edit rather than an item quietly appended to a collection.
AUTOMATION_CREATABLE = PREPARED_AUDIT_REQUIRED

#: States that require a human to establish. `AUDITED` is the whole point; it is listed here
#: rather than inferred so the prohibition is greppable.
HUMAN_ONLY_STATES = frozenset({AUDITED})

#: Legal transitions. Anything not listed is rejected -- an allowlist, so a new state cannot
#: acquire an accidental path to AUDITED by being added somewhere else.
TRANSITIONS = {
    NO_DRIFT: frozenset({DRIFT_DETECTED}),
    DRIFT_DETECTED: frozenset({PREPARED_AUDIT_REQUIRED, BLOCKED, NO_DRIFT}),
    PREPARED_AUDIT_REQUIRED: frozenset({AUDITED, BLOCKED, DRIFT_DETECTED}),
    BLOCKED: frozenset({DRIFT_DETECTED, NO_DRIFT}),
    AUDITED: frozenset({NO_DRIFT, DRIFT_DETECTED}),
}


class StateError(Exception):
    """An illegal state was requested, or an illegal transition attempted."""


def is_state(value):
    return value in ALL_STATES


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
            "automation may only create %s, never %s. %s is established by a human (or an agent "
            "under a task contract) reading the diff and recording a fresh reviewed_at/"
            "reviewed_by." % (AUTOMATION_CREATABLE, state, state))
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
