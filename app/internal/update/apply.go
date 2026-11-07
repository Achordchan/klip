package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"klip/internal/durable"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Plan is what the helper needs to put an update in place after the program
// has exited. It is written as JSON by the program and read by the helper.
type Plan struct {
	ReadyFile string `json:"readyFile,omitempty"`
	// PID is the program to wait for.
	PID int `json:"pid"`
	// InstallDir is where the program's files are.
	InstallDir string `json:"installDir"`
	// StagingDir and PackageManifest are the unpacked package.
	StagingDir      string `json:"stagingDir"`
	PackageManifest string `json:"packageManifest"`
	// InstalledManifest remembers what the last package put there, so the
	// next one can remove the files it no longer has.
	InstalledManifest string `json:"installedManifest"`
	// RestartExe is the program to start again when the installer cannot be
	// started (only used by an installer plan).
	RestartExe string `json:"restartExe,omitempty"`
	BackupDir  string `json:"backupDir"`
	LogPath    string `json:"logPath"`
	// Installer, when set, is a full installer to start instead of copying
	// files.
	Installer string `json:"installer,omitempty"`
	// Version is the version being installed, for the result.
	Version string `json:"version,omitempty"`
	// Quiet says that nobody asked for this update (it was put in place while
	// the user was away): no message box is shown; how it went is written to
	// ResultPath, which the program reads when it starts. A helper of an
	// earlier version ignores both.
	Quiet bool `json:"quiet,omitempty"`
	// ResultPath, when set, is where the helper writes how the update went
	// (InstallResult).
	ResultPath string `json:"resultPath,omitempty"`
	// CommitFile, when set, must exist once the program has exited, or the
	// helper does nothing: the program writes it at the moment it quits for
	// the update. A quiet install is set up before that moment is final, and
	// the program may exit for another reason meanwhile (the user quits it, a
	// migration restarts it): that exit is no leave to install.
	CommitFile string `json:"commitFile,omitempty"`
}

// errNotCommitted is the helper of a quiet install whose program exited
// without having decided to quit for it.
var errNotCommitted = errors.New("the program exited without committing to the update; nothing was changed")

// InstallResult is how an update went, as the helper writes it
// (Plan.ResultPath).
type InstallResult struct {
	Version string    `json:"version"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
	// RolledBack says that the old version was put back whole.
	RolledBack bool `json:"rolledBack,omitempty"`
	// Pending says that the installer was started and the helper does not
	// know how it ends (the user may cancel it): OK is false, and only the
	// version that starts next tells whether it was installed.
	Pending bool `json:"pending,omitempty"`
}

// ReadResult reads what the helper wrote at path; a missing file is no
// result (nil, nil).
func ReadResult(path string) (*InstallResult, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r InstallResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// exitWait is how long the helper waits for the program to exit. A program
// that does not (its quit was held, as during a data migration) keeps its
// files, and the helper gives up without touching them: waiting for ever
// would hold the update mutex, and every later start of Klip, which sees it,
// would exit at once.
const exitWait = 2 * time.Minute

// errAppDidNotExit is the helper giving up on a program that did not exit.
var errAppDidNotExit = errors.New("the program did not exit; nothing was changed")

// Hooks are the parts of Apply that touch the system, for tests.
type Hooks struct {
	// WaitExit blocks until the process is gone, or for timeout at most; it
	// says whether the process is gone.
	WaitExit func(pid int, timeout time.Duration) bool
	// Sleep is time.Sleep.
	Sleep func(time.Duration)
	// Launch starts a program and does not wait for it.
	Launch func(path string) error
	// LaunchInstaller starts the installer, which may need to ask for rights
	// (the shell does that; a plain process start cannot).
	LaunchInstaller func(path string) error
	// Alert tells the user that the update failed.
	Alert func(title, msg string)
	// Now is time.Now.
	Now func() time.Time
	// Copy puts a file in place (copyFileRetry); tests make it fail.
	Copy func(src, dst string) error
}

func (h Hooks) fill() Hooks {
	if h.WaitExit == nil {
		h.WaitExit = waitExit
	}
	if h.Sleep == nil {
		h.Sleep = time.Sleep
	}
	if h.LaunchInstaller == nil {
		if h.Launch != nil {
			h.LaunchInstaller = h.Launch
		} else {
			h.LaunchInstaller = launchInstaller
		}
	}
	if h.Launch == nil {
		h.Launch = launch
	}
	if h.Alert == nil {
		h.Alert = alert
	}
	if h.Now == nil {
		h.Now = time.Now
	}
	if h.Copy == nil {
		h.Copy = func(src, dst string) error { return copyFileRetry(src, dst, h.Sleep) }
	}
	return h
}

// ReadPlan loads a plan file.
func ReadPlan(path string) (Plan, error) {
	var p Plan
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(b, &p)
}

// WritePlan stores a plan file, durably: the helper reads it after the
// program has gone, and a plan cut short by a power cut must not be half one.
func WritePlan(path string, p Plan) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeJSON(path, p)
}

type logger struct {
	path string
	now  func() time.Time
}

func (l logger) say(msg string) {
	_ = os.MkdirAll(filepath.Dir(l.path), 0o755)
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", l.now().Format("2006-01-02T15:04:05"), msg)
}

type rollbackItem struct {
	path, backup   string
	existed        bool
	moved          bool
	manifest       bool
	originalDigest string
}

// RunPlan is the helper: it waits for the program to exit and then, as the
// PowerShell script of the C# client does, checks the package once more,
// backs up and copies each file, checks the copies, removes what the
// previous package had and this one has not, remembers the list, and starts
// the program again. On any failure it puts everything back, tells the user
// and starts the old program. It returns the error.
func RunPlan(p Plan, h Hooks) error {
	h = h.fill()
	release, lockErr := holdUpdateExclusion()
	if lockErr != nil {
		return lockErr
	}
	released := false
	unlock := func() {
		if !released {
			released = true
			release()
		}
	}
	defer unlock()
	launch := h.Launch
	h.Launch = func(path string) error {
		unlock()
		err := launch(path)
		if err != nil {
			var lockErr error
			release, lockErr = holdUpdateExclusion()
			if lockErr != nil {
				return fmt.Errorf("launch: %v; update exclusion: %w", err, lockErr)
			}
			released = false
		}
		return err
	}
	lg := logger{p.LogPath, h.Now}
	_ = os.Remove(p.LogPath)
	if p.Quiet {
		// Nobody is there to read a message box: the log and the result say it.
		h.Alert = func(title, msg string) { lg.say("not shown (quiet): " + title + ": " + msg) }
	}
	// The result is written before any program that reads it at its start is
	// launched, and written again if that launch fails.
	write := func(r InstallResult) {
		if p.ResultPath == "" {
			return
		}
		r.Version, r.At = p.Version, h.Now()
		if werr := writeJSON(p.ResultPath, r); werr != nil {
			lg.say("cannot write the result: " + werr.Error())
		}
	}
	report := func(err error, rolledBack bool) {
		r := InstallResult{OK: err == nil, RolledBack: rolledBack}
		if err != nil {
			r.Error = err.Error()
		}
		write(r)
	}
	if p.ReadyFile != "" {
		if err := os.WriteFile(p.ReadyFile, []byte("ready"), 0600); err != nil {
			return err
		}
	}
	lg.say("waiting for app exit")
	if p.PID > 0 && !h.WaitExit(p.PID, exitWait) {
		lg.say("failed: " + errAppDidNotExit.Error())
		report(errAppDidNotExit, true)
		return errAppDidNotExit
	}
	h.Sleep(300 * time.Millisecond)
	if p.CommitFile != "" {
		if !fileExists(p.CommitFile) {
			lg.say("failed: " + errNotCommitted.Error())
			report(errNotCommitted, true)
			return errNotCommitted
		}
		_ = os.Remove(p.CommitFile) // used once
	}

	if p.Installer != "" {
		unlock()
		lg.say("starting installer " + p.Installer)
		write(InstallResult{Pending: true}) // the installer may start Klip itself
		if err := h.LaunchInstaller(p.Installer); err != nil {
			lg.say("failed: " + err.Error())
			report(err, true) // nothing was changed: the old version is as it was
			h.Alert("Klip 更新失败", "无法启动安装程序："+err.Error())
			// Klip has already exited for this: start it again.
			if p.RestartExe != "" && fileExists(p.RestartExe) {
				_ = h.Launch(p.RestartExe)
			}
			return err
		}
		return nil
	}

	var rollback []rollbackItem
	// What the last package left as its list of files: put back too if this
	// one has to be undone.
	prevList, prevListErr := os.ReadFile(p.InstalledManifest)
	// undo puts everything back and returns the first thing that could not be
	// (nil: the old version is whole again). A file a scanner holds for a moment
	// is tried a few times.
	undo := func() error {
		var first error
		note := func(err error) {
			if first == nil {
				first = err
			}
		}
		for i := len(rollback) - 1; i >= 0; i-- {
			it := rollback[i]
			var rerr error
			for try := 0; try < 4; try++ {
				if try > 0 {
					h.Sleep(300 * time.Millisecond)
				}
				rerr = nil
				if it.moved {
					if _, err := os.Stat(it.backup); os.IsNotExist(err) {
						digest, verifyErr := transitionDigest(it.path)
						if verifyErr != nil || it.originalDigest == "" || digest != it.originalDigest {
							rerr = fmt.Errorf("cannot verify original transition path %s: backup missing", it.path)
						}
						break
					}
					rerr = os.RemoveAll(it.path)
					if rerr == nil {
						rerr = os.Rename(it.backup, it.path)
					}
				} else if it.existed {
					if rerr = os.MkdirAll(filepath.Dir(it.path), 0o755); rerr == nil {
						rerr = copyFile(it.backup, it.path)
					}
				} else if _, serr := os.Stat(it.path); serr == nil {
					rerr = os.Remove(it.path)
				}
				if rerr == nil {
					break
				}
			}
			if rerr != nil {
				lg.say("rollback failed: " + rerr.Error())
				note(rerr)
			}
		}
		if p.InstalledManifest != "" {
			var rerr error
			if prevListErr == nil {
				rerr = os.WriteFile(p.InstalledManifest, prevList, 0o644)
			} else if errors.Is(prevListErr, os.ErrNotExist) {
				if err := os.Remove(p.InstalledManifest); err != nil && !errors.Is(err, os.ErrNotExist) {
					rerr = err
				}
			}
			if rerr != nil {
				lg.say("rollback of the file list failed: " + rerr.Error())
				note(rerr)
			}
		}
		if first == nil {
			_ = os.Remove(recoveryPath(p.InstallDir))
			_ = os.Remove(rollbackPath(p.InstallDir))
		}
		return first
	}
	err := applyFiles(p, h, lg, &rollback)
	if err == nil {
		lg.say("light update applied")
		// Committed first, durably: a power cut from here on must keep the new
		// version, which is whole, not roll it back (RecoverInterruptedUpdate
		// drops a committed journal). Then the journal goes.
		commitErr := saveCommitted(p.InstallDir, rollback)
		if commitErr != nil {
			lg.say("cannot mark the update committed: " + commitErr.Error())
		}
		if err := removeRecoveryJournal(p.InstallDir, h.Sleep); err != nil {
			if commitErr != nil {
				// Neither: the next start would roll the new version back.
				h.Alert("Klip 更新未完成", "无法清理恢复记录，请关闭占用程序后重新启动Klip以恢复："+err.Error())
				report(err, false)
				return err
			}
			// A committed journal left behind is dropped at the next start.
			lg.say("journal left committed: " + err.Error())
		}
		report(nil, false) // before the new version starts and reads it
		if lerr := h.Launch(filepath.Join(p.InstallDir, ExeName)); lerr != nil {
			// The new program will not start (a virus scanner may have taken
			// it): that is a failed update, and the old files come back.
			lg.say("failed to start the new program: " + lerr.Error())
			err = lerr
			// The journal says "undo" again before anything is undone, so that a
			// stop half way is finished at the next start. If it cannot be
			// written (the committed one may be stuck), the undo goes ahead all
			// the same, from what is in memory: stopping here would leave a
			// committed journal, which startup passes over, on a version that
			// does not start.
			if jerr := saveRecovery(p.InstallDir, rollback); jerr != nil {
				lg.say("cannot write the journal again: " + jerr.Error())
				// A marker of its own, which startup honours over the journal.
				// Without either nothing is undone: an undo cut short with no
				// record of it would leave old and new files mixed, which no
				// start would repair; the new version is at least whole.
				if merr := writeRollback(p.InstallDir, rollback); merr != nil {
					lg.say("cannot record the rollback: " + merr.Error())
					h.Alert("Klip 更新失败", "新版本无法启动："+lerr.Error()+"\n\n也无法记录恢复信息（"+merr.Error()+"），为免新旧文件混杂，没有回滚。请重新下载安装包安装。日志："+p.LogPath)
					report(err, false)
					return err
				}
			}
			if uerr := undo(); uerr != nil {
				h.Alert("Klip 更新失败", "新版本无法启动："+lerr.Error()+"\n\n旧版本没能完全恢复（"+uerr.Error()+"），Klip 可能无法使用，请重新下载安装包安装。日志："+p.LogPath)
				report(err, false)
				return err
			}
			report(err, true)
			if exe := filepath.Join(p.InstallDir, ExeName); fileExists(exe) {
				if oerr := h.Launch(exe); oerr != nil {
					lg.say("failed to start the old program: " + oerr.Error())
					h.Alert("Klip 更新失败", "新版本无法启动："+lerr.Error()+"\n\n旧版本已恢复，但也没能启动："+oerr.Error()+"\n日志："+p.LogPath)
					return err
				}
			}
			h.Alert("Klip 更新失败", "新版本无法启动："+lerr.Error()+"\n\n已恢复旧版本。日志："+p.LogPath)
			return err
		}
		// Transition backups are retained: ownership of pre-existing paths is unknown.
		return nil
	}
	lg.say("failed: " + err.Error())
	if uerr := undo(); uerr != nil {
		lg.say("rollback incomplete: " + uerr.Error())
		h.Alert("Klip 更新失败", "轻量更新失败，回滚也没能完成（"+uerr.Error()+"），Klip 可能无法使用，请重新下载安装包安装。日志："+p.LogPath)
		report(err, false)
		return err
	}
	report(err, true)
	h.Alert("Klip 更新失败", "轻量更新失败，已回滚。日志："+p.LogPath)
	if exe := filepath.Join(p.InstallDir, ExeName); fileExists(exe) {
		_ = h.Launch(exe)
	}
	return err
}

func applyFiles(p Plan, h Hooks, lg logger, rollback *[]rollbackItem) error {
	root, rootErr := filepath.Abs(p.InstallDir)
	if rootErr != nil {
		return rootErr
	}
	p.InstallDir = filepath.Clean(root)
	data, err := os.ReadFile(p.PackageManifest)
	if err != nil {
		return err
	}
	m, err := ParsePackageManifest(data)
	if err != nil {
		return err
	}
	if len(m.Files) < 1 {
		return errors.New("package file list is empty")
	}
	newFiles := map[string]bool{}
	type entry struct{ rel, src, dest, sha string }
	var entries []entry
	for _, f := range m.Files {
		rel, err := NormalizePath(f.Path)
		if err != nil {
			return err
		}
		if IsProtected(rel) {
			return errors.New("protected path in package: " + f.Path)
		}
		src, err := insideRoot(p.StagingDir, rel)
		if err != nil {
			return err
		}
		if !fileExists(src) {
			return errors.New("package file missing: " + f.Path)
		}
		want := strings.ToUpper(strings.TrimSpace(f.SHA256))
		if want == "" {
			return errors.New("missing sha256: " + f.Path)
		}
		if got, err := SHA256File(src); err != nil || got != want {
			return errors.New("sha256 mismatch before copy: " + f.Path)
		}
		dest, err := insideRoot(p.InstallDir, rel)
		if err != nil {
			return err
		}
		newFiles[key(rel)] = true
		entries = append(entries, entry{rel, src, dest, want})
	}
	if !newFiles[strings.ToLower(ExeName)] {
		return errors.New("package missing " + ExeName)
	}
	if err := os.MkdirAll(p.BackupDir, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		ancestors := []string{}
		for candidate := e.dest; candidate != p.InstallDir; candidate = filepath.Dir(candidate) {
			if filepath.Dir(candidate) == candidate {
				return errors.New("update path escaped installation root")
			}
			ancestors = append(ancestors, candidate)
		}
		for i := len(ancestors) - 1; i >= 0; i-- {
			candidate := ancestors[i]
			st, err := os.Stat(candidate)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			conflict := (candidate == e.dest && st.IsDir()) || (candidate != e.dest && !st.IsDir())
			if !conflict {
				continue
			}
			rel, err := filepath.Rel(p.InstallDir, candidate)
			if err != nil {
				return err
			}
			if IsProtected(rel) {
				return errors.New("protected transition")
			}
			backup, err := os.MkdirTemp(p.InstallDir, ".update-transition-")
			if err != nil {
				return err
			}
			if err = os.Remove(backup); err != nil {
				return err
			}
			digest, err := transitionDigest(candidate)
			if err != nil {
				return err
			}
			*rollback = append(*rollback, rollbackItem{path: candidate, backup: backup, existed: true, moved: true, originalDigest: digest})
			if err := saveRecovery(p.InstallDir, *rollback); err != nil {
				return err
			}
			if err = os.Rename(candidate, backup); err != nil {
				return err
			}
		}
	}
	for _, e := range entries {
		// replaced and deleted files go to separate folders: a package may replace
		// "_deleted/x" and delete "x", and neither backup may take the other's place
		backup, err := insideRoot(p.BackupDir, filepath.Join("replaced", e.rel))
		if err != nil {
			return err
		}
		targetInfo, statErr := os.Stat(e.dest)
		if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		existed := statErr == nil && targetInfo.Mode().IsRegular()
		if existed {
			if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
				return err
			}
			if err := copyFile(e.dest, backup); err != nil {
				return err
			}
		}
		*rollback = append(*rollback, rollbackItem{path: e.dest, backup: backup, existed: existed})
		if err := saveRecovery(p.InstallDir, *rollback); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(e.dest), 0o755); err != nil {
			return err
		}
		if err := h.Copy(e.src, e.dest); err != nil {
			return err
		}
	}
	for _, e := range entries {
		if got, err := SHA256File(e.dest); err != nil || got != e.sha {
			return errors.New("sha256 mismatch after copy: " + e.rel)
		}
	}

	// What the previous package had and this one does not.
	var gone []string
	if prev, err := os.ReadFile(p.InstalledManifest); err == nil {
		if pm, err := ParsePackageManifest(prev); err == nil {
			for _, f := range pm.Files {
				gone = append(gone, f.Path)
			}
		}
	}
	gone = append(gone, m.LegacyFiles...)
	for _, old := range gone {
		rel, err := NormalizePath(old)
		if err != nil {
			return err
		}
		if newFiles[key(rel)] || IsProtected(rel) {
			continue
		}
		dest, err := insideRoot(p.InstallDir, rel)
		if err != nil {
			return err
		}
		if !fileExists(dest) {
			continue
		}
		backup, err := insideRoot(p.BackupDir, filepath.Join("deleted", rel))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
			return err
		}
		if err := copyFile(dest, backup); err != nil {
			return err
		}
		*rollback = append(*rollback, rollbackItem{path: dest, backup: backup, existed: true})
		if err := saveRecovery(p.InstallDir, *rollback); err != nil {
			return err
		}
		if err := os.Remove(dest); err != nil {
			return err
		}
	}

	// The list is kept without the legacy part: that is done now.
	keep := *m
	keep.LegacyFiles = nil
	if err := os.MkdirAll(filepath.Dir(p.InstalledManifest), 0o755); err != nil {
		return err
	}
	backup := filepath.Join(p.BackupDir, "previous-installed-manifest.json")
	existed := fileExists(p.InstalledManifest)
	if existed {
		if err := copyFile(p.InstalledManifest, backup); err != nil {
			return err
		}
	}
	*rollback = append(*rollback, rollbackItem{path: p.InstalledManifest, backup: backup, existed: existed, manifest: true})
	if err := saveRecovery(p.InstallDir, *rollback); err != nil {
		return err
	}
	return writeJSON(p.InstalledManifest, keep)
}

func key(rel string) string { return strings.ToLower(strings.ReplaceAll(rel, "\\", "/")) }

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".update-json-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return durable.Rename(f.Name(), path)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".update-copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if err := out.Chmod(0755); err != nil {
		out.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return durable.Rename(out.Name(), dst)
}

// copyFileRetry is copyFile, tried again a few times: a virus scanner may
// hold a file that was just written.
func copyFileRetry(src, dst string, sleep func(time.Duration)) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = copyFile(src, dst); err == nil {
			return nil
		}
		sleep(200 * time.Millisecond)
	}
	return err
}

// CleanLegacy removes the files of the earlier program that the installed
// package lists as legacy (see PackageManifest.LegacyFiles), once: the list
// is dropped from the remembered manifest afterwards. 1.0.10 puts the
// package's list there verbatim when it applies the first light update, but
// it knows nothing of the legacy list, so the 1.0.11 program does it on its
// first start. It returns the paths it removed.
func CleanLegacy(installDir, installedManifest string) ([]string, error) {
	data, err := os.ReadFile(installedManifest)
	if err != nil {
		return nil, nil // nothing installed by a package: nothing to clean
	}
	m, err := ParsePackageManifest(data)
	if err != nil || len(m.LegacyFiles) == 0 {
		return nil, nil
	}
	own := map[string]bool{}
	for _, f := range m.Files {
		if rel, err := NormalizePath(f.Path); err == nil {
			own[key(rel)] = true
		}
	}
	var removed []string
	for _, old := range m.LegacyFiles {
		rel, err := NormalizePath(old)
		if err != nil || own[key(rel)] || IsProtected(rel) {
			continue
		}
		dest, err := insideRoot(installDir, rel)
		if err != nil || !fileExists(dest) {
			continue
		}
		if err := os.Remove(dest); err != nil {
			return removed, err
		}
		removed = append(removed, rel)
	}
	m.LegacyFiles = nil
	return removed, writeJSON(installedManifest, m)
}

// HelperMain runs the program as the helper when it was started as one
// (maccy.exe --apply-update plan.json). It says whether it was; if so the
// caller must exit with the code.
func HelperMain(args []string, h Hooks) (handled bool, code int) {
	if len(args) >= 4 && args[1] == "--recover-update" {
		release, err := holdUpdateExclusion()
		if err != nil {
			return true, 1
		}
		defer release()
		pid, _ := strconv.Atoi(args[3])
		if !waitExit(pid, exitWait) {
			// The program that asked did not go: its files are in use, and
			// nothing is restored now. Its next start asks again.
			fmt.Fprintln(os.Stderr, "recover-update:", errAppDidNotExit)
			return true, 1
		}
		if err := RecoverInterruptedUpdate(args[2]); err != nil {
			h = h.fill()
			h.Alert("Klip 恢复失败", "自动恢复未完成："+err.Error()+"\n请关闭占用文件的程序后重试；若仍失败，请重新安装。请保留恢复日志和备份。")
			fmt.Fprintln(os.Stderr, "recover-update:", err)
			return true, 1
		}
		release()
		if err := launch(filepath.Join(args[2], ExeName)); err != nil {
			return true, 1
		}
		return true, 0
	}
	if len(args) < 3 || args[1] != "--apply-update" {
		return false, 0
	}
	plan, err := ReadPlan(args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "apply-update:", err)
		return true, 2
	}
	if err := RunPlan(plan, h); err != nil {
		return true, 1
	}
	return true, 0
}

func removeRecoveryJournal(root string, sleep func(time.Duration)) error {
	var err error
	for i := 0; i < 5; i++ {
		err = removeJournal(recoveryPath(root))
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		if i < 4 {
			sleep(200 * time.Millisecond)
		}
	}
	return fmt.Errorf("cannot retire update recovery journal: %w", err)
}
