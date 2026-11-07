package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Phase is what the coordinator is doing.
type Phase int

const (
	Idle        Phase = iota
	Checking          // looking at the manifest
	UpToDateNow       // the user asked, and there is nothing newer
	Available         // Info is a newer version
	Downloading       // Read/Total of the download
	Applying          // the helper is started, the program is about to exit
	Problem           // Message says what went wrong
)

// State is what a window shows.
type State struct {
	Phase Phase
	Info  *Info
	// Message says what happened. For a Problem, Title and Detail are the
	// two halves of it, as the C# message window has them (the title of the
	// window, and its text).
	Message string
	// Check says that a Problem came from looking for a version, not from
	// installing one (so there is nothing to retry but the look).
	Check  bool
	Title  string
	Detail string
	Read   int64
	Total  int64
}

// Coordinator is UpdateCoordinator without the windows: it keeps the newest
// version found, downloads it and hands the replacing to the helper.
type Coordinator struct {
	Client *Client
	// Root is the updates folder of the data directory.
	Root string
	// Exe is the running program; its folder is what gets updated.
	Exe string
	// PID is this process (os.Getpid unless a test sets it).
	PID int
	// Shutdown ends the program, once the helper is started.
	Shutdown func()
	// OnChange is called after each change of the state, from any goroutine.
	OnChange func()
	// Supported says whether this system can replace its own files; without
	// it the update is offered as a download to do by hand.
	Supported bool
	// StartHelper starts the helper (the package's own by default).
	StartHelper func(helperExe, planPath string) error
	// Now is time.Now unless a test sets it.
	Now func() time.Time

	// fsMu keeps the cleaning of old files and the preparing of an update from
	// working in the same folders at once.
	fsMu     sync.Mutex
	mu       sync.Mutex
	state    State
	busy     bool
	applying bool
	// prepared is the package of the newer version, downloaded and unpacked
	// in the background (Prepare) and waiting to be installed; preparing says
	// that Prepare is at work. Neither is part of State: nobody asked, and
	// the windows must not show it.
	prepared  *Prepared
	preparing bool
}

// NewCoordinator returns a Coordinator for the running program.
func NewCoordinator(c *Client, root, exe string, shutdown func()) *Coordinator {
	return &Coordinator{Client: c, Root: root, Exe: exe, Shutdown: shutdown, Supported: runtime.GOOS == "windows"}
}

// State returns a copy of the state.
func (c *Coordinator) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Available is the newer version found, nil when none is known.
func (c *Coordinator) Available() *Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Info == nil {
		return nil
	}
	i := *c.state.Info
	return &i
}

func (c *Coordinator) set(f func(*State)) {
	c.mu.Lock()
	f(&c.state)
	c.mu.Unlock()
	if c.OnChange != nil {
		c.OnChange()
	}
}

// setChecking is set for a look that the user asked for: once an update is
// being installed the look no longer has a say in what the window shows.
func (c *Coordinator) setChecking(f func(*State)) {
	c.set(func(s *State) {
		if c.applying {
			return
		}
		f(s)
	})
}

// MustUpdate is the newer version when it is mandatory and this system can
// install it; a release for another system is not a reason to shut this one.
func (c *Coordinator) MustUpdate() *Info {
	i := c.Available()
	if i == nil || !i.Mandatory || !c.Supported {
		return nil
	}
	// And there must be something to install: a package for this runtime or an
	// installer. A release made for another kind of machine cannot be obeyed.
	if !i.HasSupportedPackage(c.Client.runtime()) && !c.hasInstaller(i) {
		return nil
	}
	return i
}

// Refresh looks for a newer version without a word to the user (the check at
// start and every few hours): it only changes what Available says, and an
// error leaves it as it was.
func (c *Coordinator) Refresh(ctx context.Context) {
	c.mu.Lock()
	if c.busy || c.applying {
		c.mu.Unlock()
		return
	}
	c.busy = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.busy = false; c.mu.Unlock() }()

	res := c.Client.Check(ctx, nil)
	switch res.Status {
	case UpdateAvailable:
		c.set(func(s *State) {
			if !c.applying { // an update under way is not for a quiet check to overwrite
				*s = State{Phase: Available, Info: res.Info}
			}
		})
	case UpToDate:
		c.set(func(s *State) {
			if !c.applying {
				*s = State{}
			}
		})
	}
}

// Check is the user asking: it moves through Checking to UpToDateNow,
// Available or Problem, with the texts of the C# window.
func (c *Coordinator) Check(ctx context.Context) {
	waited := false
	for {
		c.mu.Lock()
		if c.applying {
			c.mu.Unlock()
			c.set(func(s *State) { s.Message = "更新正在进行中，请稍后..." })
			return
		}
		if !c.busy {
			break // the lock is held: this check is the one that runs
		}
		c.mu.Unlock()
		// A check is under way (the quiet one at start, say): wait for it to
		// end and then make our own, so that the window gets an answer of its
		// own and is not left "checking" for a request that has ended.
		if !waited {
			waited = true
			c.setChecking(func(s *State) { s.Phase, s.Message = Checking, "已有检查正在进行，稍后继续..." })
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	c.busy = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.busy = false; c.mu.Unlock() }()

	c.mu.Lock()
	known := c.state.Info // an update found before stays known if this look fails
	c.mu.Unlock()
	c.setChecking(func(s *State) {
		*s = State{Phase: Checking, Message: "正在连接更新服务器，请稍候...", Info: known}
	})
	res := c.Client.Check(ctx, func(m string) { c.setChecking(func(s *State) { s.Message = m }) })
	switch res.Status {
	case UpToDate:
		c.setChecking(func(s *State) {
			*s = State{Phase: UpToDateNow, Message: "当前已经是最新版本。", Title: "检查更新", Detail: "当前已经是最新版本。"}
		})
	case Failed:
		c.setChecking(func(s *State) {
			*s = State{Phase: Problem, Message: "检查更新失败：" + res.Error, Info: known, Check: true, Title: "检查更新失败", Detail: res.Error}
		})
	default:
		c.setChecking(func(s *State) { *s = State{Phase: Available, Info: res.Info} })
	}
}

// Dismiss clears a finished message (UpToDateNow, Problem) and, for a version
// the user postponed, goes back to Available.
func (c *Coordinator) Dismiss() {
	// One step, under the lock: an installation that starts meanwhile is not
	// turned back into "available" by a dismissal that read the state earlier.
	c.set(func(s *State) {
		if s.Phase == Applying || s.Phase == Downloading || c.applying {
			return
		}
		if info := s.Info; info != nil {
			*s = State{Phase: Available, Info: info}
		} else {
			*s = State{}
		}
	})
}

// Apply downloads the newer version and starts the helper that replaces the
// files; then it asks the program to exit. The light package comes first;
// when it fails and there is an installer, that is used instead, as in C#.
func (c *Coordinator) Apply(ctx context.Context) {
	c.mu.Lock()
	info := c.state.Info
	if info == nil || c.applying {
		c.mu.Unlock()
		return
	}
	c.applying = true
	c.mu.Unlock()
	fail := func(title, detail string) {
		c.mu.Lock()
		c.applying = false
		c.mu.Unlock()
		msg := title + "：" + detail
		if title == "更新失败" {
			msg = detail
		}
		c.set(func(s *State) { s.Phase, s.Message, s.Title, s.Detail = Problem, msg, title, detail })
	}

	if !c.Supported {
		fail("更新失败", "当前系统不能自动更新，请到发布页下载新版本："+strings.TrimSpace(c.manualURL(info)))
		return
	}
	progress := func(read, total int64) {
		c.set(func(s *State) { s.Phase, s.Read, s.Total = Downloading, read, total })
	}
	c.set(func(s *State) { s.Phase, s.Read, s.Total, s.Message = Downloading, 0, 0, "" })

	// The folders this uses are the ones CleanOld empties: not both at once.
	c.fsMu.Lock()
	defer c.fsMu.Unlock()

	notice := ""
	canWrite := c.installDirWritable()
	if info.HasSupportedPackage(c.Client.runtime()) && !canWrite {
		if !c.hasInstaller(info) {
			fail("更新失败", "安装目录需要管理员权限，轻量更新无法写入，且这个版本没有提供安装程序。")
			return
		}
		// The helper runs with our rights and could not replace the files; the
		// installer asks for what it needs.
		notice = "安装目录需要管理员权限，将改用完整安装包更新。"
	}
	if info.HasSupportedPackage(c.Client.runtime()) && canWrite {
		// The package made ready in the background, if it is this version's:
		// no need to download it again.
		prep, err := c.takePrepared(info.Version), error(nil)
		if prep == nil {
			prep, err = c.Client.DownloadPackage(ctx, *info, c.Root, progress)
		}
		if err == nil {
			err = c.startPackage(prep, false)
			if err == nil {
				c.done()
				return
			}
			if !c.hasInstaller(info) {
				fail("启动轻量更新失败", err.Error())
				return
			}
			notice = "启动轻量更新失败：" + err.Error() + "\n\n将改用完整安装包更新。"
		} else {
			if !c.hasInstaller(info) {
				fail("轻量更新失败", err.Error())
				return
			}
			notice = "轻量更新失败：" + err.Error() + "\n\n将改用完整安装包更新。"
		}
	}
	if !c.hasInstaller(info) {
		fail("更新失败", "没有可用的安装包下载地址。")
		return
	}
	c.set(func(s *State) { s.Phase, s.Read, s.Total, s.Message = Downloading, 0, 0, notice })
	path, err := c.Client.DownloadInstaller(ctx, *info, c.Root, progress)
	if err != nil {
		fail("下载失败", err.Error())
		return
	}
	if err := c.startInstaller(path, info.Version); err != nil {
		fail("启动安装程序失败", err.Error())
		return
	}
	c.done()
}

// installDirWritable says whether the folder of the program can be written by
// this process, which is what the helper will have.
// hasInstaller says whether the release has an installer that this machine
// can run: the installer of the release tool is for 64-bit Intel/AMD Windows.
func (c *Coordinator) hasInstaller(i *Info) bool {
	return validateInstaller(*i) == nil && strings.EqualFold(c.Client.runtime(), "win-x64")
}

// manualURL is what a user who cannot be updated automatically is sent to: the
// package made for this system when the release has one, else the installer
// (which is for Windows), else whatever there is.
func (c *Coordinator) manualURL(i *Info) string {
	if i.HasSupportedPackage(c.Client.runtime()) {
		return strings.TrimSpace(firstNonEmpty(i.PackageURL, i.InstallerURL))
	}
	if c.hasInstaller(i) {
		return strings.TrimSpace(i.InstallerURL)
	}
	return strings.TrimSpace(firstNonEmpty(i.PackageURL, i.InstallerURL))
}

func (c *Coordinator) installDirWritable() bool {
	if strings.TrimSpace(c.Exe) == "" {
		return true
	}
	dir := filepath.Dir(c.Exe)
	f, err := os.CreateTemp(dir, ".klip-write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func (c *Coordinator) done() {
	c.set(func(s *State) { s.Phase, s.Message = Applying, "正在重启以完成更新..." })
	if c.Shutdown != nil {
		c.Shutdown()
	}
}

func (c *Coordinator) pid() int {
	if c.PID != 0 {
		return c.PID
	}
	return os.Getpid()
}

func (c *Coordinator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// helperCopy copies the running program to a file of its own, which the
// helper runs from: the program's own file cannot be replaced while it runs.
func (c *Coordinator) helperCopy(version string) (string, error) {
	if strings.TrimSpace(c.Exe) == "" {
		return "", errors.New("current executable path is unavailable")
	}
	dir := filepath.Join(c.Root, "helper")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := "maccy-update-" + version
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(dir, name)
	if err := copyFile(c.Exe, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func (c *Coordinator) spawn(helper, plan string) error {
	if c.StartHelper != nil {
		return c.StartHelper(helper, plan)
	}
	p, err := ReadPlan(plan)
	if err != nil {
		return err
	}
	p.ReadyFile = plan + ".ready"
	_ = os.Remove(p.ReadyFile)
	defer os.Remove(p.ReadyFile)
	if err = WritePlan(plan, p); err != nil {
		return err
	}
	cmd, err := startHelperProcess(helper, plan)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = os.Stat(p.ReadyFile); err == nil {
			go cmd.Wait()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("cannot stop timed-out helper: %w", err)
	}
	_ = cmd.Wait()
	return errors.New("update helper readiness timed out")
}

func (c *Coordinator) startPackage(p *Prepared, quiet bool) error {
	for _, f := range p.Manifest.Files {
		target, err := insideRoot(filepath.Dir(c.Exe), f.Path)
		if err != nil {
			return err
		}
		if st, err := os.Stat(target); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0200 == 0 {
			return errors.New("update target is read-only")
		}
	}
	if strings.TrimSpace(c.Exe) == "" {
		return errors.New("current executable path is unavailable")
	}
	installDir := filepath.Dir(c.Exe)
	plan := Plan{
		PID:               c.pid(),
		InstallDir:        installDir,
		StagingDir:        p.StagingPath,
		PackageManifest:   p.PackageManifest,
		InstalledManifest: filepath.Join(c.Root, "installed-package-files.json"),
		BackupDir:         filepath.Join(c.Root, "backups", p.Version+"-"+c.now().Format("20060102-150405")),
		LogPath:           filepath.Join(c.Root, "last_light_update.log"),
		Version:           p.Version,
		ResultPath:        c.ResultPath(),
		Quiet:             quiet,
	}
	return c.launchPlan(plan, p.Version)
}

func (c *Coordinator) startInstaller(path, version string) error {
	return c.launchPlan(Plan{PID: c.pid(), Installer: path, RestartExe: c.Exe, LogPath: filepath.Join(c.Root, "last_light_update.log"),
		Version: version, ResultPath: c.ResultPath()}, version)
}

func (c *Coordinator) launchPlan(plan Plan, version string) error {
	helper, err := c.helperCopy(version)
	if err != nil {
		return err
	}
	planPath := filepath.Join(c.Root, fmt.Sprintf("apply-update-%s.json", version))
	if err := WritePlan(planPath, plan); err != nil {
		return err
	}
	return c.spawn(helper, planPath)
}

// CleanOld removes what earlier updates left that is of no more use: the
// helper copies of the program, the unpacked packages, the downloaded
// packages and installers of this version or older (what is installed, or
// was passed), and the backups of updates but the newest one (kept for a
// rollback by hand).
func (c *Coordinator) CleanOld() {
	c.fsMu.Lock()
	defer c.fsMu.Unlock()
	// If an update has been started, what is in these folders is not old: the
	// helper and the staged files are for the update that is about to run.
	c.mu.Lock()
	applying := c.applying
	if !applying {
		// The staging goes, and with it what Prepare unpacked there (it may
		// have run first, both start with the program): forgotten, so that
		// it is unpacked again from the kept zip, not handed to a helper that
		// would find it gone.
		c.prepared = nil
	}
	c.mu.Unlock()
	if applying {
		return
	}
	_ = os.RemoveAll(filepath.Join(c.Root, "helper"))
	_ = os.RemoveAll(filepath.Join(c.Root, "staging"))
	c.removeOldDownloads(filepath.Join(c.Root, "packages"))
	c.removeOldDownloads(c.Root)
	keepNewestBackup(filepath.Join(c.Root, "backups"))
}

// removeOldDownloads removes from dir the packages and installers
// (maccy-<version>-...) of this version or older. A file whose version
// cannot be read is left: it is not known to be one of ours.
func (c *Coordinator) removeOldDownloads(dir string) {
	current, ok := ParseVersion(NormalizeVersion(c.Client.Current))
	if !ok {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, "maccy-") ||
			!(strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, "-setup.exe")) {
			continue
		}
		ver, _, _ := strings.Cut(strings.TrimPrefix(name, "maccy-"), "-")
		v, ok := ParseVersion(ver)
		if !ok || v.Compare(current) > 0 {
			continue // newer: an update not yet installed
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// keepNewestBackup removes the backup folders in dir but the newest one.
func keepNewestBackup(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type backup struct {
		name string
		at   time.Time
	}
	var all []backup
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		all = append(all, backup{e.Name(), info.ModTime()})
	}
	if len(all) < 2 {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	for _, b := range all[1:] {
		_ = os.RemoveAll(filepath.Join(dir, b.name))
	}
}

// ResultPath is where the helper writes how the last update went.
func (c *Coordinator) ResultPath() string { return filepath.Join(c.Root, "last-update-result.json") }
