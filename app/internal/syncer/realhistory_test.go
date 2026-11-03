package syncer_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"sync"
	"testing"
	"time"

	"klip/internal/capture"
	"klip/internal/history"
	"klip/internal/model"
	"klip/internal/nastest"
	"klip/internal/paths"
	"klip/internal/syncclient"
	"klip/internal/syncer"
)

type memClip struct {
	formats []string
	text    string
	image   []byte
}

func (c *memClip) Formats() []string            { return c.formats }
func (c *memClip) ReadText() string             { return c.text }
func (c *memClip) ReadFiles() ([]string, error) { return nil, nil }
func (c *memClip) ReadImage() []byte            { return c.image }
func (c *memClip) WriteText(string) error       { return nil }
func (c *memClip) WriteFiles([]string) error    { return nil }
func (c *memClip) WriteImage([]byte, string) error {
	return nil
}

// realDevice is a device with the real history service, persistence and
// capture, not the fakes of the other tests.
type realDevice struct {
	layout paths.Layout
	hist   *history.Service
	pers   *history.Persister
	cap    *capture.Service
	clip   *memClip
	s      *syncer.Syncer
	mu     sync.Mutex
	st     model.Settings
}

func newRealDevice(t *testing.T, srv *nastest.Server, tok syncclient.TokenSet) *realDevice {
	d := &realDevice{layout: paths.Layout{Root: t.TempDir()}, clip: &memClip{}, st: model.DefaultSettings()}
	d.hist = history.NewService(d.layout)
	d.pers = history.NewPersister(d.hist, d.layout)
	d.pers.Delay = time.Hour // the test saves by hand
	d.cap = capture.New(d.layout, d.hist, d.clip)
	d.st.AuthAccessToken, d.st.AuthRefreshToken = &tok.AccessToken, &tok.RefreshToken
	d.st.AuthExpiresAtUnixMs = tok.ExpiresAt.UnixMilli()
	d.s = syncer.New(syncer.Config{
		Layout: d.layout,
		Settings: syncclient.Settings{
			Get:    func() model.Settings { d.mu.Lock(); defer d.mu.Unlock(); return d.st },
			Update: func(f func(*model.Settings)) error { d.mu.Lock(); f(&d.st); d.mu.Unlock(); return nil },
		},
		History: d.hist, Flush: d.pers.Flush, Client: syncclient.New(srv.URL), BaseURL: srv.URL,
	})
	return d
}

func (d *realDevice) copyText(t *testing.T, text string) {
	d.clip.formats, d.clip.text, d.clip.image = []string{"text/plain"}, text, nil
	if err := d.cap.Capture(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Capture on one device, sync, and the other device has the same history,
// persisted in history.json and loadable by a new start.
func TestCaptureSyncPersistLoad(t *testing.T) {
	srv := nastest.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srv.Subscribe(ctx, "owner@example.com", "password1")
	login := func() syncclient.TokenSet {
		tk, err := syncclient.New(srv.URL).Login(ctx, "owner@example.com", "password1")
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	a, b := newRealDevice(t, srv, login()), newRealDevice(t, srv, login())

	a.copyText(t, "first copy")
	time.Sleep(300 * time.Millisecond) // the capture's own pause is not at issue here
	a.copyText(t, "second copy")
	var pic bytes.Buffer
	png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 5, 5)))
	a.clip.formats, a.clip.text, a.clip.image = []string{"image/png"}, "", pic.Bytes()
	if err := a.cap.Capture(ctx); err != nil {
		t.Fatal(err)
	}
	a.hist.TogglePinned(a.hist.Items()[1].ID)
	a.hist.UpdateNote(a.hist.Items()[2].ID, "a note")

	if res, err := a.s.Run(ctx, nil, "t"); err != nil || res.Decision.Direction != syncer.Upload {
		t.Fatalf("A: %+v %v", res.Decision, err)
	}
	res, err := b.s.Run(ctx, nil, "t")
	if err != nil || res.Decision.Direction != syncer.Download {
		t.Fatalf("B: %+v %v", res.Decision, err)
	}

	ai, bi := a.hist.Items(), b.hist.Items()
	if len(bi) != 3 || len(ai) != 3 {
		t.Fatalf("A has %d items, B %d", len(ai), len(bi))
	}
	for i := range ai {
		if ai[i].ID != bi[i].ID || ai[i].Kind != bi[i].Kind || ai[i].Pinned != bi[i].Pinned || model.Str(ai[i].Note) != model.Str(bi[i].Note) ||
			model.Str(ai[i].Text) != model.Str(bi[i].Text) || model.Str(ai[i].ContentHash) != model.Str(bi[i].ContentHash) {
			t.Errorf("item %d differs:\n A %+v\n B %+v", i, ai[i], bi[i])
		}
	}
	if bi[0].Kind != model.KindImage || !bi[1].Pinned || model.Str(bi[2].Note) != "a note" {
		t.Errorf("B's items: %+v", bi)
	}

	// The sync flushed B's history to disk: a new start finds it, and sees no
	// change to sync.
	svc := history.NewService(b.layout)
	p := history.NewPersister(svc, b.layout)
	p.Delay = time.Hour
	if err := p.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if svc.Len() != 3 {
		t.Fatalf("a restart loads %d items", svc.Len())
	}
	if got := syncer.Fingerprint(svc.Items()); got != b.s.Fingerprint() {
		t.Error("the fingerprint changes over a restart: the next sync would think the history changed")
	}
	if res, err := b.s.Run(ctx, nil, "t"); err != nil || res.Decision.Direction != syncer.None {
		t.Errorf("B again: %+v %v", res.Decision, err)
	}
}
