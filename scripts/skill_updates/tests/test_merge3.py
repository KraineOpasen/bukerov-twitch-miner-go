"""Byte-exact and property tests for the three-way merge engine.

The property tests are the important half. Example-based tests confirm the cases someone
thought of; the properties below hold for *every* input and are what justify trusting a merged
byte string that no human will inspect line by line.
"""

import itertools
import random
import unittest

from .. import merge3

B = lambda s: s.encode("utf-8")  # noqa: E731  (a one-character helper reads better inline)


class TestRuleOrder(unittest.TestCase):
    def test_upstream_unchanged_retains_ours(self):
        v, out, _ = merge3.resolve(B("a\nb\n"), B("a\nPATCH\nb\n"), B("a\nb\n"))
        self.assertEqual(v, merge3.RETAIN_OURS)
        self.assertEqual(out, B("a\nPATCH\nb\n"))

    def test_never_patched_takes_theirs(self):
        v, out, _ = merge3.resolve(B("a\nb\n"), B("a\nb\n"), B("a\nB2\n"))
        self.assertEqual(v, merge3.TAKE_THEIRS)
        self.assertEqual(out, B("a\nB2\n"))

    def test_converged(self):
        v, out, _ = merge3.resolve(B("a\n"), B("x\n"), B("x\n"))
        self.assertEqual(v, merge3.CONVERGED)
        self.assertEqual(out, B("x\n"))

    def test_disjoint_edits_merge(self):
        base = B("h\n1\n2\n3\n4\n5\n6\n7\n8\nt\n")
        ours = B("HEAD\n1\n2\n3\n4\n5\n6\n7\n8\nt\n")
        theirs = B("h\n1\n2\n3\n4\n5\n6\n7\n8\nTAIL\n")
        v, out, _ = merge3.resolve(base, ours, theirs)
        self.assertEqual(v, merge3.MERGED)
        self.assertEqual(out, B("HEAD\n1\n2\n3\n4\n5\n6\n7\n8\nTAIL\n"))

    def test_overlapping_edits_conflict(self):
        v, out, conflicts = merge3.resolve(B("a\nb\nc\n"), B("a\nO\nc\n"), B("a\nT\nc\n"))
        self.assertEqual(v, merge3.CONFLICT)
        self.assertIsNone(out)
        self.assertEqual(len(conflicts), 1)
        self.assertEqual(conflicts[0]["ours"], ["O\n"])
        self.assertEqual(conflicts[0]["theirs"], ["T\n"])


class TestBinary(unittest.TestCase):
    def test_nul_byte_is_binary(self):
        self.assertTrue(merge3.is_binary(b"a\x00b"))

    def test_invalid_utf8_is_binary(self):
        self.assertTrue(merge3.is_binary(b"\xff\xfe\xfa"))

    def test_plain_text_is_not_binary(self):
        self.assertFalse(merge3.is_binary("héllo — ok\n".encode("utf-8")))

    def test_binary_blocks_only_when_both_sides_moved(self):
        self.assertEqual(merge3.resolve(b"\x00A", b"\x00A", b"\x00C")[0], merge3.TAKE_THEIRS)
        self.assertEqual(merge3.resolve(b"\x00A", b"\x00B", b"\x00A")[0], merge3.RETAIN_OURS)
        self.assertEqual(merge3.resolve(b"\x00A", b"\x00B", b"\x00C")[0],
                         merge3.BINARY_CONFLICT)

    def test_binary_conflict_yields_no_content(self):
        self.assertIsNone(merge3.resolve(b"\x00A", b"\x00B", b"\x00C")[1])


class TestByteFidelity(unittest.TestCase):
    """The engine must never invent, drop or normalize a byte it was not asked to change."""

    SAMPLES = [b"", b"x", b"a\n", b"a\nb", b"a\r\nb\r\n", b"a\n\n\n", b"\n",
               "unicode — em dash\n".encode("utf-8"), b"no trailing newline"]

    def test_line_split_is_lossless(self):
        for sample in self.SAMPLES:
            self.assertEqual("".join(merge3._lines(sample)), sample.decode("utf-8"), sample)

    def test_trailing_newline_difference_is_a_real_change(self):
        v, out, _ = merge3.resolve(B("a\nb"), B("a\nb"), B("a\nb\n"))
        self.assertEqual(v, merge3.TAKE_THEIRS)
        self.assertEqual(out, B("a\nb\n"))

    def test_crlf_is_not_normalized(self):
        v, out, _ = merge3.resolve(B("a\r\n"), B("a\r\n"), B("a\r\nb\r\n"))
        self.assertEqual(out, B("a\r\nb\r\n"))

    def test_identical_inputs_are_returned_unchanged(self):
        for sample in self.SAMPLES:
            v, out, _ = merge3.resolve(sample, sample, sample)
            self.assertEqual(out, sample)


class TestProperties(unittest.TestCase):
    """Invariants that must hold for every input, checked over a deterministic random corpus.

    The seed is fixed so a failure is reproducible; the point is breadth of shapes, not
    unpredictability.
    """

    def corpus(self, n=260):
        rng = random.Random(20260817)
        alphabet = ["alpha\n", "beta\n", "gamma\n", "delta\n", "eps\n", "\n", "tail"]
        for _ in range(n):
            base = [rng.choice(alphabet) for _ in range(rng.randint(0, 9))]

            def mutate(seq):
                out = list(seq)
                for _ in range(rng.randint(0, 3)):
                    if out and rng.random() < 0.4:
                        del out[rng.randrange(len(out))]
                    elif out and rng.random() < 0.5:
                        out[rng.randrange(len(out))] = rng.choice(alphabet)
                    else:
                        out.insert(rng.randint(0, len(out)), rng.choice(alphabet))
                return out

            yield ("".join(base).encode(), "".join(mutate(base)).encode(),
                   "".join(mutate(base)).encode())

    def test_clean_result_is_always_valid_utf8_and_never_carries_conflict_markers(self):
        for base, ours, theirs in self.corpus():
            verdict, out, _ = merge3.resolve(base, ours, theirs)
            if verdict in merge3.CLEAN_VERDICTS:
                self.assertIsNotNone(out)
                out.decode("utf-8")
                for marker in (b"<<<<<<<", b"=======", b">>>>>>>"):
                    self.assertNotIn(marker, out)

    def test_identity_property_one_sided_changes(self):
        """If only one side moved, the answer is that side -- never a merge, never a conflict."""
        for base, ours, theirs in self.corpus(120):
            self.assertEqual(merge3.resolve(base, ours, base)[1], ours)
            self.assertEqual(merge3.resolve(base, base, theirs)[1], theirs)

    def test_symmetry_of_conflict_detection(self):
        """Swapping OURS and THEIRS must not change whether the merge is clean.

        A merge engine that conflicts one way round and not the other would make the verdict
        depend on which repository happened to be called 'ours' -- an arbitrary label.
        """
        for base, ours, theirs in self.corpus():
            forward = merge3.resolve(base, ours, theirs)[0]
            reverse = merge3.resolve(base, theirs, ours)[0]
            conflicted = {merge3.CONFLICT, merge3.BINARY_CONFLICT}
            self.assertEqual(forward in conflicted, reverse in conflicted,
                             (base, ours, theirs, forward, reverse))

    def test_determinism_across_repeats(self):
        for base, ours, theirs in self.corpus(60):
            results = {merge3.resolve(base, ours, theirs)[:2] for _ in range(8)}
            self.assertEqual(len(results), 1)

    def test_merged_output_contains_only_lines_from_the_inputs(self):
        """No line in a clean merge may be fabricated: every one came from base, ours or theirs."""
        for base, ours, theirs in self.corpus():
            verdict, out, _ = merge3.resolve(base, ours, theirs)
            if verdict != merge3.MERGED:
                continue
            allowed = set(itertools.chain(merge3._lines(base), merge3._lines(ours),
                                          merge3._lines(theirs)))
            for line in merge3._lines(out):
                self.assertIn(line, allowed)


if __name__ == "__main__":
    unittest.main()
