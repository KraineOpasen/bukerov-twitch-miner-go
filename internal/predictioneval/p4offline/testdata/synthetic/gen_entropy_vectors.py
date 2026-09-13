#!/usr/bin/env python3
"""Independent oracle for p4-hmac-sha256-u64/v1.

Written from the algorithm SPECIFICATION, not from the Go implementation:

  LP(s)      = u64 big-endian byte length of UTF-8(s) || UTF-8(s)
  M(k)       = LP("p4-hmac-sha256-u64/v1") || LP("p4-offline/v1")
               || LP(decimal trajectory) || LP(sourceRoundID)
               || LP(policyID) || LP(decimal k)
  word(k)    = big-endian u64 of the FIRST 8 bytes of HMAC-SHA256(UTF-8(key), M(k))
  rendering  = 16 lower-case hex digits

Words are consumed sequentially from k = 0. This script uses only Python's
standard library (hmac, hashlib, struct) so the expectation shares no code with
the implementation under test. Run it to regenerate entropy_vectors.json.
"""
import hashlib
import hmac
import json
import struct

ALGO = "p4-hmac-sha256-u64/v1"
PROTO = "p4-offline/v1"


def lp(s: str) -> bytes:
    b = s.encode("utf-8")
    return struct.pack(">Q", len(b)) + b


def message(trajectory: int, round_id: str, policy_id: str, k: int) -> bytes:
    return lp(ALGO) + lp(PROTO) + lp(str(trajectory)) + lp(round_id) + lp(policy_id) + lp(str(k))


def word(key: str, trajectory: int, round_id: str, policy_id: str, k: int):
    m = message(trajectory, round_id, policy_id, k)
    mac = hmac.new(key.encode("utf-8"), m, hashlib.sha256).digest()
    w = struct.unpack(">Q", mac[:8])[0]
    return m, mac, w


cases = [
    ("p4-synthetic-key-01", 0, "event-round-0001", "ruleset-alpha", [0, 1, 2, 3]),
    ("p4-synthetic-key-01", 16383, "event-round-0001", "ruleset-alpha", [0]),
    ("p4-synthetic-key-01", 7, "event-round-0002", "ruleset-alpha", [0, 1]),
    ("p4-synthetic-key-01", 7, "event-round-0002", "ruleset-beta", [0]),
    # Multi-byte UTF-8: the length prefix counts BYTES (6 and 9), not runes.
    ("κλειδί", 42, "раунд-№7", "règle-ß", [0, 1]),
    ("another key", 1, "e", "p", [0, 5, 4096]),
]

out = {"algorithm": ALGO, "protocol": PROTO, "framing": "u64 big-endian byte-length prefix per part, parts: algorithm, protocol, decimal trajectory, source round id, policy id, decimal word index", "vectors": []}
for key, traj, rid, pid, ks in cases:
    for k in ks:
        m, mac, w = word(key, traj, rid, pid, k)
        out["vectors"].append({
            "key": key, "trajectory": traj, "sourceRoundId": rid, "policyId": pid, "index": k,
            "messageHex": m.hex(), "macHex": mac.hex(), "word": "%016x" % w,
        })
    if len(ks) > 1 and ks == list(range(len(ks))):
        out.setdefault("sequences", []).append({
            "key": key, "trajectory": traj, "sourceRoundId": rid, "policyId": pid,
            "words": ["%016x" % word(key, traj, rid, pid, k)[2] for k in ks],
        })

with open("entropy_vectors.json", "w", encoding="utf-8") as f:
    json.dump(out, f, indent=2, ensure_ascii=False)
    f.write("\n")
print("vectors:", len(out["vectors"]), "sequences:", len(out.get("sequences", [])))
