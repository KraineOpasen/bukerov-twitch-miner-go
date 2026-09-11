# Ordered-rules fixtures — provenance and licence disposition

These fixtures support `internal/predictioneval`'s ordered-rules core. They are
OFFLINE test data. Nothing here was collected from Twitch, from a production
database, or from any live account, and nothing here should be described as a
production fact.

## What the model is

A pure re-derivation of a donor project's ordered odds-rules mechanism —
ordered detailed rules with a per-rule participation rate, a per-outcome
default, and a percentage stake — evaluated over data a caller supplies
explicitly. It produces results labelled
`CORE_MECHANISM_COMMON_ADMITTED_DATA`.

It is deliberately NOT a faithful replay of the donor's full runtime policy.
The donor branches on lock/end timestamps, refreshes its balance against a live
API before each attempt, re-enters on every round update until a placement
succeeds, and may retry after a failed call. None of those transitions is
established by anything this repository persists, so none is modelled, and no
result from this model may be described as a full-policy replay.

## Pins

Every byte below was fetched at the exact revision named and verified by Git
blob hash against that revision's tree.

### Donor mechanism

| Item | Value |
| --- | --- |
| Repository | `t348575/twitch-points-miner` |
| Commit | `ae9d39d3d10ff8240f24e2529fe7cf6dd701db66` |
| Tree | `5e781ae8b1e4f28427dcd8ca790c8e9a7afbcf49` |

| Path | Blob SHA-1 | What it establishes |
| --- | --- | --- |
| `app/src/pubsub.rs` | `7fa0b4c8b9133d78b3705bad30ddf603ff6b500d` | `prediction_logic`: the outcome-outer traversal, the `find` over ordered rules with fall-through on a failed draw, the per-outcome default, and the reciprocal pool share |
| `common/src/config/strategy.rs` | `eee117476395c194b4a675b64f64bcefee3de058` | `Normalize` (a single division by 100) and `Points::value` (multiply, truncate to `u32`, then cap; `max_value == 0` means no cap) |
| `common/src/config/filters.rs` | `abead64b1f622fd206afb689b465991c9702ef04` | The donor's runtime filters — read to establish that they are NOT part of this model |
| `common/src/types.rs` | `237ebc1da800454daa73b66768f5c518912ef34a` | `StreamerState.points` is `u32`, fixing the stake domain |
| `example.config.yaml` | `0e5889816fd9dda591deb7a09aeac444c43ff6a2` | The raw-percentage basis, in the donor's own words: `attempt_rate: 1.0` is annotated "1% of the time" |
| `LICENSE` | `261eeb9e9f8b2b4b0d119366dda99c6fd7d35c64` | Apache License 2.0 |

The donor's outcome points are `i64`: fixed by its own test helper
`outcome_from(id: u32, points: i64, users: i64)` in `app/src/pubsub.rs`.

### Entropy semantics

The donor draws from `rand`'s thread-local generator. That realization was never
recorded anywhere, so the historical entropy is **UNAVAILABLE** and this model
does not attempt to reconstruct it. What is pinned is the exact CONSUMPTION
rule, so that a supplied trace is consumed deterministically.

| Item | Value |
| --- | --- |
| Crate | `rand` 0.8.5, per the donor's `Cargo.lock` |
| Source | `rust-random/rand`, commit `937320cbfeebd4352a23086d9c6e68f067f74644` (the immutable commit `0.8.5` resolves to) |
| `src/distributions/bernoulli.rs` | blob `226db79fa9cea89df0083ca352c5d486f1efdbe9` |
| `src/rng.rs` | blob `79a9fbff46e0e8309c9a19f0ef2fe6471d597b27` |

The semantics those two blobs establish, verbatim in behaviour:

```text
Bernoulli::new(p):  !(0.0..1.0).contains(p) -> if p == 1.0 { ALWAYS_TRUE } else { Err }
                    otherwise                -> p_int = (p * 2^64) as u64
sample():           p_int == ALWAYS_TRUE     -> true, consuming NO word
                    otherwise                -> draw one u64 v, return v < p_int
```

So a participation rate of exactly **one** succeeds and consumes **nothing**,
while a rate of exactly **zero** consumes **one word** and then fails. The
comparison is strict. `gen_bool` is called only after the comparator matched,
because Rust's `&&` short-circuits.

## Licence disposition

**KEEP_FAITHFUL_REFERENCE + independent pure reimplementation.**

The donor is Apache-2.0; this repository is GPL-3.0. The Apache Software
Foundation states that Apache-2.0 software may be included in GPLv3 projects
subject to the licence's conditions (<https://www.apache.org/licenses/GPL-compatibility.html>),
so the direction of inclusion here is the permitted one. This repository's root
`LICENSE` is unchanged.

No donor source bytes were copied into this repository. The Go implementation is
an independent reimplementation adapted to a value-in/value-out shape: it takes
supplied data instead of live state, returns typed results instead of placing
bets, and consumes an explicit entropy trace instead of a thread-local
generator. No donor crate, runtime, GraphQL refresh, simulate flag or analytics
path is carried over. If protected source bytes are ever copied or adapted into
this tree, the Apache licence, attribution and modification notices must travel
with them; that has not happened here.

`rand` is Apache-2.0 OR MIT. Only a description of its Bernoulli semantics is
used; the crate itself is not imported, vendored, or reproduced.

The donor's README states it was inspired by `rdavydov`'s project. That is a
claim of inspiration, and it is recorded here rather than relied on: nothing in
this model derives from it.

## Origin of the fixture values

Every vector is HAND-CONSTRUCTED to isolate one mechanism decision, and each is
stated with its arithmetic shown in the test that consumes it. None is a
capture of production data, and none was selected by trying seeds until one
produced an agreeable result.

The recurring pool `[A:4, B:6]` is used because its two donor shares — the
double nearest `0.4` and the double nearest `0.6` — sit exactly on the raw
thresholds `40` and `60` after a single division by one hundred, which makes the
inclusive-boundary comparisons exact rather than approximate.

The pool `[9, 1]` is the numeric-fidelity falsifier. The donor's `1/(10/9)` is
`0x1.cccccccccccccp-1`; the algebraic shortcut `9/10` is
`0x1.ccccccccccccdp-1`, one ulp higher, and is exactly the double a raw
threshold of `90` normalizes to. A `Ge` rule at `90` therefore separates the two
formulations: the donor does not match, the shortcut does.

The words `0x0000000000000000` and `0x8000000000000000` are chosen against a
participation rate of one half, whose Bernoulli threshold is exactly `2^63` with
no rounding at all. The first admits; the second is the threshold itself and is
refused by the strict comparison.

## What these fixtures do NOT establish

- That any of these values ever occurred in production.
- That the donor's full policy — its filters, cadence, balance acquisition,
  lock/end branching, placement success, retry or pool mutation — is reproduced.
- Anything about profitability, strategy quality, or the merits of the donor
  mechanism versus this miner's configured policy.
