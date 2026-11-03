package account

import (
	"context"
	"errors"
	"klip/internal/model"
	"klip/internal/nastest"
	"testing"
)

type saveFailureStore struct {
	Store
	err error
}

func (s saveFailureStore) Update(f func(*model.Settings)) error {
	if err := s.Store.Update(f); err != nil {
		return err
	}
	return s.err
}

func TestRefreshReportsPersistenceFailure(t *testing.T) {
	srv := nastest.Start(t)
	s, m := newService(t, srv.URL)
	if r := s.Register(context.Background(), "save@example.com", "password1"); !r.OK {
		t.Fatal(r)
	}
	old := model.Str(m.Get().AuthRefreshToken)
	if err := m.Update(func(st *model.Settings) { st.AuthExpiresAtUnixMs = 0 }); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("disk full")
	s.Store = saveFailureStore{m, failure}
	token, err := s.EnsureAccessToken(context.Background())
	if token != "" || !errors.Is(err, failure) {
		t.Fatalf("refresh silently succeeded: %q %v", token, err)
	}
	if model.Str(m.Get().AuthRefreshToken) == old {
		t.Fatal("rotated token not retained in memory")
	}
}
