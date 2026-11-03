package settings

import (
	"sync"
	"sync/atomic"
	"time"

	"klip/internal/model"
)

// Manager is AppSettingsService: the settings in memory, shared by every part
// of the program, written to settings.json after each update.
type Manager struct {
	path string

	mu       sync.Mutex
	cur      model.Settings
	handlers []func(model.Settings)
	watchers []func(before model.Settings, after *model.Settings)

	// Handlers run one after the other on one goroutine (see Update).
	rev       int64         // number of the newest update, under mu
	delivered atomic.Int64  // the newest one the handlers have seen
	signal    chan struct{} // an update is waiting to be delivered
	start     sync.Once
	inHandler atomic.Bool
}

// Open reads the settings. The Manager is usable also when err says the file
// could not be read: it holds the defaults then, as the C# client does.
func Open(path string) (*Manager, error) {
	s, err := Load(path)
	return &Manager{path: path, cur: s}, err
}

// Get returns a copy of the settings.
func (m *Manager) Get() model.Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

// deliverWait is how long Update waits for the handlers to have run: they
// are meant to have run when Update returns, but one of them may be waiting
// for the thread that called Update, so the wait has an end.
const deliverWait = 300 * time.Millisecond

// Update changes the settings and saves them. Functions registered with
// OnChange then run with the new settings, one after another in the order of
// the updates (a single goroutine delivers them, so a slow handler of one
// update can never be overtaken by the handler of a later one). Update returns
// once they have run, or after a short wait if they are slow. A failure to
// save is returned, the change stays in memory.
func (m *Manager) Update(f func(*model.Settings)) error {
	m.start.Do(func() {
		m.signal = make(chan struct{}, 1)
		go m.deliver()
	})
	m.mu.Lock()
	before := m.cur
	f(&m.cur)
	for _, w := range m.watchers {
		w(before, &m.cur)
	}
	err := Save(m.path, m.cur)
	m.rev++
	rev := m.rev
	m.mu.Unlock()
	select {
	case m.signal <- struct{}{}:
	default: // one is pending already: it will carry this update too
	}
	if !m.inHandler.Load() { // a handler's own Update must not wait for itself
		deadline := time.Now().Add(deliverWait)
		for m.delivered.Load() < rev && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	return err
}

// deliver runs the handlers for every update, always with the newest
// settings, until the program ends.
func (m *Manager) deliver() {
	for range m.signal {
		m.mu.Lock()
		rev, cur := m.rev, m.cur
		hs := make([]func(model.Settings), len(m.handlers))
		copy(hs, m.handlers)
		m.mu.Unlock()
		m.inHandler.Store(true)
		for _, h := range hs {
			h(cur)
		}
		m.inHandler.Store(false)
		m.delivered.Store(rev)
	}
}

// Watch registers a function that sees each Update as it is made, before
// and after, in the order of the updates, before they are saved: what it
// adds to after is saved with the change. It runs inside Update with the
// settings locked: it must be quick and must not use the Manager. after
// shares its maps with before and with what Get gave out: a map is replaced,
// not changed.
func (m *Manager) Watch(f func(before model.Settings, after *model.Settings)) {
	m.mu.Lock()
	m.watchers = append(m.watchers, f)
	m.mu.Unlock()
}

// OnChange registers a function to run after each Update.
func (m *Manager) OnChange(f func(model.Settings)) {
	m.mu.Lock()
	m.handlers = append(m.handlers, f)
	m.mu.Unlock()
}
