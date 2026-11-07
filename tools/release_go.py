#!/usr/bin/env python3
"""Release pipeline of Klip (the Go program, 1.0.11 and later).

It makes the program folder (Klip.exe and mygo-sqlite3.dll), the Inno Setup
installer, the light-update zip with its maccy-package.json, and the update
manifest, all for a GitHub release (.github/workflows/release.yml uploads
them): the installed versions read the manifest from the newest release.

    release_go.py build    --version 1.0.11                  # artifacts/publish/win-x64
    release_go.py package  --version 1.0.11
    release_go.py release  --version 1.0.11 --notes-file notes.md   # artifacts/release

Run it on any system with Go: the program is cross-compiled. The installer
needs Inno Setup, so `release` is for Windows; `build` and `package` are not.
"""
from __future__ import annotations

import argparse
import datetime
import hashlib
import io
import json
import os
import re
import shutil
import subprocess
import sys
import urllib.request
import zipfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import release as base  # noqa: E402  (the C# pipeline's helpers)

REPO = HERE.parent
APP = REPO / "app"
GO_ARCH = {"win-x64": "amd64", "win-arm64": "arm64"}
WINRES = "github.com/tc-hib/go-winres@v0.3.3"
EXE = "Klip.exe"  # update.ExeName: every version from 1.0.11 on checks for it
RELEASES = "https://github.com/Achordchan/klip/releases/download"
# The files of the C# program, which the installer removes from a folder it
# installs over (the 1.0.9 and 1.0.10 packages, the last ones it had).
LEGACY_CSHARP = REPO / "installer" / "legacy-csharp-files.json"

# What a package must never hold: the user's data, as UpdateService.cs lists it.
PROTECTED_ROOTS = ("images", "files", "blobs", "sync", "updates")
PROTECTED_FILES = (
    "settings.json", "history.json", "preferences_last_error.txt",
    "sync_last_error.txt", "auth_last_error.txt", "capture_debug.log",
)


def is_protected(rel: str) -> bool:
    p = rel.replace("\\", "/").lstrip("/").lower()
    if p in PROTECTED_FILES:
        return True
    return any(p == r or p.startswith(r + "/") for r in PROTECTED_ROOTS)


def sha256_file(path: Path) -> str:
    return base._sha256_file(path)  # upper-case hex, as the manifest spells it


def run(cmd: list[str], cwd: Path, env: dict | None = None) -> None:
    print("+", " ".join(cmd), flush=True)
    full = dict(os.environ)
    if env:
        full.update(env)
    subprocess.run(cmd, cwd=str(cwd), env=full, check=True)


# ---- build ---------------------------------------------------------------


def sqlite_library(runtime: str) -> tuple[str, str, str]:
    """(file name, url, sha256) of the SQLite library MyGo's plugin loads."""
    out = subprocess.run(["go", "list", "-m", "-f", "{{.Dir}}", "github.com/egoist/mygo"],
                         cwd=str(APP), capture_output=True, text=True, check=True).stdout.strip()
    meta = json.loads((Path(out) / "plugins" / "sqlite" / "mygo-plugin.json").read_text(encoding="utf-8"))
    key = "windows-" + GO_ARCH[runtime]
    for lib in meta["libraries"]:
        if lib["name"] == "mygo-sqlite3":
            f = lib["files"][key]
            return f["name"], f["url"], f["sha256"]
    raise RuntimeError("mygo-sqlite3 not listed in mygo-plugin.json")


def fetch_sqlite(runtime: str, dest_dir: Path, cache: Path) -> Path:
    name, url, want = sqlite_library(runtime)
    cache.mkdir(parents=True, exist_ok=True)
    cached = cache / (want + "-" + name)
    if not cached.exists() or hashlib.sha256(cached.read_bytes()).hexdigest() != want:
        print("downloading", url, flush=True)
        with urllib.request.urlopen(url, timeout=120) as r:
            data = r.read()
        if hashlib.sha256(data).hexdigest() != want:
            raise RuntimeError("sha256 of " + name + " does not match mygo-plugin.json")
        cached.write_bytes(data)
    dest = dest_dir / name
    shutil.copyfile(cached, dest)
    return dest


GENERATED_PACKAGE = re.compile(r"(?:maccy|Klip)-[^/]+-(?:" + "|".join(re.escape(r) for r in GO_ARCH) + r")\.zip")

# The program as built before 1.0.11 (maccy.exe) counts too: a folder an
# earlier build of this checkout left is cleared as well.
BUILD_OUTPUT = re.compile(r"Klip\.exe|maccy\.exe|maccy-package\.json|mygo-sqlite3[^/]*\.dll|" + GENERATED_PACKAGE.pattern)


def check_disposable(publish_dir: Path) -> None:
    """build clears the publish folder first. Refuse a folder that is not the
    leftover of an earlier build: this checkout or any folder around it, a
    folder with a repository or the user's data in it, or one that holds
    anything else than what a build makes."""
    pub = publish_dir.resolve()
    repo = REPO.resolve()
    if pub == repo or pub in repo.parents:
        raise RuntimeError(f"refusing to clear {pub}: it is, or holds, this checkout")
    if pub == Path(pub.anchor) or pub == Path.home():
        raise RuntimeError(f"refusing to clear {pub}")
    entries = list(pub.rglob("*"))
    if not entries:
        return
    if not any((pub / name).is_file() for name in (EXE, "maccy.exe")):
        raise RuntimeError(f"refusing to clear {pub}: it holds files but no {EXE} (or maccy.exe) from an earlier build")
    # Only what a build puts there, nothing else: the program, the SQLite
    # library, a package made from it. A folder with anything more is somebody's.
    for path in entries:
        rel = path.relative_to(pub).as_posix()
        if not (path.is_file() and BUILD_OUTPUT.fullmatch(rel)):
            raise RuntimeError(f"refusing to clear {pub}: {rel} is not something a build makes")


def build_publish(version: str, runtime: str, publish_dir: Path, manifest_url: str = "") -> None:
    """Klip.exe (a Windows GUI program with icon and version) and the SQLite
    library, in publish_dir. manifest_url is for a test build only (the end to
    end test of an update, tools/e2e_update.py): a release reads the address
    built into the program."""
    if runtime not in GO_ARCH:
        raise ValueError("runtime must be one of " + ", ".join(GO_ARCH))
    arch = GO_ARCH[runtime]
    if publish_dir.exists():
        check_disposable(publish_dir)
        shutil.rmtree(publish_dir)
    publish_dir.mkdir(parents=True)

    cmd_dir = APP / "cmd" / "klip"
    for old in cmd_dir.glob("*.syso"):
        old.unlink()
    try:
        run(["go", "run", WINRES, "make", "--in", "winres/winres.json", "--out", "rsrc", "--arch", arch,
             "--product-version", version, "--file-version", version + ".0"], cwd=cmd_dir)
        ldflags = f"-s -w -H=windowsgui -X klip/internal/version.Version={version}"
        if manifest_url:
            ldflags += f" -X klip/internal/app.UpdateManifestURL={manifest_url}"
        run(["go", "build", "-trimpath", "-ldflags", ldflags,
             "-o", str(publish_dir / EXE), "./cmd/klip"],
            cwd=APP, env={"GOOS": "windows", "GOARCH": arch, "CGO_ENABLED": "0"})
    finally:
        for syso in cmd_dir.glob("*.syso"):
            syso.unlink()
    fetch_sqlite(runtime, publish_dir, REPO / "artifacts" / "cache")


# ---- package -------------------------------------------------------------


def package_file_list(publish_dir: Path, exclude: Path | None = None) -> list[dict]:
    files = []
    for path in sorted(publish_dir.rglob("*")):
        if not path.is_file():
            continue
        rel = path.relative_to(publish_dir).as_posix()
        if rel == "maccy-package.json" or (exclude is not None and path.resolve() == exclude.resolve()):
            continue
        if GENERATED_PACKAGE.fullmatch(path.name):
            continue  # a package of an earlier version, left in the folder it was made from
        if is_protected(rel):
            raise RuntimeError("publish folder holds user data, not for a package: " + rel)
        files.append({"path": rel, "sha256": sha256_file(path), "size": path.stat().st_size})
    if not any(f["path"].lower() == EXE.lower() for f in files):
        raise RuntimeError("publish folder has no " + EXE)
    return files


def legacy_files_from(source: str, new_paths: set[str]) -> list[str]:
    """The files of the earlier program: the list of an earlier package
    (a zip path or URL, a package list as JSON, or "manifest[:path]" for the
    package the manifest names now), without what the new package has and
    without user data."""
    if source == "manifest" or source.startswith("manifest:"):
        mpath = Path(source.split(":", 1)[1]) if ":" in source else REPO / "docs" / "updates" / "manifest.json"
        data = json.loads(mpath.read_text(encoding="utf-8"))
        pkg = (data.get("latest") or {}).get("package") or {}
        if not pkg.get("url"):
            raise RuntimeError("the manifest names no package to take the old file list from")
        source = pkg["url"]
        want_sha = str(pkg.get("sha256") or "").strip().lower()
    else:
        want_sha = ""
    if source.lower().endswith(".json"):
        listed = json.loads(Path(source).read_text(encoding="utf-8-sig"))
        zf = None
    elif re.match(r"^https?://", source):
        print("downloading", source, flush=True)
        with urllib.request.urlopen(source, timeout=300) as r:
            blob = r.read()
        if want_sha and hashlib.sha256(blob).hexdigest() != want_sha:
            raise RuntimeError("the old package does not match the checksum the manifest gives for it")
        zf = zipfile.ZipFile(io.BytesIO(blob))
    else:
        zf = zipfile.ZipFile(source)
    if zf is not None:
        with zf:
            listed = json.loads(zf.read("maccy-package.json").decode("utf-8-sig"))
    out = []
    lowered = {p.lower() for p in new_paths}
    # The files of that package, and the old files it was still carrying out of
    # the releases before it: a fresh 1.0.10 installation going straight to a
    # later release has both kinds to be rid of.
    names = [str(f["path"]) for f in listed.get("files", [])] + [str(p) for p in listed.get("legacyFiles", [])]
    seen = set()
    for name in names:
        rel = name.replace("\\", "/")
        if rel.lower() in lowered or rel.lower() in seen or is_protected(rel) or rel == "maccy-package.json":
            continue
        seen.add(rel.lower())
        out.append(rel)
    return sorted(out, key=str.lower)


def create_package(publish_dir: Path, output_dir: Path, *, version: str, runtime: str,
                   legacy_from: str | None = None) -> Path:
    package = output_dir / f"Klip-{version}-{runtime}.zip"
    # The archive being made may sit in the folder it is made from (and a
    # second run finds the first one's result there): it is not one of its own files.
    files = package_file_list(publish_dir, exclude=package)
    manifest: dict = {"appId": "maccy", "version": version, "runtime": runtime, "files": files}
    if legacy_from:
        legacy = legacy_files_from(legacy_from, {f["path"] for f in files})
        if legacy:
            manifest["legacyFiles"] = legacy
    output_dir.mkdir(parents=True, exist_ok=True)
    if package.exists():
        package.unlink()
    with zipfile.ZipFile(package, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as zf:
        for f in files:
            zf.write(publish_dir / f["path"], f["path"])
        zf.writestr("maccy-package.json", json.dumps(manifest, ensure_ascii=False, indent=2) + "\n")
    return package


# ---- the whole release ---------------------------------------------------


_UNSAFE_PART = re.compile(r'[<>:"|?*{}\[\]]|~\d|[. ]$')


def safe_legacy_path(rel: str) -> bool:
    """A name that may become a deletion rule: relative, inside the folder, one
    plain file, nothing the installer would expand (wildcards, {constants}) or
    Windows would read as another file."""
    if not rel or rel.startswith(("/", "\\")) or re.match(r"^[A-Za-z]:", rel):
        return False
    parts = rel.replace("\\", "/").split("/")
    return all(p and p not in (".", "..") and not _UNSAFE_PART.search(p) for p in parts)


def write_legacy_delete(publish_dir: Path, legacy_from: str | None, out: Path | None = None) -> Path:
    """installer/legacy-delete.iss: Pascal statements (included in klip.iss's
    [Code], run once everything is installed) deleting exactly the files of the
    earlier program (the list of its package), none when it is not known.
    Wildcards would also take what is not ours."""
    path = out or REPO / "installer" / "legacy-delete.iss"
    lines = ["// written by tools/release_go.py: the files of the earlier program, by name",
             # The program file of every version before 1.0.11, known or not:
             # left, the sign-in entry of the C# program (which names it) would
             # start the old program, which takes the single-instance mutex first.
             "DeleteLegacy('maccy.exe');"]
    if legacy_from:
        new_paths = {f["path"] for f in package_file_list(publish_dir)}
        for rel in legacy_files_from(legacy_from, new_paths):
            if not safe_legacy_path(rel):
                print("not deleted by the installer (unsafe name): " + rel, flush=True)
                continue
            if rel.lower() == "maccy.exe":
                continue  # named above
            name = rel.replace("/", "\\").replace("'", "''")
            lines.append("DeleteLegacy('" + name + "');")
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return path


def build_installer(version: str, runtime: str, iscc_arg: str | None, publish_dir: Path, legacy_from: str | None = None) -> Path:
    """The installer of the program built in publish_dir. The script is for
    64-bit Intel/AMD Windows (there are no 32-bit users to serve)."""
    if runtime != "win-x64":
        raise ValueError(f"the installer is for win-x64 only, not {runtime}")
    iss = REPO / "installer" / "klip.iss"
    iscc = base._find_iscc(iscc_arg or None)
    base._set_iss_version(iss, version)
    write_legacy_delete(publish_dir, legacy_from)
    run([str(iscc), "/DGoBuild", f"/DMyPublishDir={publish_dir.resolve()}", str(iss)], cwd=REPO)
    out = REPO / "artifacts" / "installer" / f"Klip-{version}-setup.exe"
    if not out.exists():
        raise FileNotFoundError(str(out))
    return out


def write_manifest(path: Path, *, version: str, mandatory: bool, notes: str, base_url: str,
                   installer: Path, package: Path, runtime: str) -> None:
    """The update manifest (update.Manifest) of this release, with the
    addresses of its own files: base_url/v<version>/<file name>. Never the
    "latest" address: a client that read this manifest must get these very
    files, whatever has been released since."""
    root = base_url.rstrip("/") + f"/v{version}/"
    data = {
        "appId": "maccy",
        "latest": {
            "version": version,
            "mandatory": bool(mandatory),
            "notes": notes,
            "publishedAt": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "installer": {"url": root + installer.name, "sha256": sha256_file(installer), "size": installer.stat().st_size},
            "package": {"kind": "zip", "runtime": runtime, "url": root + package.name,
                        "sha256": sha256_file(package), "size": package.stat().st_size},
        },
    }
    path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def cmd_release(a: argparse.Namespace) -> int:
    """The files of a GitHub release, in out_dir: the installer, the package
    and manifest.json."""
    version = base._validate_version(a.version)
    notes = a.notes
    if a.notes_file:
        notes = Path(a.notes_file).read_text(encoding="utf-8")
    notes = notes.replace("\r\n", "\n").strip("\n")
    if not re.match(r"^https://", a.base_url):
        raise ValueError("--base-url must be an https address")
    publish_dir = (Path(a.publish_dir) if a.publish_dir else REPO / "artifacts" / "publish" / a.runtime).resolve()
    out_dir = (Path(a.out_dir) if a.out_dir else REPO / "artifacts" / "release").resolve()
    legacy = a.legacy_from or None

    print(f"[1/4] build the program ({a.runtime})")
    build_publish(version, a.runtime, publish_dir)
    print("[2/4] installer (Inno Setup)")
    installer = build_installer(version, a.runtime, a.iscc, publish_dir, legacy)
    print("[3/4] light-update package")
    package = create_package(publish_dir, out_dir, version=version, runtime=a.runtime, legacy_from=legacy)
    print("[4/4] manifest")
    out_dir.mkdir(parents=True, exist_ok=True)
    final_installer = out_dir / installer.name
    if final_installer.resolve() != installer.resolve():
        shutil.copyfile(installer, final_installer)
    manifest = out_dir / "manifest.json"
    write_manifest(manifest, version=version, mandatory=a.mandatory, notes=notes, base_url=a.base_url,
                   installer=final_installer, package=package, runtime=a.runtime)
    print(json.dumps({"installer": str(final_installer), "package": str(package), "manifest": str(manifest)}, indent=2))
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="release_go.py", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    def common(p: argparse.ArgumentParser) -> None:
        p.add_argument("--version", required=True)
        p.add_argument("--runtime", default="win-x64")

    b = sub.add_parser("build", help="Klip.exe and the SQLite library into artifacts/publish/<runtime>")
    common(b)
    b.add_argument("--publish-dir", default="")
    b.add_argument("--manifest-url", default="", help="test builds only: the update manifest to read")

    pk = sub.add_parser("package", help="the light-update zip from a publish folder")
    common(pk)
    pk.add_argument("--publish-dir", default="")
    pk.add_argument("--out-dir", default="")
    pk.add_argument("--legacy-from", default="", help='zip path or URL of the earlier package, a package list (.json), or "manifest"')

    r = sub.add_parser("release", help="build, installer, package and manifest of a GitHub release")
    common(r)
    r.add_argument("--notes", default="")
    r.add_argument("--notes-file", default="")
    r.add_argument("--mandatory", action="store_true")
    r.add_argument("--base-url", default=RELEASES, help="where the release's files are downloaded from (<base>/v<version>/<file>)")
    r.add_argument("--iscc", default="")
    r.add_argument("--legacy-from", default=str(LEGACY_CSHARP), help="the earlier program's files, for the installer to remove")
    r.add_argument("--publish-dir", default="")
    r.add_argument("--out-dir", default="")

    a = ap.parse_args(argv)
    version = base._validate_version(a.version)
    if a.cmd == "build":
        build_publish(version, a.runtime, (Path(a.publish_dir) if a.publish_dir else REPO / "artifacts" / "publish" / a.runtime).resolve(),
                      a.manifest_url)
        return 0
    if a.cmd == "package":
        pub = (Path(a.publish_dir) if a.publish_dir else REPO / "artifacts" / "publish" / a.runtime).resolve()
        out = (Path(a.out_dir) if a.out_dir else REPO / "artifacts" / "installer").resolve()
        p = create_package(pub, out, version=version, runtime=a.runtime, legacy_from=a.legacy_from or None)
        print(json.dumps({"path": str(p), "sha256": sha256_file(p), "size": p.stat().st_size}))
        return 0
    return cmd_release(a)


if __name__ == "__main__":
    sys.exit(main())
