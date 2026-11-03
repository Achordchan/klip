#!/usr/bin/env python3
"""Release pipeline of the Go program (Klip 1.0.11 and later).

It makes what the C# pipeline (release.py) makes and what the clients that
exist expect: the program folder (maccy.exe and mygo-sqlite3.dll), the Inno
Setup installer, the light-update zip with its maccy-package.json, and the
entry in docs/updates/manifest.json. release.py's helpers for Inno Setup, the
manifest and Gitee are used as they are.

    release_go.py build    --version 1.0.11                  # artifacts/publish/win-x64
    release_go.py package  --version 1.0.11 --legacy-from manifest
    release_go.py release  --version 1.0.11 --notes-file notes.txt --legacy-from manifest
    release_go.py release  ... --publish-gitee --yes          # also upload and push

Run it on any system with Go: the program is cross-compiled. The installer
needs Inno Setup, so `release` is for Windows; `build` and `package` are not.
"""
from __future__ import annotations

import argparse
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


GENERATED_PACKAGE = re.compile(r"maccy-[^/]+-(?:" + "|".join(re.escape(r) for r in GO_ARCH) + r")\.zip")

BUILD_OUTPUT = re.compile(r"maccy\.exe|maccy-package\.json|mygo-sqlite3[^/]*\.dll|" + GENERATED_PACKAGE.pattern)


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
    if not (pub / "maccy.exe").is_file():
        raise RuntimeError(f"refusing to clear {pub}: it holds files but no maccy.exe from an earlier build")
    # Only what a build puts there, nothing else: the program, the SQLite
    # library, a package made from it. A folder with anything more is somebody's.
    for path in entries:
        rel = path.relative_to(pub).as_posix()
        if not (path.is_file() and BUILD_OUTPUT.fullmatch(rel)):
            raise RuntimeError(f"refusing to clear {pub}: {rel} is not something a build makes")


def build_publish(version: str, runtime: str, publish_dir: Path) -> None:
    """maccy.exe (a Windows GUI program with icon and version) and the SQLite
    library, in publish_dir."""
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
        run(["go", "build", "-trimpath",
             "-ldflags", f"-s -w -H=windowsgui -X klip/internal/version.Version={version}",
             "-o", str(publish_dir / "maccy.exe"), "./cmd/klip"],
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
    if not any(f["path"].lower() == "maccy.exe" for f in files):
        raise RuntimeError("publish folder has no maccy.exe")
    return files


def legacy_files_from(source: str, new_paths: set[str]) -> list[str]:
    """The files of the earlier program: the list of an earlier package
    (a zip path or URL, or "manifest[:path]" for the package the manifest
    names now), without what the new package has and without user data."""
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
    if re.match(r"^https?://", source):
        print("downloading", source, flush=True)
        with urllib.request.urlopen(source, timeout=300) as r:
            blob = r.read()
        if want_sha and hashlib.sha256(blob).hexdigest() != want_sha:
            raise RuntimeError("the old package does not match the checksum the manifest gives for it")
        zf = zipfile.ZipFile(io.BytesIO(blob))
    else:
        zf = zipfile.ZipFile(source)
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
    package = output_dir / f"maccy-{version}-{runtime}.zip"
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
    lines = ["// written by tools/release_go.py: the files of the earlier program, by name"]
    if legacy_from:
        new_paths = {f["path"] for f in package_file_list(publish_dir)}
        for rel in legacy_files_from(legacy_from, new_paths):
            if not safe_legacy_path(rel):
                print("not deleted by the installer (unsafe name): " + rel, flush=True)
                continue
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
    out = REPO / "artifacts" / "installer" / f"maccy-{version}-setup.exe"
    if not out.exists():
        raise FileNotFoundError(str(out))
    return out


def cmd_release(a: argparse.Namespace) -> int:
    version = base._validate_version(a.version)
    notes = a.notes
    if a.notes_file:
        notes = Path(a.notes_file).read_text(encoding="utf-8")
    notes = notes.replace("\r\n", "\n").rstrip("\n")
    manifest = Path(a.manifest)
    # Where the downloads will be: needed before anything is built, as the
    # manifest is rewritten with it. Gitee fills it in itself when publishing.
    base_url = a.base_url or base._infer_base_url_from_manifest(manifest)
    if not a.publish_gitee and not base_url:
        raise ValueError("give --base-url (the manifest has none to take it from), or use --publish-gitee")
    publish_dir = (REPO / "artifacts" / "publish" / a.runtime).resolve()
    installer_dir = REPO / "artifacts" / "installer"

    print(f"[1/5] build the program ({a.runtime})")
    build_publish(version, a.runtime, publish_dir)
    print("[2/5] installer (Inno Setup)")
    installer = build_installer(version, a.runtime, a.iscc, publish_dir, a.legacy_from or None)
    print("[3/5] light-update package")
    package = create_package(publish_dir, installer_dir, version=version, runtime=a.runtime, legacy_from=a.legacy_from)
    sha, size = sha256_file(installer), installer.stat().st_size
    psha, psize = sha256_file(package), package.stat().st_size

    installer_url = package_url = None
    if a.publish_gitee:
        if not a.yes:
            raise ValueError("--publish-gitee needs --yes")
        owner, repo_name = a.gitee_owner, a.gitee_repo
        inferred = base._infer_repo_from_git(REPO)
        if inferred:
            owner, repo_name = owner or inferred[0], repo_name or inferred[1]
        token = a.gitee_token or os.environ.get("GITEE_TOKEN", "")
        if not (owner and repo_name and token):
            raise ValueError("missing gitee owner/repo/token")
        print("[4/5] publish to Gitee")
        published = base._publish_to_gitee(REPO, owner=owner, repo_name=repo_name, token=token, version=version,
                                           notes=notes, installer_path=installer, package_path=package)
        installer_url, package_url = published.get("installer"), published.get("package")

    print("[5/5] manifest")
    base._update_manifest(manifest, version=version, mandatory=a.mandatory, notes=notes,
                          base_url=base_url or "https://gitee.com", sha256=sha, size=size, installer_url=installer_url,
                          package_sha256=psha, package_size=psize, package_runtime=a.runtime, package_url=package_url)
    if a.publish_gitee:
        base._git(REPO, ["add", str(manifest)])
        commit_manifest(REPO, version)
        base._push_current_to_upstream(REPO)
    print(json.dumps({"installer": str(installer), "sha256": sha, "size": size,
                      "package": str(package), "package_sha256": psha, "package_size": psize}, indent=2))
    return 0


def commit_manifest(repo: Path, version: str) -> None:
    """Skip an empty index, but never suppress a rejected commit."""
    result = subprocess.run(["git", "diff", "--cached", "--quiet"], cwd=repo)
    if result.returncode == 0:
        return
    if result.returncode != 1:
        raise SystemExit(result.returncode)
    base._git(repo, ["commit", "-m", "update manifest for v" + version])


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="release_go.py", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    def common(p: argparse.ArgumentParser) -> None:
        p.add_argument("--version", required=True)
        p.add_argument("--runtime", default="win-x64")

    b = sub.add_parser("build", help="maccy.exe and the SQLite library into artifacts/publish/<runtime>")
    common(b)
    b.add_argument("--publish-dir", default="")

    pk = sub.add_parser("package", help="the light-update zip from a publish folder")
    common(pk)
    pk.add_argument("--publish-dir", default="")
    pk.add_argument("--out-dir", default="")
    pk.add_argument("--legacy-from", default="", help='zip path or URL of the earlier package, or "manifest"')

    r = sub.add_parser("release", help="build, installer, package, manifest (and Gitee)")
    common(r)
    r.add_argument("--notes", default="")
    r.add_argument("--notes-file", default="")
    r.add_argument("--mandatory", action="store_true")
    r.add_argument("--base-url", default="")
    r.add_argument("--iscc", default="")
    r.add_argument("--legacy-from", default="", help='zip path or URL of the earlier package, or "manifest"')
    r.add_argument("--manifest", default=str(REPO / "docs" / "updates" / "manifest.json"))
    r.add_argument("--publish-gitee", action="store_true")
    r.add_argument("--yes", action="store_true")
    r.add_argument("--gitee-owner", default="")
    r.add_argument("--gitee-repo", default="")
    r.add_argument("--gitee-token", default="")

    a = ap.parse_args(argv)
    version = base._validate_version(a.version)
    if a.cmd == "build":
        build_publish(version, a.runtime, (Path(a.publish_dir) if a.publish_dir else REPO / "artifacts" / "publish" / a.runtime).resolve())
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
