package syncclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Event is one server-sent event.
type Event struct {
	Name string
	Data string
}

// ErrUnauthorized is returned by Listen when the server refuses the access
// token: refresh it and listen again.
var ErrUnauthorized = errors.New("sync events unauthorized")

// Listen opens the event stream and calls onEvent for each event until the
// stream ends (nil) or fails. The stream is long-lived: no timeout, only ctx
// ends it. The parsing is that of SyncEventStreamService: lines starting
// with ":" are comments, "event:" names the event, "data:" lines are joined
// with newlines, and an empty line sends the event.
func (c *Client) Listen(ctx context.Context, accessToken string, onEvent func(Event)) error {
	if c.baseURL == "" {
		return ErrNoBaseURL
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/sync/events", accessToken, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errors.New("sync events failed: " + http.StatusText(resp.StatusCode))
	}
	return parseEvents(resp.Body, onEvent)
}

func parseEvents(r io.Reader, onEvent func(Event)) error {
	br := bufio.NewReader(r)
	var name string
	var data strings.Builder
	for {
		line, err := br.ReadString('\n')
		if line == "" && err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			onEvent(Event{Name: name, Data: data.String()})
			name = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimLeft(line[len("data:"):], " \t"))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// TokenSource gives Watch the access token to listen with.
type TokenSource interface {
	// AccessToken returns a token that is still valid, refreshing it when it
	// is about to expire. "" means there is none now.
	AccessToken(ctx context.Context) (string, error)
	// Refresh renews the tokens after the server refused one. It says
	// whether it has new ones.
	Refresh(ctx context.Context) (bool, error)
}

// WatchOptions set the timing of Watch. The zero value is the C# client's.
type WatchOptions struct {
	InitialDelay time.Duration // before reconnecting, 2 s
	MaxDelay     time.Duration // the delay doubles up to this, 15 s
	Debounce     time.Duration // events within this are one, 250 ms
}

func (o WatchOptions) withDefaults() WatchOptions {
	if o.InitialDelay <= 0 {
		o.InitialDelay = 2 * time.Second
	}
	if o.MaxDelay <= 0 {
		o.MaxDelay = 15 * time.Second
	}
	if o.Debounce <= 0 {
		o.Debounce = 250 * time.Millisecond
	}
	return o
}

// Watch keeps the event stream open, reconnecting after a failure with a
// growing delay and refreshing the tokens when the server refuses them, and
// calls onUpdated (on its own goroutine) when another device uploaded. It
// returns when ctx is done.
func (c *Client) Watch(ctx context.Context, ts TokenSource, opts WatchOptions, onUpdated func()) {
	opts = opts.withDefaults()
	var mu sync.Mutex
	running, pending := false, false
	dispatch := func(e Event) {
		// A connection is also a reason to look again: events published while
		// this watcher was disconnected are not replayed by the server.
		if e.Name != "sync-updated" && e.Name != "connected" {
			return
		}
		mu.Lock()
		pending = true
		if running {
			mu.Unlock()
			return
		}
		running = true
		mu.Unlock()
		go func() {
			for {
				select {
				case <-time.After(opts.Debounce):
				case <-ctx.Done():
					mu.Lock()
					running = false
					mu.Unlock()
					return
				}
				mu.Lock()
				pending = false
				mu.Unlock()
				onUpdated()
				mu.Lock()
				if !pending {
					running = false
					mu.Unlock()
					return
				}
				mu.Unlock()
			}
		}()
	}

	delay := opts.InitialDelay
	wait := func(d time.Duration) bool {
		select {
		case <-time.After(d):
			return true
		case <-ctx.Done():
			return false
		}
	}
	for ctx.Err() == nil {
		started := time.Now()
		token, err := ts.AccessToken(ctx)
		if err != nil || strings.TrimSpace(token) == "" {
			if !wait(delay) {
				return
			}
			continue
		}
		err = c.Listen(ctx, token, dispatch)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, ErrUnauthorized):
			if ok, _ := ts.Refresh(ctx); ok {
				delay = opts.InitialDelay
				continue
			}
		case err == nil:
			// A clean EOF. A stream that lasted is a healthy connection: go
			// back at once. One that closed at once is a server or proxy
			// that keeps saying 200 and hanging up: back off like a failure.
			if time.Since(started) >= 30*time.Second {
				delay = opts.InitialDelay
				continue
			}
			if !wait(delay) {
				return
			}
			delay = min(delay*2, opts.MaxDelay)
			continue
		}
		if !wait(delay) {
			return
		}
		delay = min(delay*2, opts.MaxDelay)
	}
}
