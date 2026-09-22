#!/usr/bin/env python3
"""Supplemental independent oracle for p4-hmac-sha256-u64/v1.

Written from the APPROVED algorithm (P4 preregistration protocol section 8),
not from the Go implementation, and reproducing the framing the owner's Work
vectors pin (work/entropy_python_vectors.json, work/entropy_node_vectors_and_
controls.json — those are the primary oracle; this file adds sequences, more
indices and more multi-byte identities):

  seed       = SHA-256(UTF-8("BUKEROV|P4|COMMON_CUTOFF|v1"))   (32 raw bytes)
  LP(s)      = u64 big-endian byte length of UTF-8(s) || UTF-8(s)
  M(k)       = LP("p4-hmac-sha256-u64/v1") || LP(dataset_id) || LP(dataset_version)
               || LP("sha256:" + common_factset_digest) || LP(paired_opportunity_id)
               || LP("P3B_ORDERED_RULES") || LP(decimal run_index)
               || LP("bernoulli_raw") || LP(decimal k)
  word(k)    = big-endian u64 of the FIRST 8 bytes of HMAC-SHA256(seed, M(k))
  rendering  = 16 lower-case hex digits

Words are consumed sequentially from k = 0. This script uses only Python's
standard library (hmac, hashlib, struct) so the expectation shares no code
with the implementation under test. Run it to regenerate entropy_vectors.json.
"""
import hashlib
import hmac
import json
import struct

ALGO = "p4-hmac-sha256-u64/v1"
SEED = hashlib.sha256("BUKEROV|P4|COMMON_CUTOFF|v1".encode("utf-8")).digest()
POLICY = "P3B_ORDERED_RULES"
DRAW = "bernoulli_raw"


def lp(s: str) -> bytes:
    b = s.encode("utf-8")
    return struct.pack(">Q", len(b)) + b


def message(dataset_id, dataset_version, digest_ref, opportunity, run, k) -> bytes:
    return (lp(ALGO) + lp(dataset_id) + lp(dataset_version) + lp(digest_ref) + lp(opportunity)
            + lp(POLICY) + lp(str(run)) + lp(DRAW) + lp(str(k)))


def word(dataset_id, dataset_version, digest_ref, opportunity, run, k):
    m = message(dataset_id, dataset_version, digest_ref, opportunity, run, k)
    mac = hmac.new(SEED, m, hashlib.sha256).digest()
    return m, mac, struct.unpack(">Q", mac[:8])[0]


def digest_ref(label: str) -> str:
    # A syntactically valid factset digest reference for a synthetic label.
    return "sha256:" + hashlib.sha256(("synthetic-factset:" + label).encode("utf-8")).hexdigest()


cases = [
    ("p4-synthetic-dataset", "v1", digest_ref("a"), "event-round-0001", 0, [0, 1, 2, 3]),
    ("p4-synthetic-dataset", "v1", digest_ref("a"), "event-round-0001", 16383, [0]),
    ("p4-synthetic-dataset", "v1", digest_ref("b"), "event-round-0002", 7, [0, 1]),
    ("p4-synthetic-dataset", "v2", digest_ref("b"), "event-round-0002", 7, [0]),
    # Multi-byte UTF-8: the length prefix counts BYTES, not runes or UTF-16
    # code units (набор = 10 bytes, раунд-№7 = 15 bytes).
    ("набор-π", "v1", digest_ref("c"), "раунд-№7", 42, [0, 1]),
    ("another dataset", "v1", digest_ref("d"), "e", 1, [0, 5, 4096]),
]

out = {
    "algorithm": ALGO,
    "seedHex": SEED.hex(),
    "framing": "u64 big-endian byte-length prefix per part, parts: algorithm, dataset id, dataset version, sha256:-prefixed common factset digest, paired opportunity id, P3B_ORDERED_RULES, decimal run index, bernoulli_raw, decimal word index; HMAC-SHA256 under the raw 32-byte seed; first 8 bytes big-endian",
    "vectors": [],
}
for dataset_id, version, ref, opp, run, ks in cases:
    for k in ks:
        m, mac, w = word(dataset_id, version, ref, opp, run, k)
        out["vectors"].append({
            "datasetId": dataset_id, "datasetVersion": version, "commonFactsetDigest": ref,
            "pairedOpportunityId": opp, "trajectory": run, "index": k,
            "messageHex": m.hex(), "macHex": mac.hex(), "word": "%016x" % w,
        })
    if len(ks) > 1 and ks == list(range(len(ks))):
        out.setdefault("sequences", []).append({
            "datasetId": dataset_id, "datasetVersion": version, "commonFactsetDigest": ref,
            "pairedOpportunityId": opp, "trajectory": run,
            "words": ["%016x" % word(dataset_id, version, ref, opp, run, k)[2] for k in ks],
        })

with open("entropy_vectors.json", "w", encoding="utf-8") as f:
    json.dump(out, f, indent=2, ensure_ascii=False)
    f.write("\n")
print("vectors:", len(out["vectors"]), "sequences:", len(out.get("sequences", [])))
