package tooling

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchCommitsStrictMultiFileChange(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	applier := &patchApplier{root: root}
	result, err := applier.apply(context.Background(), ApplyPatchInput{Patch: `diff --git a/old.txt b/old.txt
--- a/old.txt
+++ b/old.txt
@@ -1 +1 @@
-old
+updated
diff --git a/new.txt b/new.txt
new file mode 100644
--- /dev/null
+++ b/new.txt
@@ -0,0 +1 @@
+created
`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Changed, ",") != "old.txt,new.txt" {
		t.Fatalf("changed paths = %#v", result.Changed)
	}
	assertFileContent(t, filepath.Join(root, "old.txt"), "updated\n")
	assertFileContent(t, filepath.Join(root, "new.txt"), "created\n")
	info, err := os.Stat(filepath.Join(root, "old.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("updated file mode = %o, want 640", info.Mode().Perm())
	}
}

func TestApplyPatchConflictLeavesEveryFileUnchanged(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"one.txt": "one\n", "two.txt": "two\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	applier := &patchApplier{root: root}
	_, err := applier.apply(context.Background(), ApplyPatchInput{Patch: `diff --git a/one.txt b/one.txt
--- a/one.txt
+++ b/one.txt
@@ -1 +1 @@
-one
+changed
diff --git a/two.txt b/two.txt
--- a/two.txt
+++ b/two.txt
@@ -1 +1 @@
-not-two
+changed
`})
	if err == nil {
		t.Fatal("conflicting patch succeeded")
	}
	assertFileContent(t, filepath.Join(root, "one.txt"), "one\n")
	assertFileContent(t, filepath.Join(root, "two.txt"), "two\n")
}

func TestApplyPatchRejectsLexicalAndSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	applier := &patchApplier{root: root}
	for name, patch := range map[string]string{
		"lexical": `--- ../outside/owned.txt
+++ ../outside/owned.txt
@@ -0,0 +1 @@
+owned
`,
		"symlink": `--- /dev/null
+++ escape/owned.txt
@@ -0,0 +1 @@
+owned
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := applier.apply(context.Background(), ApplyPatchInput{Patch: patch}); err == nil {
				t.Fatal("escaping patch succeeded")
			}
		})
	}
	if _, err := os.Stat(filepath.Join(outside, "owned.txt")); !os.IsNotExist(err) {
		t.Fatalf("escape created outside file: %v", err)
	}
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != expected {
		t.Fatalf("%s = %q, want %q", path, content, expected)
	}
}
