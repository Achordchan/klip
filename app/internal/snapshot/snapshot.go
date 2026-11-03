// Package snapshot writes and reads the sync snapshot: a SQLite file that
// holds the history and the portable settings, in the format of
// SnapshotSqliteService (schema 2; schema 1, which carried the file bytes
// inside, is still read). Klip 1.0.10 and 1.0.11 devices exchange these
// files, so the tables, the column types and the text formats below are the
// C# ones, not a free choice.
package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/plugins/sqlite"

	"klip/internal/durable"
	"klip/internal/model"
	"klip/internal/paths"
)

// SchemaVersion is the schema this package writes.
const SchemaVersion = 2

// ddl is the text SnapshotSqliteService runs, kept as it is.
const ddl = `
PRAGMA journal_mode=MEMORY;
PRAGMA synchronous=OFF;
PRAGMA temp_store=MEMORY;
PRAGMA locking_mode=EXCLUSIVE;

CREATE TABLE meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE settings (
  json TEXT NOT NULL
);

CREATE TABLE items (
  position INTEGER NOT NULL,
  id TEXT PRIMARY KEY,
  kind INTEGER NOT NULL,
  capturedAt TEXT NOT NULL,
  approxBytes INTEGER NOT NULL,
  text TEXT,
  filePathsJson TEXT,
  pinned INTEGER NOT NULL,
  copyCount INTEGER NOT NULL,
  firstCapturedAt TEXT,
  note TEXT,
  contentHash TEXT,
  sourceAppName TEXT,
  sourceAppPath TEXT
);

CREATE TABLE item_images (
  itemId TEXT PRIMARY KEY,
  fileName TEXT NOT NULL,
  blobSha256 TEXT,
  sizeBytes INTEGER,
  bytes BLOB
);

CREATE TABLE item_files (
  itemId TEXT NOT NULL,
  idx INTEGER NOT NULL,
  fileName TEXT NOT NULL,
  blobSha256 TEXT,
  sizeBytes INTEGER,
  bytes BLOB,
  PRIMARY KEY(itemId, idx)
);
`

// Blob is a file that the snapshot refers to by hash and that has to be
// uploaded next to it.
type Blob struct {
	SHA256     string
	SourcePath string
	Size       int64
}

// ExportResult describes the snapshot file that Export wrote.
type ExportResult struct {
	Size   int64
	SHA256 string
	Blobs  []Blob
}

// Export writes the snapshot of items and settings to snapshotPath. The
// image and file contents are not inside it: it names them by SHA-256, and
// Blobs lists where they are. It fails when an item's content is missing.
func Export(ctx context.Context, l paths.Layout, snapshotPath string, items []model.Item, s model.Settings) (ExportResult, error) {
	if strings.TrimSpace(snapshotPath) == "" {
		return ExportResult{}, errors.New("snapshot: path is empty")
	}
	// A folder made here is the user's own; one that is there (a shared
	// temporary folder) keeps its permissions: the snapshot is kept private
	// by its own.
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		return ExportResult{}, err
	}
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if err := os.Remove(snapshotPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ExportResult{}, err
		}
	}
	// The snapshot carries the clipboard history: it is for this user only,
	// from the moment it exists (SQLite opens the empty file, and gives its
	// journal the same permissions).
	f, err := os.OpenFile(snapshotPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ExportResult{}, err
	}
	if err := f.Close(); err != nil {
		return ExportResult{}, err
	}

	blobs := map[string]Blob{}
	if err := writeDB(ctx, l, snapshotPath, items, s, blobs); err != nil {
		return ExportResult{}, err
	}

	_ = os.Chmod(snapshotPath, 0o600) // a umask cannot widen it, but be sure
	st, err := os.Stat(snapshotPath)
	if err != nil {
		return ExportResult{}, err
	}
	sum, err := fileSHA256(snapshotPath)
	if err != nil {
		return ExportResult{}, err
	}
	list := make([]Blob, 0, len(blobs))
	for _, b := range blobs {
		stable, err := stageExportBlob(l, b)
		if err != nil {
			return ExportResult{}, err
		}
		b.SourcePath = stable
		list = append(list, b)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].SHA256 < list[j].SHA256 })
	return ExportResult{Size: st.Size(), SHA256: sum, Blobs: list}, nil
}

func writeDB(ctx context.Context, l paths.Layout, path string, items []model.Item, s model.Settings, blobs map[string]Blob) error {
	db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		return err
	}
	defer db.Close()

	// The plugin runs one statement at a time, so the script is cut into
	// its statements. The PRAGMAs only tune how the file is written.
	for _, stmt := range splitStatements(ddl) {
		if _, err := db.Execute(ctx, stmt); err != nil {
			return fmt.Errorf("snapshot: %q: %w", firstLine(stmt), err)
		}
	}

	stmts := []sqlite.Statement{{SQL: "INSERT INTO meta(key, value) VALUES (?, ?)", Args: []any{"schema", strconv.Itoa(SchemaVersion)}}}
	if j, err := s.Portable().Marshal(); err == nil && len(j) > 0 {
		stmts = append(stmts, sqlite.Statement{SQL: "INSERT INTO settings(json) VALUES (?)", Args: []any{string(j)}})
	}
	itemStmts, err := itemStatements(ctx, l, items, blobs)
	if err != nil {
		return err
	}
	stmts = append(stmts, itemStmts...)

	// One transaction: the file is complete or Export fails.
	if _, err := db.Transaction(ctx, stmts); err != nil {
		return err
	}
	return db.Close()
}

// splitStatements cuts a script at its semicolons. The DDL has no semicolon
// inside a string or a comment.
func splitStatements(script string) []string {
	var out []string
	for _, part := range strings.Split(script, ";") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// itemStatements builds the statements that insert the items, with their
// images and files, and records the blobs they refer to.
func itemStatements(ctx context.Context, l paths.Layout, items []model.Item, blobs map[string]Blob) ([]sqlite.Statement, error) {
	var stmts []sqlite.Statement
	seen := map[string]bool{}
	position := 0
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := strings.ToLower(it.ID)
		if seen[id] {
			continue
		}
		seen[id] = true

		var filePathsJSON any
		if it.FilePaths != nil {
			b, err := json.Marshal(it.FilePaths)
			if err != nil {
				return nil, err
			}
			filePathsJSON = string(b)
		}
		var firstCaptured any
		if it.FirstCapturedAt != nil {
			firstCaptured = model.FormatO(it.FirstCapturedAt.Time)
		}
		stmts = append(stmts, sqlite.Statement{SQL: `
INSERT OR REPLACE INTO items(
  position, id, kind, capturedAt, approxBytes, text, filePathsJson, pinned, copyCount,
  firstCapturedAt, note, contentHash, sourceAppName, sourceAppPath
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			Args: []any{position, id, int(it.Kind), model.FormatO(it.CapturedAt.Time), it.ApproxBytes,
				nullable(it.Text), filePathsJSON, boolInt(it.Pinned), it.CopyCount, firstCaptured,
				nullable(it.Note), nullable(it.ContentHash), nullable(it.SourceAppName), nullable(it.SourceAppPath)}})

		var err error
		switch it.Kind {
		case model.KindImage:
			var st sqlite.Statement
			if st, err = imageStatement(it, id, blobs); err == nil {
				stmts = append(stmts, st)
			}
		case model.KindFileList:
			var sts []sqlite.Statement
			if sts, err = fileStatements(l, it, id, blobs); err == nil {
				stmts = append(stmts, sts...)
			}
		}
		if err != nil {
			return nil, err
		}
		position++
	}
	return stmts, nil
}

func imageStatement(it model.Item, id string, blobs map[string]Blob) (sqlite.Statement, error) {
	p := model.Str(it.ImageFilePath)
	size, sum, ok := hashFile(p)
	if !ok || size <= 0 {
		return sqlite.Statement{}, errors.New("snapshot: local image payload missing")
	}
	name := filepath.Base(p)
	if name == "" || name == "." {
		name = "image.png"
	}
	blobs[sum] = Blob{SHA256: sum, SourcePath: p, Size: size}
	return sqlite.Statement{
		SQL:  "INSERT OR REPLACE INTO item_images(itemId, fileName, blobSha256, sizeBytes, bytes) VALUES (?, ?, ?, ?, NULL)",
		Args: []any{id, name, sum, size},
	}, nil
}

type cachedFile struct {
	idx  int
	name string
	path string
}

func fileStatements(l paths.Layout, it model.Item, id string, blobs map[string]Blob) ([]sqlite.Statement, error) {
	idn, err := itemIDN(it.ID)
	if err != nil {
		return nil, err
	}
	dir, err := itemDir(l.Files(), idn)
	if err != nil {
		return nil, err
	}
	// An imported item keeps its files in a unique folder under files/; a
	// migrated one keeps them in files/<idn> and names the documents.
	if len(it.FilePaths) > 0 {
		if d := filepath.Dir(it.FilePaths[0]); within(l.Files(), d) {
			dir = d
		}
	}
	files := cachedFiles(dir)
	wantFiles := 0
	for _, p := range it.FilePaths {
		if strings.TrimSpace(p) != "" {
			wantFiles++
		}
	}
	if len(files) > 0 && wantFiles > 0 {
		// Export exactly the files the item lists: they must all be in the
		// cache, but surplus files left by an older import are harmless.
		byKey := make(map[string]cachedFile, len(files))
		byIdx := make(map[int][]cachedFile, len(files))
		for _, f := range files {
			byKey[cacheKey(f.idx, f.name)] = f
			byIdx[f.idx] = append(byIdx[f.idx], f)
		}
		// at is the cached file of the document at position i called name:
		// of that position and that name as the cache writes it
		// (sanitizeFileName; Windows does not tell case apart). Another file of
		// that position is a stale one from an earlier import, not this
		// document: exporting it would publish the wrong bytes.
		at := func(i int, name string) (cachedFile, error) {
			if f, ok := byKey[cacheKey(i, name)]; ok {
				return f, nil
			}
			safe := sanitizeFileName(name)
			if f, ok := byKey[cacheKey(i, safe)]; ok {
				return f, nil
			}
			for _, f := range byIdx[i] {
				if strings.EqualFold(f.name, name) || strings.EqualFold(f.name, safe) {
					return f, nil
				}
			}
			return cachedFile{}, fmt.Errorf("snapshot: local file payload missing (%q at position %d)", name, i)
		}
		selected := make([]cachedFile, 0, wantFiles)
		for i, p := range it.FilePaths {
			if strings.TrimSpace(p) == "" {
				continue
			}
			name := docName(p)
			// Only a path inside Klip's files folder names a cached file; an
			// original document called "2026_report.pdf" is not index 2026.
			if !within(l.Files(), filepath.Dir(filepath.Clean(p))) {
				f, err := at(i, name)
				if err != nil {
					return nil, err
				}
				selected = append(selected, f)
				continue
			}
			if idx, cut := indexPrefix(name); cut > 0 {
				f, ok := byKey[cacheKey(idx, name[cut:])]
				if !ok {
					return nil, errors.New("snapshot: local file payload missing")
				}
				selected = append(selected, f)
				continue
			}
			f, err := at(i, name)
			if err != nil {
				return nil, err
			}
			selected = append(selected, f)
		}
		files = selected
	}
	if len(files) == 0 {
		var srcs []string
		for _, p := range it.FilePaths {
			if strings.TrimSpace(p) != "" {
				srcs = append(srcs, p)
			}
		}
		if len(srcs) == 0 {
			return nil, errors.New("snapshot: local file payload missing")
		}
		for i, p := range srcs {
			if st, err := os.Stat(p); err != nil || st.IsDir() {
				continue
			}
			files = append(files, cachedFile{i, filepath.Base(p), p})
		}
		if len(files) != len(srcs) {
			return nil, errors.New("snapshot: local file payload missing")
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].idx < files[j].idx })

	var stmts []sqlite.Statement
	for _, f := range files {
		size, sum, ok := hashFile(f.path)
		if !ok {
			continue
		}
		blobs[sum] = Blob{SHA256: sum, SourcePath: f.path, Size: size}
		stmts = append(stmts, sqlite.Statement{
			SQL:  "INSERT OR REPLACE INTO item_files(itemId, idx, fileName, blobSha256, sizeBytes, bytes) VALUES (?, ?, ?, ?, ?, NULL)",
			Args: []any{id, f.idx, f.name, sum, size},
		})
	}
	// Like the C# client: a file that cannot be exported fails the item.
	if len(stmts) != len(files) {
		return nil, errors.New("snapshot: local file payload missing")
	}
	return stmts, nil
}

// itemIDN validates the GUID of an item and returns the folder name the C#
// client uses (Guid.ToString("N")). A snapshot is not trusted: an id that is
// not a GUID is refused rather than becoming a path.
func itemIDN(id string) (string, error) {
	n := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
	if len(n) != 32 {
		return "", fmt.Errorf("snapshot: item id %q is not a GUID", id)
	}
	if _, err := hex.DecodeString(n); err != nil {
		return "", fmt.Errorf("snapshot: item id %q is not a GUID", id)
	}
	return n, nil
}

// itemDir is the cache folder of an item under base, checked to stay there.
func itemDir(base, idn string) (string, error) {
	p := filepath.Join(base, idn)
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || filepath.IsAbs(rel) ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("snapshot: item path %q escapes %q", idn, base)
	}
	return p, nil
}

// cachedFiles lists dir/NNNN_name files: the copies the client keeps of the
// files of a file-list item.
func cachedFiles(dir string) []cachedFile {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []cachedFile
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		idx, cut := indexPrefix(name)
		if idx < 0 {
			continue
		}
		short := name[cut:]
		if strings.TrimSpace(short) == "" {
			short = name
		}
		out = append(out, cachedFile{idx, short, filepath.Join(dir, name)})
	}
	return out
}

// cacheKey is how a cached file is matched with the FilePaths entry that
// names it.
func cacheKey(idx int, name string) string {
	return strconv.Itoa(idx) + ":" + name
}

// indexPrefix returns N of a name that starts with digits and "_", and where
// the name starts. Indices above 9999 are written with more digits by
// fmt.Sprintf("%04d", n), so the prefix is not limited to four.
func indexPrefix(name string) (int, int) {
	i := strings.IndexByte(name, '_')
	if i < 4 {
		return -1, 0
	}
	for _, c := range name[:i] {
		if c < '0' || c > '9' {
			return -1, 0
		}
	}
	n, err := strconv.Atoi(name[:i])
	if err != nil {
		return -1, 0
	}
	return n, i + 1
}

// ReadResult is what a snapshot holds, with its images and files written
// into the data folder.
type ReadResult struct {
	Settings   *model.Settings
	Items      []model.Item
	ImageCount int
	FileCount  int
}

// EnsureBlob fetches a blob that is not in the local store, and returns where
// it put it. It may be nil.
type EnsureBlob func(sha256 string) (string, error)

// Read reads a snapshot. Images and files are staged first and applied as a
// transaction: the caller must Commit after saving the history, or Rollback.
// The old cache of an item is not replaced in place, so a failed save leaves
// the previous history and its files intact.
func Read(ctx context.Context, l paths.Layout, snapshotPath string, ensure EnsureBlob) (ReadResult, *AppliedPayloads, error) {
	if err := os.MkdirAll(l.Root, 0o700); err != nil {
		return ReadResult{}, nil, err
	}
	root, err := os.MkdirTemp(l.Root, ".read-*")
	if err != nil {
		return ReadResult{}, nil, err
	}
	defer os.RemoveAll(root)
	res, err := ReadDetached(ctx, l, root, snapshotPath, ensure)
	if err != nil {
		return ReadResult{}, nil, err
	}
	out, applied, err := ApplyPayloads(l, root, res.Items, nil)
	if err != nil {
		return ReadResult{}, nil, err
	}
	res.Items = out
	return res, applied, nil
}

// ReadDetached reads a snapshot like Read, but writes its payloads under
// root (a folder of the caller's) instead of the data folders, and the items
// point there. It is for a merge: which payloads are wanted is known only
// after the items are merged, and the local payloads of an item that the
// merge keeps must not be replaced first. ApplyPayloads moves the payloads
// of the items that were kept.
func ReadDetached(ctx context.Context, l paths.Layout, root, snapshotPath string, ensure EnsureBlob) (ReadResult, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return ReadResult{}, err
	}
	return read(ctx, l, root, filepath.Join(root, "images"), filepath.Join(root, "files"), snapshotPath, ensure)
}

func read(ctx context.Context, l paths.Layout, stagingRoot, imagesDir, filesDir, snapshotPath string, ensure EnsureBlob) (ReadResult, error) {
	if _, err := os.Stat(snapshotPath); err != nil {
		return ReadResult{}, fmt.Errorf("snapshot not found: %w", err)
	}
	db, err := sqlite.Open(ctx, snapshotPath, sqlite.OpenOptions{ReadOnly: true})
	if err != nil {
		return ReadResult{}, err
	}
	defer db.Close()

	schemaRows, err := db.Query(ctx, "SELECT value FROM meta WHERE key = ? LIMIT 1", "schema")
	if err != nil {
		return ReadResult{}, err
	}
	schema, err := strconv.Atoi(strings.TrimSpace(cellString(first(schemaRows), 0)))
	if err != nil || (schema != 1 && schema != SchemaVersion) {
		return ReadResult{}, errors.New("unsupported snapshot schema")
	}

	settingsRows, err := db.Query(ctx, "SELECT json FROM settings LIMIT 1")
	if err != nil {
		return ReadResult{}, err
	}
	settingsJSON := cellString(first(settingsRows), 0)
	items, err := readItems(ctx, db)
	if err != nil {
		return ReadResult{}, err
	}

	staging, err := newImportStaging(l, stagingRoot, imagesDir, filesDir)
	if err != nil {
		return ReadResult{}, err
	}
	defer staging.cleanup()

	var res ReadResult
	for i, it := range items {
		switch it.Kind {
		case model.KindImage:
			img, err := readImage(ctx, db, it.ID, schema)
			if err != nil {
				return ReadResult{}, err
			}
			p, err := staging.addImage(i, img, ensure)
			if err != nil {
				return ReadResult{}, err
			}
			res.ImageCount++
			items[i].ImageFilePath = &p

		case model.KindFileList:
			files, err := readFiles(ctx, db, it.ID, schema)
			if err != nil {
				return ReadResult{}, err
			}
			if len(files) == 0 {
				return ReadResult{}, errors.New("snapshot file payload missing")
			}
			local, err := staging.addFiles(i, it, files, ensure)
			if err != nil {
				return ReadResult{}, err
			}
			res.FileCount += len(local)
			items[i].FilePaths = local
		}
	}
	if err := staging.commit(); err != nil {
		return ReadResult{}, err
	}
	res.Items = items

	if strings.TrimSpace(settingsJSON) != "" {
		if s, err := model.ParseSettings([]byte(settingsJSON)); err == nil {
			res.Settings = &s
		}
	}
	return res, nil
}

// importStaging holds the payloads of a snapshot until all of them are
// there. Only then are they moved into the data folder, so that a failure
// halfway through (a blob the server does not have, say) cannot replace the
// files of an item whose history is kept.
type importStaging struct {
	layout    paths.Layout
	dir       string
	imagesDir string
	filesDir  string
	files     []stagedFile
	dirs      []stagedDir
}

type stagedFile struct{ from, to string }

type stagedDir struct{ from, to string }

func newImportStaging(l paths.Layout, root, imagesDir, filesDir string) (*importStaging, error) {
	// The staging folder is a child of root, so that the moves below stay on
	// one file system; it is private and removed afterwards.
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, ".import-")
	if err != nil {
		return nil, err
	}
	return &importStaging{layout: l, dir: dir, imagesDir: imagesDir, filesDir: filesDir}, nil
}

func (s *importStaging) cleanup() {
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
}

// addImage stages the bytes of an image and returns the path it will have in
// the images folder.
func (s *importStaging) addImage(n int, img imageRecord, ensure EnsureBlob) (string, error) {
	data := img.bytes
	if !img.inline && len(data) == 0 {
		if strings.TrimSpace(img.sha256) == "" {
			return "", errors.New("snapshot image blob missing")
		}
		blob, err := localBlobPath(s.layout, img.sha256, ensure)
		if err != nil {
			return "", err
		}
		if data, err = os.ReadFile(blob); err != nil {
			return "", err
		}
	}
	ext := filepath.Ext(sanitizeFileName(img.fileName))
	ext = truncateUTF8(ext, 32)
	if ext == "" {
		ext = ".png"
	}
	name := fmt.Sprintf("%d_%s%s", time.Now().UnixMilli(), newGUIDN(), ext)
	from := filepath.Join(s.dir, fmt.Sprintf("image-%d", n))
	if err := os.WriteFile(from, data, 0o600); err != nil {
		return "", err
	}
	to := filepath.Join(s.imagesDir, name)
	s.files = append(s.files, stagedFile{from: from, to: to})
	return to, nil
}

// addFiles stages the files of a file-list item and returns where they will
// be. The whole folder is replaced when the import is committed, so that
// the files of an older copy of the same item do not survive next to the
// imported ones.
func (s *importStaging) addFiles(n int, it model.Item, files []fileRecord, ensure EnsureBlob) ([]string, error) {
	idn, err := itemIDN(it.ID)
	if err != nil {
		return nil, err
	}
	target, err := itemDir(s.filesDir, idn)
	if err != nil {
		return nil, err
	}
	from := filepath.Join(s.dir, fmt.Sprintf("files-%d", n))
	if err := os.MkdirAll(from, 0o700); err != nil {
		return nil, err
	}
	var local []string
	for _, f := range files {
		name := importedFileName(f.idx, f.name)
		if err := writeFileRecord(s.layout, filepath.Join(from, name), f, ensure); err != nil {
			return nil, err
		}
		local = append(local, filepath.Join(target, name))
	}
	s.dirs = append(s.dirs, stagedDir{from: from, to: target})
	return local, nil
}

func writeFileRecord(l paths.Layout, dest string, f fileRecord, ensure EnsureBlob) error {
	if f.inline {
		return os.WriteFile(dest, f.bytes, 0o600)
	}
	if strings.TrimSpace(f.sha256) == "" {
		return errors.New("snapshot file blob missing")
	}
	blob, err := localBlobPath(l, f.sha256, ensure)
	if err != nil {
		return err
	}
	return copyPayload(blob, dest)
}

func copyPayload(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// commit moves the staged payloads into place. If a move fails, the moves
// that already happened are undone, so that a failure cannot leave an item
// with a mix of old and new files. The folders that were replaced are
// deleted once every move has succeeded.
func (s *importStaging) commit() error {
	var undo []func()
	var stale []string
	rollback := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	for _, f := range s.files {
		if err := os.MkdirAll(filepath.Dir(f.to), 0o755); err != nil {
			rollback()
			return err
		}
		if err := os.Rename(f.from, f.to); err != nil {
			rollback()
			return err
		}
		to := f.to
		undo = append(undo, func() { _ = os.Remove(to) })
	}
	for _, d := range s.dirs {
		if err := os.MkdirAll(filepath.Dir(d.to), 0o755); err != nil {
			rollback()
			return err
		}
		backup, err := moveDir(d.from, d.to)
		if err != nil {
			rollback()
			return err
		}
		if backup != "" {
			stale = append(stale, backup)
		}
		to, old := d.to, backup
		undo = append(undo, func() {
			_ = os.RemoveAll(to)
			if old != "" {
				_ = os.Rename(old, to)
			}
		})
	}
	for _, p := range stale {
		_ = os.RemoveAll(p)
	}
	return nil
}

// moveDir moves staged to target, keeping a target that was there as
// target.old-<guid>; the caller deletes it when the import is complete.
func moveDir(staged, target string) (backup string, err error) {
	backup = target + ".old-" + newGUIDN()
	if err := moveDirTo(staged, target, backup); err != nil {
		return "", err
	}
	if _, err := os.Stat(backup); err != nil {
		return "", nil
	}
	return backup, nil
}

// moveDirTo is moveDir with the backup name the journal already records.
func moveDirTo(staged, target, backup string) error {
	had := false
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			return err
		}
		had = true
	}
	if err := os.Rename(staged, target); err != nil {
		if had {
			_ = os.Rename(backup, target)
		}
		return err
	}
	return nil
}

// AppliedPayloads is a set of payload moves of ApplyPayloads. The caller
// commits it once the history replacement that selects these payloads has
// succeeded, or rolls it back when that fails: the files of the old history
// stay until both are in place.
type payloadUndo struct {
	target string
	undo   func()
}

type AppliedPayloads struct {
	undo    []payloadUndo
	stale   []string
	journal string
	entries []payloadJournalEntry
	before  string
	after   string
}

// payloadJournalEntry is one cache-folder replacement that has not been
// committed by saving the history yet.
type payloadJournalEntry struct {
	Target string            `json:"target"`
	Backup string            `json:"backup"`
	Files  map[string]string `json:"files,omitempty"`
}

type payloadJournal struct {
	Entries   []payloadJournalEntry `json:"entries"`
	Committed bool                  `json:"committed,omitempty"`
	// Before and After are the digests of the history before and after the
	// replacement: they tell a saved replacement from an interrupted one even
	// when the filenames are the same.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

func historyDigest(items []model.Item) string {
	b, err := json.Marshal(items)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// payloadEntryCommitted says the saved history refers to the payloads the
// journal installed: every file it names in the target exists and hashes to
// the installed bytes. Concurrent captures elsewhere do not matter.
func payloadEntryCommitted(items []model.Item, e payloadJournalEntry) bool {
	base := strings.ToLower(filepath.Base(e.Target))
	var owner *model.Item
	for i := range items {
		if strings.ToLower(strings.ReplaceAll(strings.ToLower(items[i].ID), "-", "")) == base {
			owner = &items[i]
			break
		}
	}
	// The installed files must be there: without them the replacement never
	// finished, whatever the history says.
	for name, want := range e.Files {
		sum, err := fileSHA256(filepath.Join(e.Target, name))
		if err != nil || !strings.EqualFold(sum, want) {
			return false
		}
	}
	if owner == nil {
		return true // the item is gone: nothing refers to the folder
	}
	for _, p := range owner.FilePaths {
		if filepath.Dir(filepath.Clean(p)) != e.Target {
			continue
		}
		want, ok := e.Files[filepath.Base(p)]
		if !ok {
			return false
		}
		sum, err := fileSHA256(p)
		if err != nil || !strings.EqualFold(sum, want) {
			return false
		}
	}
	return true
}

func payloadJournalPath(l paths.Layout) string {
	return filepath.Join(l.Root, "sync", "payload-journal.json")
}

func writePayloadJournal(path string, entries []payloadJournalEntry, before, after string, committed bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(payloadJournal{Entries: entries, Committed: committed, Before: before, After: after})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RecoverPayloads rolls back cache-folder replacements whose journal says
// they were never committed. It runs before the history is normalized, so an
// item whose files only exist under a .old-* backup is not dropped.
func RecoverPayloads(l paths.Layout, items []model.Item) {
	path := payloadJournalPath(l)
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var j payloadJournal
	if json.Unmarshal(b, &j) != nil {
		_ = os.Remove(path)
		return
	}
	cur := historyDigest(items)
	for _, e := range j.Entries {
		if e.Target != "" && e.Backup == "" {
			if !within(l.Images(), e.Target) && !within(l.Files(), e.Target) {
				continue
			}
			referenced := false
			for _, it := range items {
				if model.Str(it.ImageFilePath) == e.Target {
					referenced = true
				}
				for _, p := range it.FilePaths {
					if within(e.Target, p) {
						referenced = true
					}
				}
			}
			if !referenced {
				_ = os.RemoveAll(e.Target)
			}
			continue
		}
		if e.Target == "" || e.Backup == "" {
			continue
		}
		committed := j.Committed
		switch {
		case j.After != "" && cur == j.After:
			committed = true
		case j.Before != "" && cur == j.Before:
			committed = false
		case !committed:
			committed = payloadEntryCommitted(items, e)
		}
		if committed {
			_ = os.RemoveAll(e.Backup)
			continue
		}
		if _, err := os.Stat(e.Backup); err != nil {
			continue // the move never started, or was rolled back
		}
		_ = os.RemoveAll(e.Target)
		_ = os.Rename(e.Backup, e.Target)
	}
	_ = os.Remove(path)
}

// Commit marks the replacements as saved and deletes the folders they
// replaced. The journal goes first: a crash after it is gone keeps the new
// files, and only leaves an orphan backup.
func (a *AppliedPayloads) Commit() {
	if a.journal != "" {
		_ = os.Remove(a.journal)
	}
	for _, p := range a.stale {
		_ = os.RemoveAll(p)
	}
	a.undo, a.stale, a.entries = nil, nil, nil
}

// MarkCommitted records that the history referring to the moved payloads was
// saved: a crash before the backups are deleted is then recovered as a
// committed replacement.
func (a *AppliedPayloads) MarkCommitted() {
	if a.journal == "" || len(a.entries) == 0 {
		return
	}
	_ = writePayloadJournal(a.journal, a.entries, a.before, a.after, true)
}

// Rollback undoes the moves: the folders that were replaced are put back.
func (a *AppliedPayloads) Rollback() { a.RollbackKeeping(nil) }

// RollbackKeeping undoes the moves except the payloads keep says are still
// referenced by the restored history.
func (a *AppliedPayloads) RollbackKeeping(keep func(string) bool) {
	for i := len(a.undo) - 1; i >= 0; i-- {
		u := a.undo[i]
		if keep != nil && keep(u.target) {
			continue
		}
		u.undo()
	}
	if a.journal != "" {
		_ = os.Remove(a.journal)
	}
	a.undo, a.stale, a.entries = nil, nil, nil
}

// ApplyPayloads moves the payloads that ReadDetached put under root into the
// data folders, for the items that a merge kept, and rewrites their paths.
// The payloads of items that were not kept stay under root, for the caller to
// delete. The returned handle commits or rolls back the moved folders, so
// that the files change only together with the history.
func ApplyPayloads(l paths.Layout, root string, items, previous []model.Item) ([]model.Item, *AppliedPayloads, error) {
	imagesDir := filepath.Join(root, "images")
	filesDir := filepath.Join(root, "files")
	out := append([]model.Item(nil), items...)
	applied := &AppliedPayloads{journal: payloadJournalPath(l), before: historyDigest(previous)}

	rollback := func() {
		applied.Rollback()
	}

	for i := range out {
		if out[i].Kind != model.KindImage {
			continue
		}
		p := model.Str(out[i].ImageFilePath)
		if !within(imagesDir, p) {
			continue
		}
		if err := os.MkdirAll(l.Images(), 0o755); err != nil {
			rollback()
			return nil, nil, err
		}
		dest := filepath.Join(l.Images(), filepath.Base(p))
		if err := durable.SyncTree(p); err != nil {
			rollback()
			return nil, nil, err
		}
		applied.entries = append(applied.entries, payloadJournalEntry{Target: dest})
		if err := writePayloadJournal(applied.journal, applied.entries, applied.before, "", false); err != nil {
			rollback()
			return nil, nil, err
		}
		if err := durable.Rename(p, dest); err != nil {
			rollback()
			return nil, nil, err
		}
		out[i].ImageFilePath = &dest
		d := dest
		applied.undo = append(applied.undo, payloadUndo{target: d, undo: func() { _ = os.Remove(d) }})
	}

	type dirMove struct{ staged, dest string }
	var moves []dirMove
	for i := range out {
		if out[i].Kind != model.KindFileList || len(out[i].FilePaths) == 0 {
			continue
		}
		idn, err := itemIDN(out[i].ID)
		if err != nil {
			rollback()
			return nil, nil, err
		}
		staged := filepath.Join(filesDir, idn)
		if !within(staged, out[i].FilePaths[0]) {
			continue // the merge kept the payload that is already here
		}
		if err := os.MkdirAll(l.Files(), 0o755); err != nil {
			rollback()
			return nil, nil, err
		}
		// A unique folder, not a replacement of the canonical one: the old
		// cache stays intact until the new history is saved, so a crash
		// cannot pair old history with new files (nor the reverse).
		dest := filepath.Join(l.Files(), idn+"-"+newGUIDN())
		paths := make([]string, len(out[i].FilePaths))
		for j, p := range out[i].FilePaths {
			if within(staged, p) {
				paths[j] = filepath.Join(dest, filepath.Base(p))
			} else {
				paths[j] = p
			}
		}
		out[i].FilePaths = paths
		canonical := filepath.Join(l.Files(), idn)
		referenced := false
		for j, other := range out {
			if j == i {
				continue
			}
			for _, p := range other.FilePaths {
				if within(canonical, p) {
					referenced = true
				}
			}
		}
		if !referenced {
			if st, err := os.Lstat(canonical); err == nil && st.IsDir() && st.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
				applied.stale = append(applied.stale, canonical)
			}
		}
		moves = append(moves, dirMove{staged: staged, dest: dest})
	}

	_ = applied
	for _, m := range moves {
		if err := durable.SyncTree(m.staged); err != nil {
			rollback()
			return nil, nil, err
		}
		applied.entries = append(applied.entries, payloadJournalEntry{Target: m.dest})
		if err := writePayloadJournal(applied.journal, applied.entries, applied.before, "", false); err != nil {
			rollback()
			return nil, nil, err
		}
		if err := durable.Rename(m.staged, m.dest); err != nil {
			rollback()
			return nil, nil, err
		}
		to := m.dest
		applied.undo = append(applied.undo, payloadUndo{target: to, undo: func() { _ = os.RemoveAll(to) }})
	}

	return out, applied, nil
}

// within says p is base or is inside it.
func within(base, p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	rel, err := filepath.Rel(base, p)
	if err != nil || filepath.IsAbs(rel) || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func readItems(ctx context.Context, db *sqlite.DB) ([]model.Item, error) {
	rows, err := db.Query(ctx, `
SELECT position, id, kind, capturedAt, approxBytes, text, filePathsJson, pinned, copyCount,
       firstCapturedAt, note, contentHash, sourceAppName, sourceAppPath
FROM items
ORDER BY position ASC`)
	if err != nil {
		return nil, err
	}

	var items []model.Item
	for _, r := range rows.Values {
		at, err := model.ParseTime(cellString(r, 3))
		if err != nil {
			return nil, err
		}
		id := strings.ToLower(strings.TrimSpace(cellString(r, 1)))
		if _, err := itemIDN(id); err != nil {
			return nil, err
		}
		it := model.Item{
			ID:            id,
			Kind:          model.Kind(cellInt(r, 2)),
			CapturedAt:    model.Time{Time: at},
			ApproxBytes:   cellInt(r, 4),
			Text:          cellStringPtr(r, 5),
			Pinned:        cellInt(r, 7) != 0,
			CopyCount:     int32(cellInt(r, 8)),
			Note:          cellStringPtr(r, 10),
			ContentHash:   cellStringPtr(r, 11),
			SourceAppName: cellStringPtr(r, 12),
			SourceAppPath: cellStringPtr(r, 13),
		}
		if f := cellStringPtr(r, 9); f != nil {
			t, err := model.ParseTime(*f)
			if err != nil {
				return nil, err
			}
			it.FirstCapturedAt = &model.Time{Time: t}
		}
		if j := cellString(r, 6); strings.TrimSpace(j) != "" {
			var paths []string
			if json.Unmarshal([]byte(j), &paths) == nil && len(paths) > 0 {
				it.FilePaths = paths
			}
		}
		items = append(items, it)
	}
	return items, nil
}

type imageRecord struct {
	fileName string
	sha256   string
	bytes    []byte
	// inline says the snapshot carried the bytes (schema 1 always, schema 2
	// when the BLOB is present), even when they are empty.
	inline bool
}

type fileRecord struct {
	idx    int
	name   string
	sha256 string
	bytes  []byte
	inline bool
}

func readImage(ctx context.Context, db *sqlite.DB, id string, schema int) (imageRecord, error) {
	q := "SELECT fileName, bytes FROM item_images WHERE itemId = ? COLLATE NOCASE LIMIT 1"
	if schema >= 2 {
		q = "SELECT fileName, blobSha256, sizeBytes, bytes FROM item_images WHERE itemId = ? COLLATE NOCASE LIMIT 1"
	}
	rows, err := db.Query(ctx, q, id)
	if err != nil {
		return imageRecord{}, err
	}
	r := first(rows)
	if r == nil {
		return imageRecord{}, errors.New("snapshot image payload missing")
	}
	if schema >= 2 {
		data, inline := cellBlob(r, 3)
		return imageRecord{fileName: cellString(r, 0), sha256: cellString(r, 1), bytes: data, inline: inline}, nil
	}
	data, _ := cellBlob(r, 1)
	return imageRecord{fileName: cellString(r, 0), bytes: data, inline: true}, nil
}

func readFiles(ctx context.Context, db *sqlite.DB, id string, schema int) ([]fileRecord, error) {
	q := "SELECT idx, fileName, bytes FROM item_files WHERE itemId = ? COLLATE NOCASE ORDER BY idx ASC"
	if schema >= 2 {
		q = "SELECT idx, fileName, blobSha256, sizeBytes, bytes FROM item_files WHERE itemId = ? COLLATE NOCASE ORDER BY idx ASC"
	}
	rows, err := db.Query(ctx, q, id)
	if err != nil {
		return nil, err
	}
	var out []fileRecord
	for _, r := range rows.Values {
		f := fileRecord{idx: int(cellInt(r, 0)), name: cellString(r, 1)}
		if schema >= 2 {
			f.sha256 = cellString(r, 2)
			f.bytes, f.inline = cellBlob(r, 4)
		} else {
			f.bytes, _ = cellBlob(r, 2)
			f.inline = true
		}
		out = append(out, f)
	}
	return out, nil
}

// localBlobPath finds a blob in the local store, or asks ensure for it. The
// contents are checked against the hash: a truncated file in the store must
// not be imported as the blob, and must not stop ensure from fetching it
// again.
func localBlobPath(l paths.Layout, sha256 string, ensure EnsureBlob) (string, error) {
	local := l.BlobPath(sha256)
	if local == "" {
		return "", errors.New("snapshot: blob hash is not a SHA-256")
	}
	if blobMatches(local, sha256) {
		return local, nil
	}
	if isFile(local) {
		_ = os.Remove(local)
	}
	if ensure != nil {
		p, err := ensure(sha256)
		if err != nil {
			return "", err
		}
		if p != "" && blobMatches(p, sha256) {
			return p, nil
		}
	}
	if blobMatches(local, sha256) {
		return local, nil
	}
	return "", errors.New("snapshot blob missing: " + sha256)
}

func blobMatches(p, sha256 string) bool {
	if !isFile(p) {
		return false
	}
	sum, err := fileSHA256(p)
	return err == nil && strings.EqualFold(sum, strings.TrimSpace(sha256))
}

// docName is the file name of a document path, which may be a Windows one
// (a history from the C# client) on any platform.
func docName(p string) string {
	p = filepath.Clean(p)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// sanitizeFileName replaces what Windows does not allow in a file name. The
// set is Windows' on every platform, so a snapshot imported anywhere gives
// the same names.
func sanitizeFileName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "file"
	}
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`"<>|:*?\/`, r) {
			return '_'
		}
		return r
	}, name)
	// Windows strips a trailing dot or space from a file name, so the file
	// created there would not match the path recorded here.
	name = strings.TrimRight(name, ". ")
	if strings.TrimSpace(name) == "" {
		return "file"
	}
	return name
}

func hashFile(p string) (size int64, sum string, ok bool) {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return 0, "", false
	}
	sum, err = fileSHA256(p)
	if err != nil {
		return 0, "", false
	}
	return st.Size(), sum, true
}

// FileSHA256 is the SHA-256 of a file: the hash a snapshot calls a blob, and
// the hash the server gives a downloaded snapshot.
func FileSHA256(p string) (string, error) { return fileSHA256(p) }

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func nullable(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// first returns the first row, or nil.
func first(r sqlite.Rows) []any {
	if len(r.Values) == 0 {
		return nil
	}
	return r.Values[0]
}

// The cell helpers read a column of a row the plugin returned: values are
// nil, int64, float64, string or []byte.
func cell(row []any, i int) any {
	if i < 0 || i >= len(row) {
		return nil
	}
	return row[i]
}

func cellString(row []any, i int) string {
	switch v := cell(row, i).(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

func cellStringPtr(row []any, i int) *string {
	if cell(row, i) == nil {
		return nil
	}
	s := cellString(row, i)
	return &s
}

func cellInt(row []any, i int) int64 {
	switch v := cell(row, i).(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

func cellBytes(row []any, i int) []byte {
	data, _ := cellBlob(row, i)
	return data
}

// cellBlob says whether the column is present: an empty BLOB is content, a
// NULL is not.
func cellBlob(row []any, i int) ([]byte, bool) {
	switch v := cell(row, i).(type) {
	case []byte:
		return v, true
	case string:
		return []byte(v), true
	}
	return nil, false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// stageExportBlob freezes exported bytes under their content hash. A concurrent
// cache replacement either leaves the expected bytes or fails export validation.
func stageExportBlob(l paths.Layout, b Blob) (string, error) {
	dest := l.BlobPath(b.SHA256)
	if sum, err := fileSHA256(dest); err == nil && strings.EqualFold(sum, b.SHA256) {
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return "", err
	}
	in, err := os.Open(b.SourcePath)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dest), "export-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(out.Name())
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return "", err
	}
	if err = out.Sync(); err != nil {
		out.Close()
		return "", err
	}
	if err = out.Close(); err != nil {
		return "", err
	}
	sum, err := fileSHA256(out.Name())
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(sum, b.SHA256) {
		return "", errors.New("snapshot payload changed during export")
	}
	if err = durable.Rename(out.Name(), dest); err != nil {
		return "", err
	}
	return dest, nil
}
