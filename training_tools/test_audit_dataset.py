import json
import os
import tempfile
import unittest
from pathlib import Path

from audit_dataset import audit


def example(identifier: str, group: str) -> dict:
    return {
        "schema_version": 1,
        "id": identifier,
        "group_id": group,
        "source_model": "test",
        "messages": [
            {"role": "system", "content": "repair"},
            {
                "role": "user",
                "content": json.dumps(
                    {
                        "task": "fix it",
                        "verified_diagnostics": "failed",
                        "current_files": [
                            {"path": "repair.go", "content": "return 0\n"}
                        ],
                    }
                ),
            },
            {
                "role": "assistant",
                "content": json.dumps(
                    {
                        "files": [
                            {"path": "repair.go", "content": "return 42\n"}
                        ]
                    }
                ),
            },
        ],
    }


class AuditDatasetTest(unittest.TestCase):
    def make_dataset(self, root: Path) -> None:
        train = root / "train.jsonl"
        holdout = root / "holdout.jsonl"
        manifest = root / "manifest.json"
        train.write_text(json.dumps(example("a", "train")) + "\n", encoding="utf-8")
        holdout.write_text(json.dumps(example("b", "holdout")) + "\n", encoding="utf-8")
        manifest.write_text(
            json.dumps({"train_examples": 1, "holdout_examples": 1}),
            encoding="utf-8",
        )
        for path in (train, holdout, manifest):
            os.chmod(path, 0o600)

    def test_valid_private_dataset(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.make_dataset(root)
            report = audit(root)
            self.assertEqual(report["examples"]["total"], 2)
            self.assertEqual(report["examples"]["group_overlap"], 0)

    def test_rejects_group_leakage(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.make_dataset(root)
            (root / "holdout.jsonl").write_text(
                json.dumps(example("b", "train")) + "\n", encoding="utf-8"
            )
            os.chmod(root / "holdout.jsonl", 0o600)
            with self.assertRaisesRegex(ValueError, "group overlap"):
                audit(root)

    def test_rejects_secret(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.make_dataset(root)
            value = example("a", "train")
            request = json.loads(value["messages"][1]["content"])
            request["task"] += " api_key = 'abcdefghijklmnopqrstuvwxyz123456'"
            value["messages"][1]["content"] = json.dumps(request)
            (root / "train.jsonl").write_text(
                json.dumps(value) + "\n", encoding="utf-8"
            )
            os.chmod(root / "train.jsonl", 0o600)
            with self.assertRaisesRegex(ValueError, "possible secrets"):
                audit(root)


if __name__ == "__main__":
    unittest.main()
