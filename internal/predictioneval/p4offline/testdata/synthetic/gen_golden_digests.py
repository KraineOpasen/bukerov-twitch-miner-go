#!/usr/bin/env python3
"""Independent exact-wire oracle for the p4offline canonical serializations.

Written from the documented FRAMING SPECIFICATION (see PROVENANCE.md, section
"Canonical framing"), not from the Go code:

  LP(part)  = uint64 big-endian byte length || bytes
  str(s)    = LP(UTF-8(s))
  i64(v)    = str(decimal v)            u64(v) = str(decimal v)
  bool(v)   = str("true" | "false")
  f64(v)    = str(16 lower-case hex digits of the IEEE-754 bit pattern)
  count(n)  = i64(n)
  digest    = lower-case hex SHA-256 over the concatenated parts

Field orders are the ones PROVENANCE.md lists for each artifact. Fixture values
are the ones fixtures_test.go builds (the "placed attempt" case) and the
resolution_test.go / evidence_test.go literals. Standard library only.
"""
import hashlib
import json
import struct


def lp(b: bytes) -> bytes:
    return struct.pack(">Q", len(b)) + b


def s(x: str) -> bytes:
    return lp(x.encode("utf-8"))


def i64(v: int) -> bytes:
    return s(str(v))


u64 = i64


def boolean(v: bool) -> bytes:
    return s("true" if v else "false")


def f64(v: float) -> bytes:
    return s("%016x" % struct.unpack(">Q", struct.pack(">d", v))[0])


count = i64


def digest(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


# ---- shared fixture identities (fixtures_test.go) --------------------------
EPOCH, SESSION, POOL, ROUND, EVENT = 20260901, "p4-synth-session-a", "pool-a", "r1", "e1"
ATTEMPT = 1
TERMINAL_OBS = "auto_decision-AUTO_DECIDED-2"   # kind-phase-seq; due=1, terminal=2
CUTOFF = 2
PROJECTOR = "predictioneval/v1"
POLICY_REV = "policy-378d05d6ccc7d2a914730a1e1d023ff754bcf873"
PRODUCER_REV = "obs-v2|" + POLICY_REV


def episode_string() -> str:
    return (i64(EPOCH) + s(SESSION) + s(POOL) + s(ROUND) + s(EVENT)).hex()


def settings(strategy, pct, gap, maxp, minp, stealth, delay, mode, filt) -> bytes:
    b = boolean(True) + s(strategy) + i64(pct) + i64(gap) + i64(maxp) + i64(minp) + boolean(stealth) + f64(delay) + s(mode)
    b += boolean(filt is not None)
    if filt is not None:
        b += s(filt[0]) + s(filt[1]) + f64(filt[2])
    return b


# ---- (a) CommonFactset: the placed-attempt fixture -------------------------
def factset_bytes() -> bytes:
    b = s("p4-common-factset/v1") + s("p4-offline/v1") + s(PROJECTOR) + s(episode_string())
    b += i64(EPOCH) + s(SESSION) + s(POOL) + u64(ATTEMPT) + s(TERMINAL_OBS) + i64(CUTOFF)
    b += s("COMPLETE") + count(0)
    b += boolean(True) + s("") + s("FACTUAL_STEALTH_OFF")
    b += settings("MOST_VOTED", 5, 20, 50000, 0, False, 6.0, "FROM_END", None)
    b += boolean(True) + i64(1000)          # balance present, 1000
    b += boolean(True) + count(2)           # outcomes present, 2
    for slot, present, oid, users, points, top, pct, odds, oddspct in [
        (0, True, "o1", 6, 600, 90, 60.0, 1.66, 60.24),
        (1, True, "o2", 4, 400, 80, 40.0, 2.5, 40.0),
    ]:
        b += i64(slot) + boolean(present) + s(oid) + i64(users) + i64(points) + i64(top) + f64(pct) + f64(odds) + f64(oddspct)
    b += boolean(True) + i64(10) + boolean(True) + i64(1000)   # bet totals
    b += boolean(True) + i64(0) + i64(0)                        # risk present, 0, 0
    b += s("ALLOWED") + s("") + i64(10)                         # health, reason, minimum stake
    return b


# ---- (b) P2 config binding for the same fixture -----------------------------
def binding_bytes() -> bytes:
    b = s("p4-p2-config-binding/v1") + s(PROJECTOR) + s(POLICY_REV) + s(PRODUCER_REV) + i64(64)
    b += settings("MOST_VOTED", 5, 20, 50000, 0, False, 6.0, "FROM_END", None)
    b += boolean(True) + i64(0) + i64(0) + i64(10)
    return b


# ---- (c) ResolutionArtifact for resolution_test.go's goodWinnerEvidence ------
def resolution_bytes() -> bytes:
    b = s("p4-resolution-facts/v1") + s("p4offline-resolution-obligations/provisional-v1")
    b += s(EVENT) + s("")                     # round: event id, channel id
    b += count(2) + s("o1") + s("o2")
    b += s("WINNER_KNOWN") + s("o2") + i64(1)
    b += s("PLATFORM_RESOLVED_EVENT_WINNING_OUTCOME")
    b += count(1) + s("resolved-1") + i64(EPOCH) + s(SESSION) + s("channel_event") + s("ROUND_UPDATED") + s("RESOLVED") + s(EVENT)
    b += s("AVAILABLE") + s("test-projector/v1") + s("test-proof/v1")
    b += count(0)                              # refusals
    return b


# ---- (d) SourceRoundRegistry for evidence_test.go's identical-claims case ----
def claim_key(ep_epoch, ep_session, ep_pool, ep_round, ep_event, at_epoch, at_session, at_pool, at_id, fsd) -> str:
    ep = (i64(ep_epoch) + s(ep_session) + s(ep_pool) + s(ep_round) + s(ep_event)).hex()
    return (s(ep) + i64(at_epoch) + s(at_session) + s(at_pool) + s(str(at_id)) + s(fsd)).hex()


def registry_bytes() -> bytes:
    k1 = claim_key(1, "s-a", "p", "r", "e1", 1, "s-a", "p", 1, "d1")
    k2 = claim_key(1, "s-a", "p", "r2", "e2", 1, "s-a", "p", 1, "d2")
    b = s("p4-source-round-registry/v1") + count(2)
    b += s("e1") + s("DEDUPLICATED_IDENTICAL") + count(2) + s(k1) + s(k1) + boolean(True)
    b += s("e2") + s("UNIQUE") + count(1) + s(k2) + boolean(True)
    return b


out = {}
for name, fn in [("commonFactset", factset_bytes), ("p2ConfigBinding", binding_bytes),
                 ("resolutionArtifact", resolution_bytes), ("sourceRoundRegistry", registry_bytes)]:
    raw = fn()
    out[name] = {"framedHex": raw.hex(), "sha256": digest(raw)}
with open("golden_digests.json", "w", encoding="utf-8") as f:
    json.dump(out, f, indent=2)
    f.write("\n")
for k, v in out.items():
    print(k, v["sha256"], len(v["framedHex"]) // 2, "bytes")
