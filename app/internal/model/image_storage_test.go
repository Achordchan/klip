package model

import "testing"

func TestWebPPreferenceDefaultAndRoundtrip(t *testing.T) {
	s := DefaultSettings()
	if !s.StoreImagesAsWebP() {
		t.Fatal("not enabled by default")
	}
	s.SetStoreImagesAsWebP(false)
	b, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSettings(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.StoreImagesAsWebP() {
		t.Fatal("disabled setting lost")
	}
	legacy, err := ParseSettings([]byte(`{}`))
	if err != nil || !legacy.StoreImagesAsWebP() {
		t.Fatal("legacy default not enabled")
	}
}
