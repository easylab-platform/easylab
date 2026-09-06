package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnoredBasic(t *testing.T) {
	m := NewWithLines(".", "*.log", "build/", "!keep.log")
	if !m.Ignored("x.log") {
		t.Fatal("x.log should be ignored")
	}
	if m.Ignored("keep.log") {
		t.Fatal("keep.log should NOT be ignored (negation)")
	}
	if !m.Ignored("build/x.txt") {
		t.Fatal("build/x.txt should be ignored (dir rule)")
	}
	if m.Ignored("src/main.go") {
		t.Fatal("src/main.go should not be ignored")
	}
}

func TestNestedGitignoreLayer(t *testing.T) {
	root := t.TempDir()
	mk := func(p string) {
		_ = os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		_ = os.WriteFile(filepath.Join(root, p), nil, 0o644)
	}
	_ = os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.tmp\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	// nested .gitignore ignores under sub/ (positive pattern in nested layer)
	_ = os.WriteFile(filepath.Join(root, "sub", ".gitignore"), []byte("*.build\n"), 0o644)
	mk("root.tmp")
	mk("sub/a.tmp")
	mk("sub/b.build")

	m, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Ignored("root.tmp") {
		t.Fatal("root.tmp should be ignored by root .gitignore")
	}
	if !m.Ignored("sub/a.tmp") {
		t.Fatal("sub/a.tmp should be ignored")
	}
	if !m.Ignored("sub/b.build") {
		t.Fatal("sub/b.build should be ignored by nested .gitignore")
	}
	if m.Ignored("other.txt") {
		t.Fatal("other.txt should not be ignored")
	}
}

func TestVcsIgnoreAlsoRespected(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, ".vcsignore"), []byte("secret*\n"), 0o644)
	m, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Ignored("secret.txt") {
		t.Fatal(".vcsignore pattern should be honored")
	}
	if m.Ignored("public.txt") {
		t.Fatal("public.txt should not be ignored")
	}
}
