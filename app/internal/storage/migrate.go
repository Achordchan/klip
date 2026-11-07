// Package storage migrates a stopped application's data without deleting its source.
package storage

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"klip/internal/durable"
	"klip/internal/paths"
)

func Validate(source, target string) (string, error) {
	src, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", err
	}
	src, err = filepath.Abs(src)
	if err != nil {
		return "", err
	}
	dst, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	dst, err = filepath.Abs(dst)
	if err != nil {
		return "", err
	}
	if inside(src, dst) || inside(dst, src) || sameOrWithin(src, dst) || sameOrWithin(dst, src) {
		return "", errors.New("新目录不能与旧目录相同或互相包含")
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		return "", err
	}
	if len(entries) != 0 {
		return "", errors.New("请选择空文件夹，避免覆盖已有文件")
	}
	return dst, nil
}

// relUnder gives the place of p below root, when p is below it. The spelling
// decides first; then, as for sameOrWithin, the identity of each folder above
// p: a path saved with another capitalization, or through another name of the
// folder (/var and /private/var on macOS), names the same files and is moved
// with them.
func relUnder(root string, rootInfo os.FileInfo, p string) (string, bool) {
	if p == "" || !filepath.IsAbs(p) {
		return "", false
	}
	if inside(root, p) {
		rel, err := filepath.Rel(root, p)
		return rel, err == nil
	}
	for q := filepath.Dir(p); ; {
		if info, err := os.Stat(q); err == nil && os.SameFile(info, rootInfo) {
			rel, err := filepath.Rel(q, p)
			return rel, err == nil
		}
		up := filepath.Dir(q)
		if up == q {
			return "", false
		}
		q = up
	}
}

// sameOrWithin says whether path is root or below it by what the file system
// says, not by the spelling: on a volume that ignores case (macOS, Windows),
// /data/Klip and /data/klip/new are one inside the other while their names
// differ. path and each folder above it are compared with root by identity.
func sameOrWithin(root, path string) bool {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	for p := path; ; {
		if info, err := os.Stat(p); err == nil && os.SameFile(info, rootInfo) {
			return true
		}
		up := filepath.Dir(p)
		if up == p {
			return false
		}
		p = up
	}
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Migrate requires all source writers to be stopped. Selection changes last;
// failed attempts remove only files created by this invocation.
func Migrate(source, target, selection string, progress func(int, int)) error {
	dst, err := Validate(source, target)
	if err != nil {
		return err
	}
	// Before anything is copied: the settings, with the login's tokens, must
	// not inherit what the chosen folder lets others do.
	if err := makePrivate(dst); err != nil {
		return fmt.Errorf("无法把新目录设为仅当前用户可访问：%w", err)
	}
	src, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	for _, name := range []string{"pending-merge.json", "upload-intent.json"} {
		if _, err := os.Stat(filepath.Join(src, "sync", name)); err == nil {
			return errors.New("存在未完成的同步恢复记录，请先完成同步再迁移")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	var files []string
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("存储目录包含符号链接，不能迁移：%s", p)
		}
		rel, _ := filepath.Rel(src, p)
		if recoveryRecord(rel) {
			return errors.New("存在未完成的文件恢复记录，请重启后再迁移")
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("不支持的存储文件：%s", p)
		}
		if rel == "storage-location.json" {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return err
	}
	var created []string
	committed := false
	defer func() {
		if !committed {
			for i := len(created) - 1; i >= 0; i-- {
				_ = os.Remove(created[i])
			}
		}
	}()
	for i, rel := range files {
		out := filepath.Join(dst, rel)
		dir := filepath.Dir(out)
		var missing []string
		for p := dir; p != dst; p = filepath.Dir(p) {
			if _, err := os.Lstat(p); os.IsNotExist(err) {
				missing = append(missing, p)
			} else if err != nil {
				return err
			}
		}
		for j := len(missing) - 1; j >= 0; j-- {
			if err := os.Mkdir(missing[j], 0700); err != nil {
				return err
			}
			created = append(created, missing[j])
		}
		if err := copyChecked(filepath.Join(src, rel), out); err != nil {
			return err
		}
		created = append(created, out)
		if progress != nil {
			progress(i+1, len(files))
		}
	}
	// Keep arbitrary JSON fields; rewrite only image/file payload paths in history.
	hp := filepath.Join(dst, "history.json")
	if b, err := os.ReadFile(hp); err == nil {
		var items []map[string]json.RawMessage
		if err = json.Unmarshal(b, &items); err != nil {
			return fmt.Errorf("历史记录无法解析：%w", err)
		}
		srcInfo, err := os.Stat(src)
		if err != nil {
			return err
		}
		remap := func(p string) string {
			if rel, ok := relUnder(src, srcInfo, p); ok {
				return filepath.Join(dst, rel)
			}
			return p
		}
		for _, it := range items {
			var image string
			if json.Unmarshal(it["ImageFilePath"], &image) == nil && image != "" {
				it["ImageFilePath"], _ = json.Marshal(remap(image))
			}
			var list []string
			if json.Unmarshal(it["FilePaths"], &list) == nil && list != nil {
				for i := range list {
					list[i] = remap(list[i])
				}
				it["FilePaths"], _ = json.Marshal(list)
			}
		}
		b, err = json.Marshal(items)
		if err != nil {
			return err
		}
		// A successful quiescence has already resolved normal history writes. Preserve
		// the saved local mutation suffix rather than silently discarding its evidence.
		original, _ := os.ReadFile(hp)
		end := len(original)
		for end > 0 && strings.ContainsRune(" \t\r\n", rune(original[end-1])) {
			end--
		}
		b = append(b, original[end:]...)
		if err = os.WriteFile(hp, b, 0600); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := durable.SyncTree(dst); err != nil {
		return err
	}
	if err := durable.SyncParent(dst); err != nil {
		return err
	}
	if err := paths.SaveSelection(selection, dst); err != nil {
		// Rename may have succeeded before directory sync failed. Never delete a
		// destination that the selector may already reference.
		if b, readErr := os.ReadFile(selection); readErr == nil {
			var selected struct{ Root string }
			if json.Unmarshal(b, &selected) == nil && selected.Root == dst {
				committed = true
			}
		}
		return err
	}
	committed = true
	return nil
}
func copyChecked(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		out.Close()
		if err != nil {
			os.Remove(dst)
		}
	}()
	h := sha256.New()
	if _, err = io.Copy(io.MultiWriter(out, h), in); err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	f, err := os.Open(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	verify := sha256.New()
	if _, err = io.Copy(verify, f); err != nil {
		return err
	}
	if string(h.Sum(nil)) != string(verify.Sum(nil)) {
		return errors.New("迁移文件校验失败")
	}
	return nil
}

// recoveryRecord says whether rel (relative to the data folder) is a record
// of an interrupted change that a restart finishes: the journal of a payload
// replacement (sync/payload-journal.json), or the backup of a captured file
// folder and its marker (files/<id>.capture-backup[.capture-time]) with what
// is inside. Only those places are looked at: a captured file of the user may
// have any name.
func recoveryRecord(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 2 && parts[0] == "sync" && strings.HasPrefix(parts[1], "payload-journal") {
		return true
	}
	if len(parts) >= 2 && parts[0] == "files" {
		name := parts[1]
		return strings.HasSuffix(name, ".capture-backup") || strings.HasSuffix(name, ".capture-backup.capture-time")
	}
	return false
}
