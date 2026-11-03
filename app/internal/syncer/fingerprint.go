package syncer

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"strconv"
	"strings"
	"time"

	"klip/internal/model"
)

// ticks counts 100 ns from 0001-01-01 UTC, as DateTime.Ticks does.
func ticks(t time.Time) int64 {
	u := t.UTC()
	return (u.Unix()+62135596800)*10_000_000 + int64(u.Nanosecond()/100)
}

// fileName is Path.GetFileName: on Windows it cuts at / and \, elsewhere at
// / only.
func fileName(p string, windows bool) string {
	cut := strings.LastIndex(p, "/")
	if windows {
		if i := strings.LastIndex(p, `\`); i > cut {
			cut = i
		}
	}
	return p[cut+1:]
}

// Fingerprint is ComputeLocalFingerprint: a hash of what makes the history
// different from another one, to tell whether it changed since the last
// sync. It stays the C# text, line ends included (Environment.NewLine),
// because a device that was updated from 1.0.10 keeps the fingerprint that
// 1.0.10 stored.
func Fingerprint(items []model.Item) string {
	return fingerprint(items, runtime.GOOS == "windows")
}

func fingerprint(items []model.Item, windows bool) string {
	nl := "\n"
	if windows {
		nl = "\r\n"
	}
	var b strings.Builder
	b.WriteString("schema=1" + nl)
	for _, it := range items {
		b.WriteString(it.ID)
		b.WriteByte('|')
		b.WriteString(strconv.Itoa(int(it.Kind)))
		b.WriteByte('|')
		b.WriteString(strconv.FormatInt(ticks(it.CapturedAt.Time), 10))
		b.WriteByte('|')
		b.WriteString(strconv.FormatInt(it.ApproxBytes, 10))
		b.WriteByte('|')
		if it.Pinned {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
		b.WriteByte('|')
		b.WriteString(strconv.Itoa(int(it.CopyCount)))
		b.WriteByte('|')
		first := int64(0)
		if it.FirstCapturedAt != nil {
			first = ticks(it.FirstCapturedAt.Time)
		}
		b.WriteString(strconv.FormatInt(first, 10))
		b.WriteByte('|')
		b.WriteString(model.Str(it.Note))
		b.WriteByte('|')
		b.WriteString(model.Str(it.ContentHash))
		b.WriteByte('|')
		b.WriteString(model.Str(it.Text))
		b.WriteByte('|')
		if len(it.FilePaths) > 0 {
			names := make([]string, len(it.FilePaths))
			for i, p := range it.FilePaths {
				names[i] = fileName(p, windows)
			}
			b.WriteString(strings.Join(names, ","))
		}
		b.WriteString(nl)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
