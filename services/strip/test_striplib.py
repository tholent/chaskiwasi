"""Golden-corpus test for the real strip service's core logic.

Runs services/strip/testdata/replies/*.eml through striplib.strip() (the
exact code app.py calls) and compares against the matching *.expected.txt.

Run with:  python3 -m unittest test_striplib.py -v
(or, from a checkout with the requirements installed: `python3 -m unittest
discover -s services/strip`.)

Deliberately unittest, stdlib only — no pytest dependency to pin or install
just for five fixture comparisons.
"""

from __future__ import annotations

import email
import glob
import os
import unittest

from striplib import strip

TESTDATA_DIR = os.path.join(os.path.dirname(__file__), "testdata", "replies")


def _load_case(eml_path: str) -> tuple[str, bool]:
    with open(eml_path, "rb") as f:
        msg = email.message_from_binary_file(f)
    charset = msg.get_content_charset() or "utf-8"
    text = msg.get_payload(decode=True).decode(charset)
    format_flowed = "format=flowed" in (msg.get("Content-Type") or "")
    return text, format_flowed


class GoldenCorpusTest(unittest.TestCase):
    def test_corpus_matches_expected_output(self):
        eml_paths = sorted(glob.glob(os.path.join(TESTDATA_DIR, "*.eml")))
        self.assertTrue(eml_paths, f"no fixtures found under {TESTDATA_DIR}")

        for eml_path in eml_paths:
            expected_path = eml_path[: -len(".eml")] + ".expected.txt"
            with self.subTest(case=os.path.basename(eml_path)):
                text, format_flowed = _load_case(eml_path)
                with open(expected_path, encoding="utf-8") as f:
                    expected_body = f.read()

                result = strip(text, format_flowed)
                self.assertEqual(result.body, expected_body)

    def test_signature_only_is_not_touched_by_quotations(self):
        # talon.quotations strips *quoted reply chains*, not sender
        # signatures — that split matters here because the Go fallback
        # (internal/strip, §5.3) is deliberately more aggressive and *does*
        # cut at "-- " when this service is unreachable. This test pins
        # down the real service's honest behavior so that asymmetry stays
        # visible rather than being "discovered" later by a confused reader
        # comparing the two.
        path = os.path.join(TESTDATA_DIR, "signature_delimiter.eml")
        text, format_flowed = _load_case(path)
        result = strip(text, format_flowed)
        self.assertFalse(result.trimmed)
        self.assertEqual(result.removed_bytes, 0)
        self.assertIn("Uncle Theo", result.body)

    def test_trimmed_ignores_trailing_whitespace_only_changes(self):
        # talon normalises a trailing newline even with nothing to quote-
        # strip; that must not surface as trimmed=True; §4.3 defines
        # `trimmed` as "a quoted tail was removed", not "bytes changed".
        result = strip("just a plain letter, no quoting here\n", False)
        self.assertFalse(result.trimmed)
        self.assertEqual(result.removed_bytes, 0)

    def test_format_flowed_soft_break_keeps_the_word_space(self):
        # RFC 3676 §4.5, DelSp=no (the default, and the only mode this
        # service supports — the wire contract has no DelSp field): the
        # trailing space on a soft-wrapped line IS the word separator and
        # must survive unwrapping, or "reading" + "every" glues into
        # "readingevery" on the device screen.
        flowed = "one two three \nfour five six\n"
        result = strip(flowed, True)
        self.assertEqual(result.body, "one two three four five six")


if __name__ == "__main__":
    unittest.main()
