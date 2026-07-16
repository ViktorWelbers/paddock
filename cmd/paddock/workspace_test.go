package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tarGz builds an archive from a list of headers and bodies.
func tarGz(t *testing.T, entries ...func(*tar.Writer)) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		e(tw)
	}
	tw.Close()
	gz.Close()
	return bytes.NewReader(buf.Bytes())
}

func file(name, body string) func(*tar.Writer) {
	return func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
}

func symlink(name, target string) func(*tar.Writer) {
	return func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: name, Linkname: target, Typeflag: tar.TypeSymlink, Mode: 0o777})
	}
}

func extractInto(t *testing.T, dir string, r *bytes.Reader) (int, error) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	return extractTarGz(root, r)
}

func TestExtractWritesFilesAndDirs(t *testing.T) {
	dir := t.TempDir()
	n, err := extractInto(t, dir, tarGz(t,
		file("main.go", "package main"),
		file("pkg/util/helper.go", "package util"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("extracted %d files, want 2", n)
	}
	got, err := os.ReadFile(filepath.Join(dir, "pkg/util/helper.go"))
	if err != nil || string(got) != "package util" {
		t.Errorf("nested file = %q, %v", got, err)
	}
}

// The sandbox is the untrusted end of a pull: the agent could have written
// anything into /workspace, including a tar-slip.
func TestExtractRefusesPathTraversal(t *testing.T) {
	for _, name := range []string{
		"../escaped.txt",
		"../../etc/passwd",
		"foo/../../escaped.txt",
		"/etc/passwd",
	} {
		dir := t.TempDir()
		_, err := extractInto(t, dir, tarGz(t, file(name, "pwned")))
		if err == nil {
			t.Errorf("%q: expected the extract to be refused", name)
		}
		if _, statErr := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.txt")); statErr == nil {
			t.Fatalf("%q: wrote outside the target directory", name)
		}
	}
}

// A symlink out of the tree, followed by a write through it, is the classic
// way to turn an archive into arbitrary file overwrite. os.Root is what
// actually stops the second step.
func TestExtractRefusesSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(outside, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := extractInto(t, dir, tarGz(t,
		symlink("link", filepath.Dir(outside)),
		file("link/target.txt", "pwned"),
	))
	if err == nil {
		t.Error("expected the escaping symlink write to be refused")
	}
	got, _ := os.ReadFile(outside)
	if string(got) != "original" {
		t.Fatalf("file outside the target directory was overwritten: %q", got)
	}
}

func TestExtractKeepsInternalSymlink(t *testing.T) {
	dir := t.TempDir()
	if _, err := extractInto(t, dir, tarGz(t,
		file("real.txt", "hi"),
		symlink("alias.txt", "real.txt"),
	)); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(dir, "alias.txt"))
	if err != nil || target != "real.txt" {
		t.Errorf("symlink inside the tree should survive: %q, %v", target, err)
	}
}

// Git writes its objects read-only, so pulling twice into the same repo has
// to replace files it cannot reopen for writing.
func TestExtractOverwritesReadOnlyFile(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "obj")
	if err := os.WriteFile(existing, []byte("old"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := extractInto(t, dir, tarGz(t, file("obj", "new"))); err != nil {
		t.Fatalf("extract over a read-only file: %v", err)
	}
	got, err := os.ReadFile(existing)
	if err != nil || string(got) != "new" {
		t.Errorf("file = %q (%v), want it replaced with %q", got, err, "new")
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"./foo.txt":   "foo.txt",
		"foo/bar.txt": "foo/bar.txt",
		"a/./b.txt":   "a/b.txt",
		"./":          "",
		".":           "",
	}
	for in, want := range cases {
		got, err := safeName(in)
		if err != nil {
			t.Errorf("safeName(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"../x", "/abs", "a/../../x"} {
		if _, err := safeName(bad); err == nil {
			t.Errorf("safeName(%q) must be refused", bad)
		}
	}
}

func TestWorkspaceFilesRespectsGitignore(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		path := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "node_modules/\n")
	write("main.js", "console.log(1)")
	write("node_modules/dep/index.js", "junk")

	if out, err := runGit(dir, "init"); err != nil {
		t.Skipf("git unavailable: %v (%s)", err, out)
	}

	files, ok := gitFiles(dir)
	if !ok {
		t.Fatal("a git repo should be detected")
	}
	joined := strings.Join(files, " ")
	if !strings.Contains(joined, "main.js") {
		t.Errorf("tracked source missing from upload set: %v", files)
	}
	if strings.Contains(joined, "node_modules") {
		t.Errorf("gitignored paths must not be uploaded: %v", files)
	}
	if !strings.Contains(joined, ".git/") {
		t.Errorf("upload set should carry .git so the agent has history: %v", files)
	}
}

func TestWorkspaceFilesOutsideGitRepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := workspaceFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "notes.txt" {
		t.Errorf("files = %v, want the plain walk to find notes.txt", files)
	}
}

func TestTarRoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	if err := os.WriteFile(filepath.Join(src, "sub/b.txt"), []byte("world"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writeTarGz(&buf, src, []string{"a.txt", "sub/b.txt"}); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if _, err := extractInto(t, dst, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a.txt": "hello", "sub/b.txt": "world"} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q", name, got, err, want)
		}
	}
	info, err := os.Stat(filepath.Join(dst, "sub/b.txt"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600 preserved", info.Mode().Perm())
	}
}

// runGit is a test helper for setting up repos.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
