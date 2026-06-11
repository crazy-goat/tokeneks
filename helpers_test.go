package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandHome_TildeSlash(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot get home dir: %v", err)
	}
	got := expandHome("~/foo")
	want := filepath.Join(home, "foo")
	if got != want {
		t.Errorf("expandHome(~\"/foo\") = %q, want %q", got, want)
	}
}

func TestExpandHome_BareTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot get home dir: %v", err)
	}
	got := expandHome("~")
	if got != home {
		t.Errorf("expandHome(~) = %q, want %q", got, home)
	}
}

func TestExpandHome_NoTilde(t *testing.T) {
	got := expandHome("/absolute/path")
	want := "/absolute/path"
	if got != want {
		t.Errorf("expandHome(absolute) = %q, want %q", got, want)
	}
}

func TestOpenOCDB_ReturnsSameInstance(t *testing.T) {
	ocDBMu.Lock()
	if ocDB != nil {
		_ = ocDB.Close()
	}
	ocDB = nil
	ocDBErr = nil
	ocDBMu.Unlock()

	first, err := openOCDB()
	if err != nil {
		t.Fatalf("openOCDB() first call error: %v", err)
	}
	second, err := openOCDB()
	if err != nil {
		t.Fatalf("openOCDB() second call error: %v", err)
	}
	if first != second {
		t.Fatalf("openOCDB() returned different instances: %p vs %p", first, second)
	}

	t.Cleanup(func() {
		ocDBMu.Lock()
		if ocDB != nil {
			_ = ocDB.Close()
		}
		ocDB = nil
		ocDBErr = nil
		ocDBMu.Unlock()
	})
}
