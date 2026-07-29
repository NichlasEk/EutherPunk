import unittest

from train_qlora import fit_example


class CharacterProcessor:
    def encode(self, text: str, *, add_special_tokens: bool) -> list[int]:
        self.assert_no_special_tokens = not add_special_tokens
        return [ord(character) for character in text]

    def decode(self, token_ids: list[int]) -> str:
        return "".join(chr(token_id) for token_id in token_ids)


def repair_example(user: str, completion: str) -> dict:
    return {
        "messages": [
            {"content": "repair"},
            {"content": user},
            {"content": completion},
        ]
    }


class FitExampleTest(unittest.TestCase):
    def test_preserves_short_completion_when_prompt_is_oversized(self) -> None:
        processor = CharacterProcessor()
        fitted, stats = fit_example(
            repair_example("x" * 500, "fixed"),
            processor,
            300,
            minimum_prompt_tokens=80,
        )

        self.assertLessEqual(len(fitted["prompt"] + fitted["completion"]), 298)
        self.assertEqual(fitted["completion"], "fixed</s>")
        self.assertTrue(stats["prompt_truncated"])
        self.assertFalse(stats["completion_truncated"])

    def test_giant_completion_keeps_supervised_tokens(self) -> None:
        processor = CharacterProcessor()
        fitted, stats = fit_example(
            repair_example("prompt", "y" * 500),
            processor,
            300,
            minimum_prompt_tokens=80,
        )

        self.assertGreater(len(fitted["completion"]), 0)
        self.assertLessEqual(len(fitted["prompt"] + fitted["completion"]), 298)
        self.assertTrue(stats["completion_truncated"])


if __name__ == "__main__":
    unittest.main()
