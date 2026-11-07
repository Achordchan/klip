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
settings.json, a sign-in entry naming maccy.exe), and checks that the old
program files are gone, the sign-in entry names Klip.exe and the data is
still there.

The program's data is a folder of the test's own (LOCALAPPDATA is set for
the program, and so for the helper and the version it starts): the user's
history is never touched. What cannot be kept apart is still this Windows
user's: the sign-in entry (HKCU Run "maccy", put back as it was) and the
installer's entry under the installed programs (removed by its uninstaller).
So it runs only on a machine made for it: in GitHub Actions, or with
--disposable-machine. It stops only the Klip processes it started.
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
    """The paths of the running Klip.exe and helper processes."""
    out = subprocess.run(["powershell", "-NoProfile", "-Command",
                          "Get-Process -Name Klip, klip-update-* -ErrorAction SilentlyContinue | ForEach-Object { $_.Path }"],
                         capture_output=True, text=True).stdout
    return [line.strip() for line in out.splitlines() if line.strip()]


def inside(path: str, folder: Path) -> bool:
    try:
        Path(path).resolve().relative_to(folder.resolve())
        return True
    except ValueError:
        return False


def stop_klip(work: Path) -> None:
    """Stops the Klip processes started from the test's folder, no other."""
    folder = str(work.resolve()).replace("'", "''")
    subprocess.run(["powershell", "-NoProfile", "-Command",
                    "Get-Process -Name Klip, klip-update-* -ErrorAction SilentlyContinue | "
                    f"Where-Object {{ $_.Path -and $_.Path.StartsWith('{folder}', 'OrdinalIgnoreCase') }} | Stop-Process -Force"],
                   capture_output=True)


def show(path: Path) -> None:
    if path.exists():
        print(f"---- {path}")
        print(path.read_text(encoding="utf-8", errors="replace")[-6000:])


RUN_KEY = r"Software\Microsoft\Windows\CurrentVersion\Run"


def run_value() -> str | None:
    import winreg
    try:
        with winreg.OpenKey(winreg.HKEY_CURRENT_USER, RUN_KEY) as k:
            return winreg.QueryValueEx(k, "maccy")[0]
    except FileNotFoundError:
        return None


def set_run_value(value: str) -> None:
    import winreg
    with winreg.CreateKey(winreg.HKEY_CURRENT_USER, RUN_KEY) as k:
        winreg.SetValueEx(k, "maccy", 0, winreg.REG_SZ, value)


def delete_run_value() -> None:
    import winreg
    try:
        with winreg.OpenKey(winreg.HKEY_CURRENT_USER, RUN_KEY, 0, winreg.KEY_SET_VALUE) as k:
            winreg.DeleteValue(k, "maccy")
    except FileNotFoundError:
        pass


def check_installer(installer: Path, work: Path) -> None:
    saved = run_value()
    try:
        install_over_csharp(installer, work)
    finally:
        if saved is None:
            delete_run_value()
        else:
            set_run_value(saved)


def install_over_csharp(installer: Path, work: Path) -> None:
    """The installer over a folder of the C# program: the old program files
    go, the user's data stays, Klip.exe is there."""
    target = work / "installed-over-csharp"
    target.mkdir(parents=True)
    (target / "maccy.exe").write_bytes(b"old .NET program")
    (target / "maccy.dll").write_bytes(b"old .NET library")
    (target / "settings.json").write_text('{"MaxItems": 7}', encoding="utf-8")
    (target / "images").mkdir()
    (target / "images" / "a.png").write_bytes(b"PNG")
    # The C# program's sign-in entry, naming its program file.
    set_run_value(f'"{target / "maccy.exe"}"')
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
    entry = run_value()
    if (entry or "").lower() != f'"{target / release_go.EXE}"'.lower():
        problems.append(f"the sign-in entry is {entry!r}, not the program installed")
    if problems:
        show(log)
        raise SystemExit("installer: " + "; ".join(problems))
    print("installer: Klip.exe installed, the C# program files removed, the sign-in entry moved to it, the data kept")
    stop_klip(work)
    uninstaller = target / "unins000.exe"
    if uninstaller.exists():
        subprocess.run([str(uninstaller), "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART"])


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--version", required=True, help="the version installed")
    ap.add_argument("--installer", default="", help="a setup to check first (see above)")
    ap.add_argument("--work", default="", help="folder for the builds and the program's data (a new temporary one when empty)")
    ap.add_argument("--disposable-machine", action="store_true",
                    help="run outside GitHub Actions: this Windows user's sign-in entry and installed programs are changed for a while")
    a = ap.parse_args()
    if os.name != "nt":
        raise SystemExit("this test runs on Windows")
    if not (a.disposable_machine or os.environ.get("GITHUB_ACTIONS") == "true"):
        raise SystemExit("this test changes this Windows user's sign-in entry and installed programs: "
                         "run it in GitHub Actions, or pass --disposable-machine on a machine made for it")
    current, upcoming = a.version, next_version(a.version)
    # Absolute: the program runs in its install folder, and its LOCALAPPDATA
    # (a folder in here) must name the same place for it as for this test.
    work = (Path(a.work) if a.work else Path(tempfile.mkdtemp(prefix="klip-e2e-"))).resolve()
    work.mkdir(parents=True, exist_ok=True)
    # The program's data, and the helper it starts, in the test's folder.
    local = work / "localappdata"
    data = local / "maccy"
    updates = data / "updates"
    others = [p for p in klip_processes() if not inside(p, work)]
    if others:
        # It would hold the single-instance mutex, and it is somebody's.
        raise SystemExit(f"close the Klip that is running first: {others}")
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
        shutil.rmtree(data)  # the test's own, from an earlier run in this folder
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
    env = dict(os.environ, LOCALAPPDATA=str(local))
    subprocess.Popen([str(install / release_go.EXE)], cwd=str(install), stdout=out, stderr=subprocess.STDOUT, env=env)
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
    stop_klip(work)
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
