"""Deterministic three-way text merge for vendored skill files.

Every vendored upstream-origin file is compared as a triple:

    BASE    bytes at the OLD pinned upstream commit
    OURS    the bytes currently vendored in this repo (may carry local patches)
    THEIRS  bytes at the NEW upstream commit

and resolved by exactly four rules, in this order:

    THEIRS == BASE          -> retain OURS   (upstream did not touch the file)
    OURS   == BASE          -> take THEIRS   (we never patched it; adopt upstream verbatim)
    OURS   == THEIRS        -> take either   (both sides converged on the same bytes)
    otherwise               -> three-way merge; clean -> merged bytes, else CONFLICT

The first three rules are byte comparisons and never invoke the merge engine, which matters:
a binary or undecodable file can still be refreshed or retained safely as long as only one
side moved. The merge engine is reached only when BOTH sides changed, and that is exactly the
case where a binary/undecodable file must be reported BLOCKED rather than guessed at.

Why a hand-written diff3 instead of shelling out to `git merge-file`
--------------------------------------------------------------------
`git merge-file` would work, but its output is a function of the *installed git version's*
xdiff implementation. This engine runs in CI today and on a maintainer's laptop tomorrow, and
a candidate that merges cleanly in one place but conflicts in the other would make the bot's
verdicts unreproducible -- the one property an automated provenance tool cannot trade away.
`difflib.SequenceMatcher` is pinned to the Python version, is pure stdlib, and is fully
deterministic here because `autojunk` is disabled (see `_align`). The algorithm below is the
classic diff3: align both sides to BASE, walk the three sequences in lockstep, and classify
each region as stable (all three agree) or unstable (resolve or conflict).

This module is intentionally free of git, filesystem, network and GitHub concepts. It takes
bytes and returns a verdict, which is what makes every branch of it testable with byte-exact
fixtures.
"""

import difflib

# Merge verdicts. `resolve()` returns one of these as the first element of its result tuple.
RETAIN_OURS = "retain_ours"      # upstream unchanged; keep the vendored bytes as they are
TAKE_THEIRS = "take_theirs"      # not locally modified; adopt the new upstream bytes
CONVERGED = "converged"          # both sides independently reached identical bytes
MERGED = "merged"                # both sides changed; three-way merge succeeded
CONFLICT = "conflict"            # both sides changed and the regions overlap
BINARY_CONFLICT = "binary_conflict"  # both sides changed but the bytes cannot be merged as text

#: Verdicts that yield usable output bytes. Anything else is a BLOCKED condition upstream of
#: this module. Kept as a frozenset so a caller can assert membership rather than re-listing
#: the clean verdicts and drifting out of sync with this file.
CLEAN_VERDICTS = frozenset({RETAIN_OURS, TAKE_THEIRS, CONVERGED, MERGED})


def is_binary(data):
    """True when `data` must not be treated as mergeable text.

    Two independent disqualifiers, both byte-level facts rather than heuristics about
    "looking like" text:

    * a NUL byte anywhere -- git's own binary heuristic, and a reliable sign the file is not
      line-structured;
    * failure to decode as strict UTF-8 -- every file these six providers vendor is UTF-8
      Markdown/YAML/script text, so a decode failure means either a genuinely binary payload
      or an encoding this tool has no license to guess at.

    Deliberately NOT a size or content-sniffing heuristic: the only question here is "can this
    be split into lines and reassembled without inventing bytes", and both tests answer it
    without ambiguity.
    """
    if b"\x00" in data:
        return True
    try:
        data.decode("utf-8")
    except UnicodeDecodeError:
        return True
    return False


def _lines(data):
    """Decode to str and split into lines on ``\\n`` ONLY, keeping the separator.

    Two properties, both load-bearing.

    *Lossless*: ``"".join(_lines(x)) == x.decode("utf-8")`` for every input. A file whose only
    change is a missing or added trailing newline, or a CRLF/LF difference, is therefore a real
    line difference rather than something silently normalized away. This engine must never
    rewrite bytes it was not asked to change: a "helpful" newline normalization would surface as
    unexplained drift in the next run's `vendored_blob_sha` and would be indistinguishable from
    tampering. It would also quietly alter licence files -- three vendored `LICENSE` files end
    without a trailing newline, and these providers' ledgers assert byte-identity to upstream as
    a redistribution claim.

    *`\\n` only*: `str.splitlines()` is NOT used, because it also breaks on `\\x0b`, `\\x0c`,
    `\\x1c`-`\\x1e`, U+0085, U+2028 and U+2029. A form feed inside a Markdown paragraph would
    become a line boundary here while remaining mid-line to git and to every human reading the
    diff, so the two would disagree about what a "region" is. No vendored file currently
    contains such a character (all 603 checked), which is exactly why this is worth pinning now:
    the failure would arrive with some future upstream release, not today.
    """
    text = data.decode("utf-8")
    if not text:
        return []
    parts = text.split("\n")
    tail = parts.pop()
    lines = [part + "\n" for part in parts]
    if tail:
        lines.append(tail)
    return lines


def _align(base, other):
    """Map base-line index -> other-line index for every line inside a matching block.

    `autojunk=False` is required for determinism-in-practice: with the default,
    SequenceMatcher treats elements appearing in more than 1% of a sequence of length >= 200 as
    "popular" junk and ignores them when matching. That is still deterministic for a fixed
    input, but it makes the alignment depend on a density threshold that has nothing to do with
    the merge -- two files differing only in length can then align in qualitatively different
    ways. Disabling it keeps the alignment a pure function of content.
    """
    matcher = difflib.SequenceMatcher(a=base, b=other, autojunk=False)
    aligned = {}
    for base_start, other_start, size in matcher.get_matching_blocks():
        for offset in range(size):
            aligned[base_start + offset] = other_start + offset
    return aligned


def _chunk(base, ours, theirs):
    """Split the three sequences into ordered ('stable', lines) / ('unstable', b, o, t) chunks.

    A base line is a *sync point* only when both sides map it AND all three cursors sit exactly
    on it. Requiring cursor equality -- not merely "both sides matched this line somewhere" --
    is what keeps the three walks in lockstep; without it a line that appears twice could sync
    the walk to the wrong occurrence and silently drop or duplicate a region.

    Everything between two sync points is one unstable chunk carrying all three versions of the
    region, which `resolve()` then classifies. The trailing chunk covers whatever remains on
    either side after the last sync point.
    """
    align_ours = _align(base, ours)
    align_theirs = _align(base, theirs)
    n_base, n_ours, n_theirs = len(base), len(ours), len(theirs)
    chunks = []
    bi = oi = ti = 0

    def in_sync(index, o_cursor, t_cursor):
        return (index in align_ours and index in align_theirs
                and align_ours[index] == o_cursor and align_theirs[index] == t_cursor)

    while bi < n_base:
        if in_sync(bi, oi, ti):
            start = bi
            while bi < n_base and in_sync(bi, oi, ti):
                bi += 1
                oi += 1
                ti += 1
            chunks.append(("stable", base[start:bi]))
            continue
        # Scan forward for the next base line both sides agree on, at or after the cursors.
        # Requiring >= the current cursors prevents syncing backwards into already-consumed
        # text, which would duplicate content.
        probe = bi
        while probe < n_base and not (
                probe in align_ours and probe in align_theirs
                and align_ours[probe] >= oi and align_theirs[probe] >= ti):
            probe += 1
        if probe < n_base:
            o_next, t_next = align_ours[probe], align_theirs[probe]
        else:
            o_next, t_next = n_ours, n_theirs
        chunks.append(("unstable", base[bi:probe], ours[oi:o_next], theirs[ti:t_next]))
        bi, oi, ti = probe, o_next, t_next

    if oi < n_ours or ti < n_theirs:
        chunks.append(("unstable", [], ours[oi:n_ours], theirs[ti:n_theirs]))
    return chunks


def merge_text(base, ours, theirs):
    """Three-way merge three decoded line lists.

    Returns ``(ok, lines, conflicts)``. `conflicts` is a list of
    ``{"base": [...], "ours": [...], "theirs": [...]}`` dicts describing every region that could
    not be resolved -- reported rather than written into the output, because this tool never
    emits conflict markers into a vendored file. A conflict is a BLOCKED condition that a human
    resolves in a fresh review, not a mess to hand to the next reader.
    """
    merged = []
    conflicts = []
    for chunk in _chunk(base, ours, theirs):
        if chunk[0] == "stable":
            merged.extend(chunk[1])
            continue
        _, region_base, region_ours, region_theirs = chunk
        if region_ours == region_theirs:
            merged.extend(region_ours)          # both sides made the same edit
        elif region_ours == region_base:
            merged.extend(region_theirs)        # only upstream changed this region
        elif region_theirs == region_base:
            merged.extend(region_ours)          # only we changed this region (a local patch)
        else:
            conflicts.append({"base": region_base, "ours": region_ours,
                              "theirs": region_theirs})
    return (not conflicts), merged, conflicts


def resolve(base, ours, theirs):
    """Resolve one file's three-way state.

    Returns ``(verdict, content_bytes, conflicts)`` where `content_bytes` is the bytes to
    vendor (None when the verdict is not clean) and `conflicts` is the conflict-region list
    (empty unless the verdict is CONFLICT).

    The byte-equality rules are evaluated before any decoding so that a binary or undecodable
    file still resolves correctly whenever only one side moved. Only the genuinely
    both-sides-changed case needs text semantics, and that is the only case a binary file can
    block on.
    """
    if theirs == base:
        return RETAIN_OURS, ours, []
    if ours == base:
        return TAKE_THEIRS, theirs, []
    if ours == theirs:
        return CONVERGED, ours, []
    if is_binary(base) or is_binary(ours) or is_binary(theirs):
        return BINARY_CONFLICT, None, []
    ok, merged, conflicts = merge_text(_lines(base), _lines(ours), _lines(theirs))
    if not ok:
        return CONFLICT, None, conflicts
    return MERGED, "".join(merged).encode("utf-8"), []
