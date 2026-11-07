#!/usr/bin/env python3
"""End-to-end test of an automatic update, on a real Windows.

It builds this checkout twice: as the version installed (--version, built to
read its update manifest from a server on this machine) and as the next one
(--version with the last number one higher, built as a release is). It
serves the next version's package and manifest, starts the installed one
with nobody at the keyboard, and waits for what a user would find when they
come back: the program in the install folder is the next version, the helper
logged it as applied, the new version is running, started without showing
itself, and took in the helper's result.

    python tools/e2e_update.py --version 1.0.11 [--installer artifacts/release/Klip-1.0.11-setup.exe]

With --installer it first installs that setup silently into a folder where a
C# version seemed to be installed (maccy.exe, maccy.dll, the user's
settings.json), and checks that the old program files are gone and the data
is not.

It uses this user's real data folder (%LOCALAPPDATA%\\maccy): run it on a
machine made for tests (CI), never on one with a Klip in use.
"""
from __future__ import annotations

import argparse
import datetime
import functools
import http.server
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import release_go  # noqa: E402

PORT = 8765
WAIT = 10 * 60  # the program checks 6 s after starting; the gate looks every 30 s


def next_version(v: str) -> str:
    major, minor, patch = (int(x) for x in v.split("."))
    return f"{major}.{minor}.{patch + 1}"


def py(*args: str) -> None:
    subprocess.run([sys.executable, str(HERE / "release_go.py"), *args], check=True)


def file_version(path: Path) -> str:
    out = subprocess.run(["powershell", "-NoProfile", "-Command", f"(Get-Item -LiteralPath '{path}').VersionInfo.ProductVersion"],
                         capture_output=True, text=True, check=True).stdout
    return out.strip()


def klip_processes() -> list[str]:
    """The paths of the running Klip.exe processes."""
    out = subprocess.run(["powershell", "-NoProfile", "-Command",
                          "Get-Process -Name Klip -ErrorAction SilentlyContinue | ForEach-Object { $_.Path }"],
                         capture_output=True, text=True).stdout
    return [line.strip() for line in out.splitlines() if line.strip()]


def stop_klip() -> None:
    subprocess.run(["taskkill", "/F", "/IM", "Klip.exe"], capture_output=True)
    subprocess.run(["taskkill", "/F", "/IM", "klip-update-*"], capture_output=True)


def show(path: Path) -> None:
    if path.exists():
        print(f"---- {path}")
        print(path.read_text(encoding="utf-8", errors="replace")[-6000:])


def check_installer(installer: Path, work: Path) -> None:
    """The installer over a folder of the C# program: the old program files
    go, the user's data stays, Klip.exe is there."""
    target = work / "installed-over-csharp"
    target.mkdir(parents=True)
    (target / "maccy.exe").write_bytes(b"old .NET program")
    (target / "maccy.dll").write_bytes(b"old .NET library")
    (target / "settings.json").write_text('{"MaxItems": 7}', encoding="utf-8")
    (target / "images").mkdir()
    (target / "images" / "a.png").write_bytes(b"PNG")
    log = work / "setup.log"
    res = subprocess.run([str(installer), "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/CURRENTUSER",
                          f"/DIR={target}", f"/LOG={log}"])
    if res.returncode != 0:
        show(log)
        raise SystemExit(f"installer exited with {res.returncode}")
    problems = []
    if not (target / release_go.EXE).is_file():
        problems.append("Klip.exe was not installed")
    for gone in ("maccy.exe", "maccy.dll"):
        if (target / gone).exists():
            problems.append(f"{gone} of the C# program is still there")
    if (target / "settings.json").read_text(encoding="utf-8") != '{"MaxItems": 7}' or not (target / "images" / "a.png").exists():
        problems.append("the user's data was touched")
    if problems:
        show(log)
        raise SystemExit("installer: " + "; ".join(problems))
    print("installer: Klip.exe installed, the C# program files removed, the data kept")
    stop_klip()
    uninstaller = target / "unins000.exe"
    if uninstaller.exists():
        subprocess.run([str(uninstaller), "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART"])


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--version", required=True, help="the version installed")
    ap.add_argument("--installer", default="", help="a setup to check first (see above)")
    ap.add_argument("--work", default="", help="folder for the builds (a new temporary one when empty)")
    a = ap.parse_args()
    if os.name != "nt":
        raise SystemExit("this test runs on Windows")
    current, upcoming = a.version, next_version(a.version)
    work = Path(a.work) if a.work else Path(tempfile.mkdtemp(prefix="klip-e2e-"))
    work.mkdir(parents=True, exist_ok=True)
    data = Path(os.environ["LOCALAPPDATA"]) / "maccy"
    updates = data / "updates"

    stop_klip()
    if a.installer:
        check_installer(Path(a.installer).resolve(), work)

    print(f"[1/4] build {current} (reads the manifest from this machine) and {upcoming}")
    manifest_url = f"http://127.0.0.1:{PORT}/manifest.json"
    py("build", "--version", current, "--publish-dir", str(work / "current"), "--manifest-url", manifest_url)
    py("build", "--version", upcoming, "--publish-dir", str(work / "next"))
    serve = work / "serve"
    files = serve / f"v{upcoming}"
    files.mkdir(parents=True)
    py("package", "--version", upcoming, "--publish-dir", str(work / "next"), "--out-dir", str(files))
    package = files / f"Klip-{upcoming}-win-x64.zip"
    setup = files / f"Klip-{upcoming}-setup.exe"
    setup.write_bytes(b"not used: the package is")
    release_go.write_manifest(serve / "manifest.json", version=upcoming, mandatory=False, notes="e2e",
                              base_url=f"http://127.0.0.1:{PORT}", installer=setup, package=package, runtime="win-x64")

    print("[2/4] install", current)
    install = work / "install"
    shutil.copytree(work / "current", install)
    if data.exists():
        shutil.rmtree(data)
    updates.mkdir(parents=True)
    # Started as an update leaves it: no window. The panel would be in front,
    # and an open panel holds the update back (the user is using Klip).
    (updates / "quiet-relaunch.json").write_text(json.dumps({
        "version": current, "at": datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")}),
        encoding="utf-8")

    handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=str(serve))
    server = http.server.ThreadingHTTPServer(("127.0.0.1", PORT), handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()

    print("[3/4] start", current, "and wait for the update")
    out = open(work / "klip-current.log", "w", encoding="utf-8")
    subprocess.Popen([str(install / release_go.EXE)], cwd=str(install), stdout=out, stderr=subprocess.STDOUT)
    exe = install / release_go.EXE
    helper_log = updates / "last_light_update.log"
    deadline = time.time() + WAIT
    version = current
    while time.time() < deadline:
        time.sleep(10)
        try:
            version = file_version(exe)
        except subprocess.CalledProcessError:
            continue  # being replaced
        if version == upcoming:
            break
    server.shutdown()

    print("[4/4] check")
    problems = []
    if version != upcoming:
        problems.append(f"Klip.exe in the install folder is still {version!r} after {WAIT // 60} minutes")
    else:
        time.sleep(15)  # the new version starts and reads its marker
        if "light update applied" not in (helper_log.read_text(encoding="utf-8", errors="replace") if helper_log.exists() else ""):
            problems.append("the helper did not log the update as applied")
        running = [p for p in klip_processes() if Path(p).resolve() == exe.resolve()]
        if not running:
            problems.append(f"Klip is not running from the install folder (running: {klip_processes()})")
        if (updates / "quiet-relaunch.json").exists():
            problems.append("the new version did not start quietly (its marker is still there)")
        if (updates / "last-update-result.json").exists():
            problems.append("the new version did not take in the helper's result")
    stop_klip()
    out.close()
    if problems:
        for f in (work / "klip-current.log", helper_log, updates / "last-update-result.json"):
            show(f)
        print("FAILED: " + "; ".join(problems))
        return 1
    print(f"OK: {current} updated itself to {upcoming}, unattended, and started again without a window")
    return 0


if __name__ == "__main__":
    sys.exit(main())
