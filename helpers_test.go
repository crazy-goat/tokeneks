package main

import "testing"

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
