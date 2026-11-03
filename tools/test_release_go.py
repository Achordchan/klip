import tempfile
import subprocess
import unittest
from pathlib import Path

from release_go import commit_manifest, package_file_list


class ManifestCommitTests(unittest.TestCase):
    def test_nested_generated_archives_are_excluded(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / "maccy.exe").write_bytes(b"app")
            (root / "packages").mkdir()
            (root / "packages/maccy-1.0.1-win-x64.zip").write_bytes(b"old")
            self.assertEqual([f["path"] for f in package_file_list(root)], ["maccy.exe"])

    def test_empty_success_and_rejected_commit(self):
        with tempfile.TemporaryDirectory() as folder:
            repo = Path(folder)
            def git(*args):
                return subprocess.run(["git", *args], cwd=repo, check=True,
                                      stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            git("init")
            git("config", "user.name", "Test")
            git("config", "user.email", "test@example.invalid")
            commit_manifest(repo, "1.0")
            (repo / "manifest.json").write_text("{}")
            git("add", "manifest.json")
            hook = repo / ".git/hooks/pre-commit"
            hook.write_text("#!/bin/sh\nexit 1\n")
            hook.chmod(0o755)
            with self.assertRaises(SystemExit):
                commit_manifest(repo, "1.0")
            hook.unlink()
            commit_manifest(repo, "1.0")
            self.assertIn(b"update manifest for v1.0", git("log", "-1", "--format=%s").stdout)
            commit_manifest(repo, "1.0")


if __name__ == "__main__":
    unittest.main()
