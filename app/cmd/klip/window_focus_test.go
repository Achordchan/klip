package main

import "testing"

func TestVisibleAuxiliaryWindowsKeepMainPanel(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		main, prefs, about, hide bool
	}{
		{"main", true, false, false, false},
		{"preferences open while another application has focus", false, true, false, false},
		{"about open while another application has focus", false, false, true, false},
		{"both auxiliary windows open", false, true, true, false},
		{"another application with auxiliary windows closed", false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hideAfterFocusTransfer(tc.main, tc.prefs, tc.about); got != tc.hide {
				t.Fatalf("hide = %v, want %v", got, tc.hide)
			}
		})
	}
}
