package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"klip/internal/durable"
	"klip/internal/model"
	"klip/internal/syncer"
)

// keepDeleted is how long a deletion is still enforced here after the server
// has had it.
const keepDeleted = 30 * 24 * time.Hour

// pendingDeletes are the items the user deleted (or cleared) on this device,
// kept in a file, one list for each account (by its address, as the
// checkpoints are): another account's work never touches them, and a
// sign-out or a sign-in to the same account keeps them.
//
// A sync merges what the server has with the local history, which would bring
// such an item back; so a merge or a download leaves them out (see
// syncer.Tombstone for which items that is), and when it has left one out the
// result is uploaded; that an upload is owed for it is written down too, so
// that a restart before the upload does not forget it. A deletion counts as
// the server's once an upload went through without the item (only what that
// upload left out: a deletion asked for meanwhile is not), and is enforced
// for keepDeleted after that: a device that was offline with the item, and
// uploads its history merged with the server's, brings it back; this device
// takes it out again at its next sync.
//
// That is the limit of it: the marks are on this device only. The snapshot,
// which other devices and the C# client read and write as it is, has no place
// for them, so when this device does not sync again after another has
// brought the item back, the item stays.
type pendingDeletes struct {
	path   string
	logf   func(string, ...any)
	now    func() time.Time
	mu     sync.Mutex
	lists  map[string][]deletion // by account (lower case)
	repair map[string]bool       // accounts whose server still has deleted items
	alias  map[string]string     // an account known by another address since (Adopt)
	read   bool                  // the file has been read (or there is none)
	// unsaved says that the last save failed: the file lacks what memory has.
	unsaved bool
	// readFile reads the file (os.ReadFile when nil).
	readFile func(string) ([]byte, error)
	failed   string // why it could not be read, as last logged
}

// deletion is one item the user deleted.
type deletion struct {
	ID      string     `json:"id"`
	Key     string     `json:"key,omitempty"`   // syncer.MergeKey
	Spare   []string   `json:"spare,omitempty"` // kept items of the same content
	At      time.Time  `json:"at"`
	AckedAt *time.Time `json:"acked_at,omitempty"` // when the server had it
}

// expired says that d has been enforced long enough after the server had it.
func (d deletion) expired(cut time.Time) bool { return d.AckedAt != nil && d.AckedAt.Before(cut) }

func (d deletion) tombstone() syncer.Tombstone {
	return syncer.Tombstone{ID: d.ID, Key: d.Key, At: d.At, Spare: d.Spare}
}

// pendingFile is what the file holds.
type pendingFile struct {
	Accounts map[string][]deletion `json:"accounts"`
	Repair   map[string]bool       `json:"repair,omitempty"`
	Aliases  map[string]string     `json:"aliases,omitempty"` // sessions known by another name since (Adopt)
}

func accountKey(owner string) string { return strings.ToLower(strings.TrimSpace(owner)) }

func (p *pendingDeletes) account(owner string) string {
	k := accountKey(owner)
	for range len(p.alias) + 1 { // a chain: a session, another, an address
		to, ok := p.alias[k]
		if !ok || to == k {
			break
		}
		k = to
	}
	return k
}

func (p *pendingDeletes) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// load reads the file once. When it cannot be read (a disk error, a
// permission) what is noted meanwhile is kept in memory, the file is not
// written over (save), and the next use reads it again; what it holds then
// joins what was noted. A file that is read but is not a list of deletions
// (damaged by something else: this program replaces it whole) is kept beside
// it as ".bad", for whoever wants to look at it, and the deletions start
// afresh: what it held is lost either way, and what is deleted from now on is
// not.
func (p *pendingDeletes) load() error {
	if p.read {
		return nil
	}
	if p.lists == nil {
		p.lists, p.repair = map[string][]deletion{}, map[string]bool{}
	}
	readFile := p.readFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	b, err := readFile(p.path)
	if errors.Is(err, fs.ErrNotExist) {
		p.read, p.failed = true, ""
		return nil
	}
	if err != nil {
		return p.unread(err)
	}
	var f pendingFile
	if err := json.Unmarshal(b, &f); err != nil {
		if rerr := os.Rename(p.path, p.path+".bad"); rerr != nil {
			return p.unread(fmt.Errorf("%v; keeping it aside: %w", err, rerr))
		}
		p.log("pending deletions: %v; the file is kept as %s.bad", err, filepath.Base(p.path))
		p.read, p.failed = true, ""
		return nil
	}
	for k, v := range f.Aliases {
		if p.alias == nil {
			p.alias = map[string]string{}
		}
		if _, ok := p.alias[accountKey(k)]; !ok { // one made since is newer
			p.alias[accountKey(k)] = accountKey(v)
		}
	}
	// What was noted while the file could not be read, under a name the file
	// says is another's now, is that one's.
	for _, k := range slices.Collect(maps.Keys(p.lists)) {
		p.move(k, p.account(k))
	}
	for k, on := range p.repair {
		if a := p.account(k); on && a != k {
			p.repair[a] = true
			delete(p.repair, k)
		}
	}
	for k, l := range f.Accounts {
		a := p.account(k) // an account adopted while the file could not be read
		p.lists[a] = append(p.lists[a], l...)
	}
	for k, on := range f.Repair {
		if on {
			p.repair[p.account(k)] = true
		}
	}
	p.read, p.failed = true, ""
	return nil
}

// unread notes that the file could not be read, logging it once for each
// reason.
func (p *pendingDeletes) unread(err error) error {
	if msg := err.Error(); msg != p.failed {
		p.failed = msg
		p.log("pending deletions: %v", err)
	}
	return err
}

func (p *pendingDeletes) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// save writes the lists to a file beside it and puts that in its place, so
// that a crash or a full disk leaves the old lists, not a broken file. When it
// fails, what is in memory is ahead of the file (unsaved) until a save goes
// through.
func (p *pendingDeletes) save() error {
	err := p.write()
	p.unsaved = err != nil
	return err
}

func (p *pendingDeletes) write() error {
	if p.path == "" {
		return nil
	}
	if !p.read {
		return fmt.Errorf("not written: the file could not be read (%s)", p.failed)
	}
	for k, l := range p.lists {
		if len(l) == 0 {
			delete(p.lists, k)
		}
	}
	for k, on := range p.repair {
		if !on || len(p.lists[k]) == 0 {
			delete(p.repair, k)
		}
	}
	if len(p.lists) == 0 {
		if err := os.Remove(p.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	for _, l := range p.lists {
		sort.SliceStable(l, func(i, j int) bool { return l[i].At.Before(l[j].At) })
	}
	b, err := json.Marshal(pendingFile{Accounts: p.lists, Repair: p.repair, Aliases: p.keptAliases()})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := durable.SyncTree(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := durable.Rename(tmp, p.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Add notes that the user of the account owner deleted items, now (or at the
// time an item says it was captured, when that is later). history is the
// history as it is when they are deleted: the items in it of the same content
// that stay are not deleted with them (see syncer.Tombstone). When the file
// cannot be written the items are still noted for as long as the program
// runs, and the error says so.
func (p *pendingDeletes) Add(owner string, history []model.Item, items ...model.Item) error {
	if len(items) == 0 {
		return nil
	}
	gone := map[string]bool{}
	for _, it := range items {
		gone[strings.ToLower(it.ID)] = true
	}
	kept := map[string][]string{} // merge key: the ids that stay
	for _, it := range history {
		if k := syncer.MergeKey(it); k != "" && !gone[strings.ToLower(it.ID)] {
			kept[k] = append(kept[k], it.ID)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load()
	k, at := p.account(owner), p.clock()
	l := p.lists[k]
	for _, it := range items {
		// The deletion covers the item as it was, also when it says it was
		// captured later than now (a device whose clock is ahead, or a clock
		// set back here): the time is not what the deletion is about.
		key := syncer.MergeKey(it)
		d := deletion{ID: it.ID, Key: key, Spare: kept[key], At: at}
		if it.CapturedAt.After(d.At) {
			d.At = it.CapturedAt.Time
		}
		l = append(l, d)
	}
	p.lists[k] = l
	return p.save()
}

// Tombstones are the deletions of the account owner that are still enforced:
// those the server has not had, and those it has had for less than
// keepDeleted.
func (p *pendingDeletes) Tombstones(owner string) []syncer.Tombstone {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load()
	cut := p.clock().Add(-keepDeleted)
	var out []syncer.Tombstone
	for _, d := range p.lists[p.account(owner)] {
		if d.expired(cut) {
			continue // enforced long enough; Prune drops it
		}
		out = append(out, d.tombstone())
	}
	return out
}

// Owed says whether the account owner has deletions the server has not had.
func (p *pendingDeletes) Owed(owner string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load()
	for _, d := range p.lists[p.account(owner)] {
		if d.AckedAt == nil {
			return true
		}
	}
	return false
}

// NeedsRepair says that the server of the account owner still has items this
// device deleted (a merge or a download left them out here; see MarkRepair).
func (p *pendingDeletes) NeedsRepair(owner string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load()
	return p.repair[p.account(owner)]
}

// MarkRepair writes down that the server of the account owner still has
// items this device deleted, until an upload went through (Delivered).
func (p *pendingDeletes) MarkRepair(owner string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load()
	k := p.account(owner)
	if p.repair[k] && !p.unsaved {
		return nil // written down already
	}
	p.repair[k] = true
	return p.save()
}

// Delivered is for an upload of the account owner that went through: the
// server now has items, and the deletions of what is not among them are the
// server's (a deletion whose item the upload still had, as one asked for
// while it was on its way, is not). The server needs no repair any more.
func (p *pendingDeletes) Delivered(owner string, items []model.Item) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load()
	k := p.account(owner)
	// The items the server has, by id and by merge key.
	byID, byKey := map[string][]model.Item{}, map[string][]model.Item{}
	for _, it := range items {
		byID[strings.ToLower(it.ID)] = append(byID[strings.ToLower(it.ID)], it)
		if mk := syncer.MergeKey(it); mk != "" {
			byKey[mk] = append(byKey[mk], it)
		}
	}
	has := func(d deletion) bool {
		t := d.tombstone()
		for _, it := range append(byID[strings.ToLower(d.ID)], byKey[strings.ToLower(d.Key)]...) {
			if t.Covers(it) {
				return true
			}
		}
		return false
	}
	now := p.clock()
	changed := p.repair[k]
	delete(p.repair, k)
	for i, d := range p.lists[k] {
		if d.AckedAt != nil || has(d) {
			continue
		}
		at := now
		p.lists[k][i].AckedAt, changed = &at, true
	}
	if p.prune(k) || changed || p.unsaved {
		return p.save()
	}
	return nil
}

// Prune drops the deletions of the account owner that have been enforced
// long enough.
func (p *pendingDeletes) Prune(owner string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load()
	if p.prune(p.account(owner)) {
		return p.save()
	}
	return nil
}

func (p *pendingDeletes) prune(k string) bool {
	cut := p.clock().Add(-keepDeleted)
	l := p.lists[k]
	kept := make([]deletion, 0, len(l))
	for _, d := range l {
		if !d.expired(cut) {
			kept = append(kept, d)
		}
	}
	if len(kept) == len(l) {
		return false
	}
	p.lists[k] = kept
	return true
}

// Adopt is for a login that learned its address, or got a new session (a
// refresh): what it noted under the name it had (from) is the account to's.
// Work of that login that still names from counts as to's.
//
// The settings that name to are saved after this (ownerLearned runs before),
// and that may fail: the program then starts again with the name from. So
// that it still finds what it noted, the file keeps, with the lists, which
// session is known by which name since (a session's name is its own: no
// other login has it); an address the login had before it was signed in
// properly ("") is nobody's in particular, and is forgotten with the login
// (ForgetAliases).
//
// When the file cannot be written the change is made in memory, and the
// error says so.
func (p *pendingDeletes) Adopt(from, to string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	lerr := p.load()
	if !p.adopt(from, to) {
		return lerr
	}
	if lerr != nil {
		return lerr // what the file holds under from is not moved yet
	}
	return p.save()
}

// Move is Adopt made again after the settings were saved but the file was
// not written (see ownerMove): the file is written also when nothing is left
// to move, as the move Adopt made in memory may be what it lacks.
func (p *pendingDeletes) Move(from, to string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(); err != nil {
		return err
	}
	p.adopt(from, to)
	return p.save()
}

// adopt is Adopt in memory. The chain of names always ends at the account:
// what from is known by now (a session renamed before, or the address it
// learned) is followed first; when that is an address and to is a session
// (a login whose address the settings did not keep gets a new session), the
// session joins the address, not the other way round.
func (p *pendingDeletes) adopt(from, to string) bool {
	f0, t := accountKey(from), accountKey(to)
	f := p.account(f0)
	if f0 == t || f == t {
		return false
	}
	if p.alias == nil {
		p.alias = map[string]string{}
	}
	if !sessionOwner(f) { // known by its address already
		if sessionOwner(t) {
			p.alias[t] = f
		} else {
			p.alias[f0] = t // another address: what it noted under its own stays
		}
		return true
	}
	p.alias[f0], p.alias[f] = t, t
	for k, v := range p.alias { // what named from names to now
		if v == f0 || v == f {
			p.alias[k] = t
		}
	}
	p.move(f, t)
	return true
}

// move puts the list and the repair of the account f under t's.
func (p *pendingDeletes) move(f, t string) bool {
	if f == t || len(p.lists[f]) == 0 {
		return false
	}
	p.lists[t] = append(p.lists[t], p.lists[f]...)
	delete(p.lists, f)
	if p.repair[f] {
		p.repair[t] = true
		delete(p.repair, f)
	}
	return true
}

// ForgetAliases is for a change of login: the address a login had before it
// was signed in properly is nobody's in particular any more. The names of
// sessions are kept: they are that login's alone.
func (p *pendingDeletes) ForgetAliases() {
	p.mu.Lock()
	for k := range p.alias {
		if !strings.HasPrefix(k, sessionPrefix) {
			delete(p.alias, k)
		}
	}
	p.mu.Unlock()
}

// keptAliases are the names of sessions to write down: those that lead to a
// list.
func (p *pendingDeletes) keptAliases() map[string]string {
	var out map[string]string
	for k, v := range p.alias {
		if strings.HasPrefix(k, sessionPrefix) && len(p.lists[p.account(k)]) > 0 {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

// Ready prevents synchronization from treating an unreadable deletion file as empty.
func (p *pendingDeletes) Ready() error { p.mu.Lock(); defer p.mu.Unlock(); return p.load() }
