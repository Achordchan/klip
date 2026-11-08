package syncclient_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"klip/internal/nastest"
	"klip/internal/syncclient"
)

// These tests run the real server with blobs collected at once and backups
// that expire when the next snapshot is uploaded (nastest.FastBlobGC), and
// check which blobs it keeps: the ones a kept snapshot or a pending
// declaration refers to, and every one while a snapshot of unknown
// references is kept.

type gcAccount struct {
	t      *testing.T
	srv    *nastest.Server
	ctx    context.Context
	c      *syncclient.Client
	access string
	dir    string
	blobs  map[string]string // sha -> file
}

func newGCAccount(t *testing.T, email string, opts ...nastest.Option) *gcAccount {
	srv := nastest.Start(t, append([]nastest.Option{nastest.FastBlobGC()}, opts...)...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	tok := srv.Subscribe(ctx, email, "password1")
	return &gcAccount{t: t, srv: srv, ctx: ctx, c: syncclient.New(srv.URL), access: tok.AccessToken, dir: t.TempDir(), blobs: map[string]string{}}
}

// blob makes a file of n random bytes and returns its hash.
func (a *gcAccount) blob(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	h := sha(b)
	p := filepath.Join(a.dir, h)
	os.WriteFile(p, b, 0o644)
	a.blobs[h] = p
	return h
}

// check declares the hashes, uploads the missing ones and returns the token.
func (a *gcAccount) check(hashes ...string) string {
	a.t.Helper()
	res, err := a.c.CheckBlobs(a.ctx, a.access, hashes)
	if err != nil || res.RefsToken == "" {
		a.t.Fatalf("check %v: %+v %v", hashes, res, err)
	}
	for _, h := range res.Missing {
		if err := a.c.UploadBlob(a.ctx, a.access, h, a.blobs[h]); err != nil {
			a.t.Fatal(err)
		}
	}
	return res.RefsToken
}

// snapshot uploads a new snapshot (its content does not matter to the
// server) with refs.
func (a *gcAccount) snapshot(refs string) {
	a.t.Helper()
	if err := a.trySnapshot(refs); err != nil {
		a.t.Fatalf("snapshot with refs %q: %v", refs, err)
	}
}

func (a *gcAccount) trySnapshot(refs string) error {
	b := make([]byte, 512)
	rand.Read(b)
	p := filepath.Join(a.dir, "snapshot.sqlite")
	os.WriteFile(p, b, 0o644)
	_, err := a.c.UploadSnapshot(a.ctx, a.access, p, refs)
	return err
}

// has says whether the server serves the blob. It does not ask with a check:
// a check declares the hashes, and would keep them.
func (a *gcAccount) has(h string) bool {
	a.t.Helper()
	err := a.c.DownloadBlob(a.ctx, a.access, h, filepath.Join(a.dir, "download"))
	if err != nil && !syncclient.IsStatus(err, 404) {
		a.t.Fatal(err)
	}
	return err == nil
}

func (a *gcAccount) usage() int64 {
	a.t.Helper()
	st, err := a.c.SubscriptionStatus(a.ctx, a.access)
	if err != nil || st.StorageBytes == nil {
		a.t.Fatalf("status: %+v %v", st, err)
	}
	return *st.StorageBytes
}

// expectGone waits until the server no longer serves gone, and checks that it
// still serves kept.
func (a *gcAccount) expectGone(gone string, kept ...string) {
	a.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for a.has(gone) {
		if time.Now().After(deadline) {
			a.t.Fatalf("blob %s is still on the server", gone[:8])
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, h := range kept {
		if !a.has(h) {
			a.t.Fatalf("blob %s is still referred to, and was dropped", h[:8])
		}
	}
}

func TestBlobsNoSnapshotRefersToAreCollected(t *testing.T) {
	a := newGCAccount(t, "collect@example.com")
	x, y := a.blob(300_000), a.blob(20_000)
	a.snapshot(a.check(x, y))
	full := a.usage()

	// The next snapshot drops x. The one before becomes a backup, which
	// expires at once on this server: then nothing refers to x.
	a.snapshot(a.check(y))
	a.expectGone(x, y)
	if u := a.usage(); u > full-300_000 {
		t.Errorf("usage %d -> %d: x is still counted", full, u)
	}

	// A snapshot with no blob says so, and y goes too.
	a.snapshot(syncclient.NoBlobRefs)
	a.expectGone(y)

	// A blob can come back after it was collected.
	if m, err := a.c.CheckMissingBlobs(a.ctx, a.access, []string{x}); err != nil || len(m) != 1 {
		t.Fatalf("x is missing: %v %v", m, err)
	}
	a.snapshot(a.check(x))
	if !a.has(x) || !bytes.Equal(mustRead(t, a.blobs[x]), mustRead(t, filepath.Join(a.dir, "download"))) {
		t.Error("x was not uploaded again")
	}
}

// While the account keeps a snapshot uploaded without references (an older
// client), no blob is collected; once that snapshot is gone, collection
// resumes.
func TestASnapshotOfUnknownReferencesKeepsEveryBlob(t *testing.T) {
	a := newGCAccount(t, "legacy@example.com")
	x := a.blob(50_000)
	a.snapshot(a.check(x))
	a.snapshot("") // an older client: says nothing. The snapshot with x expires.
	time.Sleep(time.Second)
	if !a.has(x) {
		t.Fatal("x was collected while a snapshot of unknown references was kept")
	}
	a.snapshot(syncclient.NoBlobRefs) // the snapshot without references expires
	a.expectGone(x)

	// A collection does not stop an older client: its check declared what it
	// found, which a collection keeps.
	a.snapshot("")
}

// A check made before blobs were collected cannot vouch for what it found:
// the upload is refused, and a new check works.
func TestACheckFromBeforeACollectionIsRefused(t *testing.T) {
	a := newGCAccount(t, "stale@example.com")
	x, w := a.blob(5000), a.blob(5000)
	a.snapshot(a.check(x, w))
	old := a.check(x)
	a.snapshot(a.check(x)) // the snapshot with w expires, and w is collected
	a.expectGone(w, x)
	if err := a.trySnapshot(old); !syncclientIs(err, 409, "storage_generation_stale") {
		t.Fatalf("a check from before the collection: %v", err)
	}
	a.snapshot(a.check(x))
}

// The admin clears the account's cloud data: everything goes, a check made
// before is refused, and the account fills up again from the devices.
func TestResetStorage(t *testing.T) {
	a := newGCAccount(t, "reset@example.com")
	x := a.blob(30_000)
	a.snapshot(a.check(x))
	old := a.check(x)
	if r := a.srv.Admin("/admin/user/reset-storage", map[string]any{"emailOrUserId": "reset@example.com"}); r["success"] != true {
		t.Fatalf("reset: %v", r)
	}
	if _, err := a.c.GetManifestJSON(a.ctx, a.access); !syncclient.IsStatus(err, 404) {
		t.Errorf("manifest after the reset: %v", err)
	}
	a.expectGone(x)
	if u := a.usage(); u != 0 {
		t.Errorf("usage after the reset: %d", u)
	}
	// The device checked before the reset: x was there. Its upload is refused,
	// and so is one that says nothing.
	if err := a.trySnapshot(old); !syncclientIs(err, 409, "storage_generation_stale") {
		t.Errorf("a check from before the reset: %v", err)
	}
	if err := a.trySnapshot(""); !syncclientIs(err, 409, "storage_generation_stale") || !syncclient.IsBlobRefsRejected(err) {
		t.Errorf("an older client with no check since the reset: %v", err)
	}
	refs := a.check(x) // x is missing now, and uploaded again
	a.snapshot("")     // once a device has checked since the reset, an older client is let through
	a.snapshot(refs)
	if !a.has(x) || a.usage() == 0 {
		t.Error("the account did not fill up again")
	}
	a.snapshot(syncclient.NoBlobRefs) // a snapshot without blobs is always fine
}

// Device B has checked its blobs (and found x on the server) and is about to
// upload its snapshot; meanwhile device A uploads one without x. B's
// declaration keeps x, so B's snapshot does not end up without its blob.
func TestADeclarationKeepsItsBlobsUntilTheSnapshotIsUploaded(t *testing.T) {
	a := newGCAccount(t, "race@example.com")
	x := a.blob(40_000)
	a.snapshot(a.check(x))

	z := a.blob(1000)
	bRefs := a.check(x, z)            // B: x is there, z is uploaded
	a.snapshot(syncclient.NoBlobRefs) // A: the snapshot with x expires
	time.Sleep(time.Second)
	if !a.has(x) {
		t.Fatal("x was collected while B's declaration refers to it")
	}
	a.snapshot(bRefs) // B
	if !a.has(x) {
		t.Fatal("B's snapshot refers to x, which must still be there")
	}

	// B's snapshot took over the declaration: once it is gone, so is x.
	a.snapshot(syncclient.NoBlobRefs)
	a.expectGone(x)
	a.expectGone(z)
}

func TestBadDeclarationsAreRefused(t *testing.T) {
	a := newGCAccount(t, "refused@example.com")
	if err := a.trySnapshot("0123456789abcdef0123456789abcdef"); !syncclient.IsBlobRefsRejected(err) || !syncclientIs(err, 409, "blob_refs_expired") {
		t.Errorf("an unknown token: %v", err)
	}
	w := a.blob(1000)
	res, err := a.c.CheckBlobs(a.ctx, a.access, []string{w})
	if err != nil || len(res.Missing) != 1 || res.RefsToken == "" {
		t.Fatalf("check: %+v %v", res, err)
	}
	if err := a.trySnapshot(res.RefsToken); !syncclient.IsBlobRefsRejected(err) || !syncclientIs(err, 409, "blob_refs_missing") {
		t.Errorf("a snapshot whose blob was not uploaded: %v", err)
	}
	if err := a.c.UploadBlob(a.ctx, a.access, w, a.blobs[w]); err != nil {
		t.Fatal(err)
	}
	if err := a.trySnapshot(res.RefsToken); err != nil {
		t.Errorf("once the blob is there: %v", err)
	}
	// A declaration is used once.
	if err := a.trySnapshot(res.RefsToken); !syncclientIs(err, 409, "blob_refs_expired") {
		t.Errorf("a used token: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A long list is checked in batches on the server; the declaration covers all
// of them.
func TestALongDeclarationCoversEveryBatch(t *testing.T) {
	a := newGCAccount(t, "long@example.com")
	x, y := a.blob(5000), a.blob(5000)
	a.snapshot(a.check(x, y))

	// y first and x last, so they fall in different batches; between them,
	// hashes the server has never seen.
	hashes := []string{y}
	for i := 0; i < 2100; i++ {
		hashes = append(hashes, sha([]byte{byte(i), byte(i >> 8), 'n'}))
	}
	hashes = append(hashes, x)
	res, err := a.c.CheckBlobs(a.ctx, a.access, hashes)
	if err != nil || res.RefsToken == "" || len(res.Missing) != 2100 {
		t.Fatalf("check: %d missing, token %q, %v", len(res.Missing), res.RefsToken, err)
	}
	a.snapshot(syncclient.NoBlobRefs) // the snapshot with x and y expires
	time.Sleep(time.Second)
	if !a.has(x) || !a.has(y) {
		t.Fatal("the declaration did not keep the blobs of every batch")
	}
	if err := a.trySnapshot(res.RefsToken); !syncclientIs(err, 409, "blob_refs_missing") {
		t.Errorf("the declaration names blobs never uploaded: %v", err)
	}
}

// A list too long for a declaration still keeps the blobs it found, and its
// token still guards against a stale check; the snapshot counts as one of
// unknown references.
func TestAnOversizedCheckStillKeepsItsBlobs(t *testing.T) {
	a := newGCAccount(t, "oversized@example.com")
	x := a.blob(5000)
	a.snapshot(a.check(x))

	hashes := make([]string, 0, 60_002)
	for i := 0; i < 60_001; i++ {
		hashes = append(hashes, sha([]byte{byte(i), byte(i >> 8), byte(i >> 16), 'o'}))
	}
	hashes = append(hashes, x)
	res, err := a.c.CheckBlobs(a.ctx, a.access, hashes)
	if err != nil || res.RefsToken == "" || len(res.Missing) != 60_001 {
		t.Fatalf("check: %d missing, token %q, %v", len(res.Missing), res.RefsToken, err)
	}
	a.snapshot(syncclient.NoBlobRefs) // the snapshot with x expires
	time.Sleep(time.Second)
	if !a.has(x) {
		t.Fatal("x was collected after an oversized check found it")
	}
	// The token is accepted; the snapshot counts as one of unknown references
	// (what that means is TestASnapshotOfUnknownReferencesKeepsEveryBlob).
	a.snapshot(res.RefsToken)
}

// After a reset, an older client that checked its blobs before it is
// refused until it checks again, even when another device has checked since;
// one with only text, which never checks, is not held up.
func TestOlderClientsAfterAReset(t *testing.T) {
	a := newGCAccount(t, "older@example.com")
	x := a.blob(5000)
	a.snapshot(a.check(x))
	if _, err := a.c.CheckBlobs(a.ctx, a.access, []string{x}); err != nil { // A checks: x is there
		t.Fatal(err)
	}
	a.srv.Admin("/admin/user/reset-storage", map[string]any{"emailOrUserId": "older@example.com"})

	// Another device checks after the reset: that says nothing about A.
	other := a.srv.Login(a.ctx, "older@example.com", "password1").AccessToken
	if other == a.access {
		t.Fatal("two logins share a token")
	}
	if _, err := a.c.CheckBlobs(a.ctx, other, []string{x}); err != nil {
		t.Fatal(err)
	}
	if err := a.trySnapshot(""); !syncclientIs(err, 409, "storage_generation_stale") {
		t.Fatalf("A's upload relies on a check from before the reset: %v", err)
	}
	// A's next sync: with only text it does not check, and is let through.
	a.snapshot("")

	// The other device has only text, and never checked before the reset.
	b := &gcAccount{t: t, srv: a.srv, ctx: a.ctx, c: a.c, access: a.srv.Login(a.ctx, "older@example.com", "password1").AccessToken, dir: a.dir, blobs: a.blobs}
	a.srv.Admin("/admin/user/reset-storage", map[string]any{"emailOrUserId": "older@example.com"})
	b.snapshot("")
}

// A device may still be fetching the blobs of a snapshot that was just
// replaced and whose backup went: the server keeps them a while longer.
func TestTheBlobsOfADroppedSnapshotAreHeldForDownloads(t *testing.T) {
	a := newGCAccount(t, "hold@example.com", nastest.Var("BLOB_GC_DROP_HOLD_SECONDS", "3600"))
	x := a.blob(5000)
	a.snapshot(a.check(x))
	a.snapshot(syncclient.NoBlobRefs) // the snapshot with x expires
	time.Sleep(time.Second)
	if !a.has(x) {
		t.Fatal("x went at once, while a device may still be downloading the snapshot that had it")
	}
}

// olderToken signs a copy of token as issued an hour ago, before the
// account's server started its record of checks, as a token of the previous
// server version has neither iat_ms nor a record.
func olderToken(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	json.Unmarshal(raw, &claims)
	iat := time.Now().Add(-time.Hour).Unix()
	claims["iat"], claims["nbf"] = iat, iat
	delete(claims, "iat_ms")
	payload, _ := json.Marshal(claims)
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := head + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(nastest.SigningKey))
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// A token older than the server's record of checks may have checked before
// the record began: after a reset its upload is refused once; a token issued
// since, with no check on record, has never checked and is let through.
func TestAnUnrecordedOlderTokenIsRefusedOnceAfterAReset(t *testing.T) {
	a := newGCAccount(t, "untracked@example.com")
	a.snapshot(syncclient.NoBlobRefs) // the account's record of checks starts
	a.srv.Admin("/admin/user/reset-storage", map[string]any{"emailOrUserId": "untracked@example.com"})
	fresh := a.access
	a.access = olderToken(t, fresh)
	if err := a.trySnapshot(""); !syncclientIs(err, 409, "storage_generation_stale") {
		t.Fatalf("a token from before the record, after a reset: %v", err)
	}
	a.snapshot("") // its next sync goes through

	a.access = fresh // issued since the record began, and never checked
	a.snapshot("")
}

// Declarations do not count against the tier, so their number and size are
// bounded: past the limit the oldest ones go, and an oversized check is
// refused.
func TestDeclarationsAreBounded(t *testing.T) {
	a := newGCAccount(t, "bounded@example.com")
	x := a.blob(1000)
	first := a.check(x)
	for i := 0; i < 200; i++ {
		a.check(x)
	}
	if err := a.trySnapshot(first); !syncclientIs(err, 409, "blob_refs_expired") {
		t.Errorf("the oldest of 201 declarations is gone: %v", err)
	}
	a.snapshot(a.check(x))

	hashes := make([]string, 250_001)
	for i := range hashes {
		hashes[i] = sha([]byte{byte(i), byte(i >> 8), byte(i >> 16), 'b'})
	}
	if _, err := a.c.CheckBlobs(a.ctx, a.access, hashes); !syncclient.IsStatus(err, 413) {
		t.Errorf("a check of 250001 hashes: %v", err)
	}
}

// An older client's check whose declaration went over the limit no longer
// vouches for its upload; the holds kept for downloads are not part of the
// limit.
func TestEvictedDeclarations(t *testing.T) {
	a := newGCAccount(t, "evicted@example.com", nastest.Var("BLOB_GC_DROP_HOLD_SECONDS", "3600"))
	x, y := a.blob(1000), a.blob(1000)
	a.snapshot(a.check(x))
	a.snapshot(syncclient.NoBlobRefs) // x's snapshot goes: x is held for downloads

	older := a.access
	if _, err := a.c.CheckBlobs(a.ctx, older, []string{y}); err != nil { // an older client checks
		t.Fatal(err)
	}
	other := a.srv.Login(a.ctx, "evicted@example.com", "password1").AccessToken
	refs := ""
	for i := 0; i < 200; i++ {
		res, err := a.c.CheckBlobs(a.ctx, other, []string{y})
		if err != nil {
			t.Fatal(err)
		}
		refs = res.RefsToken
	}
	// A collection runs (the other device's snapshot with y goes): x is still held.
	a.access = other
	if err := a.c.UploadBlob(a.ctx, other, y, a.blobs[y]); err != nil {
		t.Fatal(err)
	}
	a.snapshot(refs)
	a.snapshot(syncclient.NoBlobRefs)
	time.Sleep(time.Second)
	if !a.has(x) {
		t.Error("the hold for downloads was evicted with the declarations")
	}

	a.access = older
	if err := a.trySnapshot(""); !syncclientIs(err, 409, "storage_generation_stale") {
		t.Errorf("an older client whose declaration was evicted: %v", err)
	}
	a.snapshot("") // its next sync goes through
}
