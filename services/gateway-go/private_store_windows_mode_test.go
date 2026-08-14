package main

import (
	"os"
	"testing"
)

func TestOwnerOnlyWindowsDescriptorModePermitted(t *testing.T) {
	for _, test := range []struct {
		name     string
		mode     os.FileMode
		writable bool
		want     bool
	}{
		{name: "normal writable Windows descriptor", mode: 0o666, writable: true, want: true},
		{name: "owner writable descriptor", mode: 0o600, writable: true, want: true},
		{name: "read-only descriptor", mode: 0o444, writable: false, want: true},
		{name: "read-only rejected for append", mode: 0o444, writable: true, want: false},
		{name: "directory rejected", mode: os.ModeDir | 0o755, writable: false, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ownerOnlyWindowsDescriptorModePermitted(test.mode, test.writable); got != test.want {
				t.Fatalf("mode permitted=%v, want %v for mode=%#o writable=%v", got, test.want, test.mode.Perm(), test.writable)
			}
		})
	}
}
