// Package model holds the data Klip shares with Klip/Maccy 1.0.10 and older:
// the clipboard item and the settings, with the exact JSON that the .NET
// client wrote (property names in PascalCase, enums as integers, times as
// DateTimeOffset), so files written by either side stay readable by the
// other.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Kind is ClipboardContentKind: the order of the C# enum is the wire value.
type Kind int

const (
	KindText Kind = iota
	KindImage
	KindFileList
)

// Time is a DateTimeOffset. It keeps the offset it was read with, as .NET
// does, and writes the format System.Text.Json writes.
type Time struct{ time.Time }

// stjLayout is how System.Text.Json writes a DateTimeOffset: the fraction has
// up to seven digits, without trailing zeros, and the offset is always
// +hh:mm, also for UTC.
const stjLayout = "2006-01-02T15:04:05.9999999-07:00"

// OLayout is DateTimeOffset.ToString("O"): seven fraction digits always.
const OLayout = "2006-01-02T15:04:05.0000000-07:00"

func (t Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(FormatSTJ(t.Time))
}

func (t *Time) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := ParseTime(s)
	if err != nil {
		return err
	}
	t.Time = v
	return nil
}

// FormatSTJ formats t as System.Text.Json does. .NET counts time in 100 ns
// ticks, so finer time is cut.
func FormatSTJ(t time.Time) string {
	return forceOffset(t.Truncate(100 * time.Nanosecond)).Format(stjLayout)
}

// FormatO formats t as DateTimeOffset.ToString("O").
func FormatO(t time.Time) string {
	return forceOffset(t.Truncate(100 * time.Nanosecond)).Format(OLayout)
}

// forceOffset turns a UTC time (which Go formats with "Z") into one with a
// zero offset, so the layout writes +00:00 as .NET does.
func forceOffset(t time.Time) time.Time {
	if _, off := t.Zone(); off == 0 {
		return t.In(time.FixedZone("", 0))
	}
	return t
}

// ParseTime reads what .NET writes and a plain RFC 3339 time.
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999", "2006-01-02T15:04:05"} {
		if v, err := time.Parse(layout, s); err == nil {
			return v, nil
		}
	}
	return time.Time{}, fmt.Errorf("model: bad time %q", s)
}

// Item is ClipboardItem. The field order is the order of the C# record,
// which System.Text.Json follows when it writes.
type Item struct {
	// LocalSyncMutation is local recovery evidence, saved atomically with the
	// item in history.json. Snapshot SQL deliberately does not export it.
	LocalSyncMutation string   `json:"LocalSyncMutation,omitempty"`
	ID                string   `json:"Id"`
	Kind              Kind     `json:"Kind"`
	CapturedAt        Time     `json:"CapturedAt"`
	ApproxBytes       int64    `json:"ApproxBytes"`
	Text              *string  `json:"Text"`
	ImageFilePath     *string  `json:"ImageFilePath"`
	FilePaths         []string `json:"FilePaths"`
	Pinned            bool     `json:"Pinned"`
	CopyCount         int32    `json:"CopyCount"`
	FirstCapturedAt   *Time    `json:"FirstCapturedAt"`
	Note              *string  `json:"Note"`
	ContentHash       *string  `json:"ContentHash"`
	SourceAppName     *string  `json:"SourceAppName"`
	SourceAppPath     *string  `json:"SourceAppPath"`
}

// UnmarshalJSON gives CopyCount the default the C# record gives a missing
// one.
func (it *Item) UnmarshalJSON(b []byte) error {
	type plain Item
	p := plain{CopyCount: 1}
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*it = Item(p)
	return nil
}

// IDN is the id without hyphens, as the C# code names cache directories
// (Guid.ToString("N")).
func (it Item) IDN() string { return strings.ReplaceAll(strings.ToLower(it.ID), "-", "") }

// Str returns the string of a nullable text, "" when null.
func Str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Ptr returns a pointer to s.
func Ptr(s string) *string { return &s }
