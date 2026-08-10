package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// piTestTree lays out a project directory with a parent session and both
// subagent storage layouts PI uses:
//
//	<project>/2026-08-10T05-58-27-620Z_parent.jsonl        parent
//	<project>/2026-08-10T05-58-27-620Z_parent/nested/run-0/session.jsonl
//	<project>/2026-08-10T05-59-35-727Z_sibling.jsonl       parentSession: parent
//	<project>/2026-08-10T05-58-27-620Z_parent/empty/       created but never written
//
// It returns the project dir plus the parent, nested and sibling paths.
func piTestTree(t *testing.T) (projectDir, parentPath, nestedPath, siblingPath string) {
	t.Helper()
	projectDir = t.TempDir()

	parentPath = filepath.Join(projectDir, "2026-08-10T05-58-27-620Z_parent.jsonl")
	if err := os.WriteFile(parentPath, []byte(`{"type":"session","version":3,"id":"parent","timestamp":"2026-08-10T05:58:27.620Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	parentDir := filepath.Join(projectDir, "2026-08-10T05-58-27-620Z_parent")
	nestedDir := filepath.Join(parentDir, "nested", "run-0")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A subagent whose short-ID directory was created but never written — the
	// real-world marker of a sibling-layout run.
	if err := os.MkdirAll(filepath.Join(parentDir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	nestedPath = filepath.Join(nestedDir, "session.jsonl")
	if err := os.WriteFile(nestedPath, []byte(`{"type":"session","version":3,"id":"nested","timestamp":"2026-08-10T06:05:58.749Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	siblingPath = filepath.Join(projectDir, "2026-08-10T05-59-35-727Z_sibling.jsonl")
	header := fmt.Sprintf(`{"type":"session","version":3,"id":"sibling","timestamp":"2026-08-10T05:59:35.727Z","parentSession":%q}`, parentPath)
	if err := os.WriteFile(siblingPath, []byte(header+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectDir, parentPath, nestedPath, siblingPath
}

func TestPiSubsessionPaths_IncludesSiblingLayout(t *testing.T) {
	_, parentPath, nestedPath, siblingPath := piTestTree(t)

	got := piSubsessionPaths(parentPath)
	want := []string{nestedPath, siblingPath}
	if len(got) != len(want) {
		t.Fatalf("piSubsessionPaths() = %v, want %v", got, want)
	}
	found := make(map[string]bool, len(got))
	for _, p := range got {
		found[p] = true
	}
	for _, p := range want {
		if !found[p] {
			t.Errorf("piSubsessionPaths() missing %s (got %v)", p, got)
		}
	}
}

func TestPiSubsessionPaths_SubsessionHasNoChildren(t *testing.T) {
	_, _, nestedPath, siblingPath := piTestTree(t)

	for _, fp := range []string{nestedPath, siblingPath} {
		if got := piSubsessionPaths(fp); len(got) != 0 {
			t.Errorf("piSubsessionPaths(%s) = %v, want none", fp, got)
		}
	}
}

func TestPiResolveParent_BothLayouts(t *testing.T) {
	_, parentPath, nestedPath, siblingPath := piTestTree(t)

	for _, fp := range []string{nestedPath, siblingPath} {
		gotPath, gotID, ok := piResolveParent(fp)
		if !ok {
			t.Fatalf("piResolveParent(%s) not resolved", fp)
		}
		if gotID != "parent" {
			t.Errorf("piResolveParent(%s) id = %q, want %q", fp, gotID, "parent")
		}
		if absPath(gotPath) != absPath(parentPath) {
			t.Errorf("piResolveParent(%s) path = %q, want %q", fp, gotPath, parentPath)
		}
	}

	if _, _, ok := piResolveParent(parentPath); ok {
		t.Errorf("piResolveParent(%s) resolved a parent, want none", parentPath)
	}
}

func TestPiSubsessionCount_CountsBothLayouts(t *testing.T) {
	_, parentPath, _, _ := piTestTree(t)

	cutoff := time.Now().AddDate(0, 0, -1)
	if got := piSubsessionCount(parentPath, cutoff, ""); got != 2 {
		t.Errorf("piSubsessionCount() = %d, want 2", got)
	}
	// A cutoff in the future excludes everything.
	if got := piSubsessionCount(parentPath, time.Now().Add(time.Hour), ""); got != 0 {
		t.Errorf("piSubsessionCount() with future cutoff = %d, want 0", got)
	}
	// Date filtering applies to both layouts alike.
	today := time.Now().UTC().Format("2006-01-02")
	if got := piSubsessionCount(parentPath, cutoff, today); got != 2 {
		t.Errorf("piSubsessionCount(date=%s) = %d, want 2", today, got)
	}
	if got := piSubsessionCount(parentPath, cutoff, "1999-01-01"); got != 0 {
		t.Errorf("piSubsessionCount(date=1999-01-01) = %d, want 0", got)
	}
}
