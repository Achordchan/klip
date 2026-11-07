import json
import tempfile
import unittest
from pathlib import Path

from release_go import LEGACY_CSHARP, legacy_files_from, package_file_list, write_legacy_delete


class ReleaseToolTests(unittest.TestCase):
    def test_nested_generated_archives_are_excluded(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / "Klip.exe").write_bytes(b"app")
            (root / "packages").mkdir()
            (root / "packages/maccy-1.0.1-win-x64.zip").write_bytes(b"old")
            (root / "packages/Klip-1.0.12-win-x64.zip").write_bytes(b"old")
            self.assertEqual([f["path"] for f in package_file_list(root)], ["Klip.exe"])

    def test_the_csharp_file_list_is_read(self):
        # The list committed for the installer: the old program file and its
        # .NET libraries, never what the new program has or the user's data.
        legacy = legacy_files_from(str(LEGACY_CSHARP), {"Klip.exe", "mygo-sqlite3.dll"})
        lowered = {p.lower() for p in legacy}
        self.assertIn("maccy.exe", lowered)
        self.assertIn("maccy.dll", lowered)
        self.assertNotIn("klip.exe", lowered)
        for p in lowered:
            self.assertFalse(p.startswith(("images/", "files/", "blobs/", "sync/", "updates/")), p)
            self.assertNotIn(p, ("settings.json", "history.json"))

    def test_the_old_program_file_is_deleted_without_a_list(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / "Klip.exe").write_bytes(b"app")
            out = write_legacy_delete(root, None, root / "legacy-delete.iss")
            self.assertEqual(out.read_text(encoding="utf-8").count("DeleteLegacy('maccy.exe');"), 1)

    def test_the_csharp_file_list_is_well_formed(self):
        data = json.loads(LEGACY_CSHARP.read_text(encoding="utf-8"))
        self.assertGreater(len(data["files"]), 10)
        self.assertTrue(all("/" not in f["path"] or not f["path"].startswith("/") for f in data["files"]))


if __name__ == "__main__":
    unittest.main()
