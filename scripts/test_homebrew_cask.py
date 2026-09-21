import hashlib
from pathlib import Path
import runpy
import tempfile
import unittest

render = runpy.run_path(str(Path(__file__).with_name("homebrew-cask.py")))["render"]


class CaskReleaseTests(unittest.TestCase):
    def test_both_architectures_pin_the_exact_artifacts(self):
        with tempfile.TemporaryDirectory() as directory:
            artifacts = Path(directory)
            payloads = {"arm64": b"signed arm payload", "amd64": b"signed intel payload"}
            for arch, payload in payloads.items():
                (artifacts / f"quesma-shipper-darwin-{arch}").write_bytes(payload)
            cask = render("0.1.0-123.abcdef", artifacts)
            for payload in payloads.values():
                self.assertIn(hashlib.sha256(payload).hexdigest(), cask)
            self.assertIn('version "0.1.0-123.abcdef"', cask)
            self.assertIn("auto_updates true", cask)
            self.assertNotIn("@", cask)

    def test_invalid_release_cannot_inject_ruby(self):
        for version in ('1.0.0"; system("oops")', "latest", "../1.0.0", "1.0.0\n"):
            with self.subTest(version=version), self.assertRaises(ValueError):
                render(version, Path("unused"))

    def test_missing_architecture_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(FileNotFoundError):
                render("1.0.0", Path(directory))


if __name__ == "__main__":
    unittest.main()
