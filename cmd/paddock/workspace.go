package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// maxPullBytes caps what `paddock pull` will write to disk. The sandbox is
// the untrusted side of this transfer: an agent that filled /workspace with
// a decompression bomb must not be able to fill the developer's disk.
const maxPullBytes = 4 << 30

// pushWorkspace uploads dir into the session's /workspace as a gzipped tar,
// streamed straight from the filesystem to the server — a large repo never
// lands in memory.
func pushWorkspace(sessionID, dir string, clean bool) error {
	files, err := workspaceFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("nothing to upload from %s", dir)
	}

	url := serverURL() + "/v1/sessions/" + sessionID + "/workspace"
	if clean {
		url += "?clean=1"
	}

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeTarGz(pw, dir, files))
	}()

	req, err := http.NewRequest("POST", url, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload workspace: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload workspace: %s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	var result struct {
		Bytes int64 `json:"bytes"`
	}
	_ = json.Unmarshal(raw, &result)
	fmt.Printf("uploaded %d files (%s) to session %s\n", len(files), humanBytes(result.Bytes), sessionID)
	return nil
}

// pullWorkspace downloads the session's /workspace into dir.
func pullWorkspace(sessionID, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	resp, err := http.Get(serverURL() + "/v1/sessions/" + sessionID + "/workspace")
	if err != nil {
		return fmt.Errorf("download workspace: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("download workspace: %s: %s", resp.Status, bytes.TrimSpace(raw))
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	n, err := extractTarGz(root, resp.Body)
	if err != nil {
		return err
	}
	fmt.Printf("pulled %d files from session %s into %s\n", n, sessionID, dir)
	return nil
}

// workspaceFiles lists what to upload. In a git repo that's the tracked and
// untracked-but-not-ignored files plus .git itself (so the agent has real
// history to work with) — which means node_modules and build output stay
// home, and .gitignore is the single source of truth a developer already
// maintains. Outside a repo, everything.
func workspaceFiles(dir string) ([]string, error) {
	if files, ok := gitFiles(dir); ok {
		return files, nil
	}
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}

// gitFiles returns the repo's files, or ok=false when dir isn't a git repo
// (or git isn't installed).
func gitFiles(dir string) ([]string, bool) {
	cmd := exec.Command("git", "-C", dir, "ls-files", "-co", "--exclude-standard", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var files []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	// .git carries the history and branch state; ls-files never lists it.
	gitDir := filepath.Join(dir, ".git")
	if info, err := os.Stat(gitDir); err == nil && info.IsDir() {
		_ = filepath.WalkDir(gitDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // an unreadable .git entry just gets skipped
			}
			if rel, err := filepath.Rel(dir, path); err == nil {
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	return files, true
}

// writeTarGz streams files (relative slash paths) from dir into w.
func writeTarGz(w io.Writer, dir string, files []string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, name := range files {
		if err := addFile(tw, dir, name); err != nil {
			return fmt.Errorf("archive %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func addFile(tw *tar.Writer, dir, name string) error {
	path := filepath.Join(dir, filepath.FromSlash(name))
	info, err := os.Lstat(path)
	if err != nil {
		// A file listed by git but deleted on disk is normal; skip it.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var link string
	if info.Mode()&os.ModeSymlink != 0 {
		if link, err = os.Readlink(path); err != nil {
			return err
		}
	} else if !info.Mode().IsRegular() {
		return nil // sockets, devices, fifos: nothing an agent needs
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	hdr.Name = name
	// Ownership is meaningless across the boundary: the sandbox runs as its
	// own uid, and the tar shouldn't try to recreate the developer's.
	hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

// extractTarGz unpacks a tar.gz from the sandbox under root.
//
// The archive is hostile input — it was assembled from a directory an agent
// had write access to. os.Root confines every write to the target directory
// at the kernel level, so a "../.ssh/authorized_keys" entry or a symlink
// pointing at /etc cannot escape. The name checks and the byte cap sit on
// top of that: fail early with a clear message, and don't let a
// decompression bomb fill the disk on the way.
func extractTarGz(root *os.Root, r io.Reader) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("read workspace archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var written int64
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, fmt.Errorf("read workspace archive: %w", err)
		}
		name, err := safeName(hdr.Name)
		if err != nil {
			return count, err
		}
		if name == "" {
			continue // "./"
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := mkdirAllIn(root, name); err != nil {
				return count, err
			}
		case tar.TypeReg:
			if err := mkdirAllIn(root, filepath.ToSlash(filepath.Dir(name))); err != nil {
				return count, err
			}
			// Replace rather than truncate. Git writes its objects read-only
			// (0444), so a second pull into the same repo cannot reopen them
			// for writing; and creating fresh means a write can never follow
			// a symlink that was already sitting at this path.
			_ = root.Remove(name)
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(hdr.Mode).Perm())
			if err != nil {
				return count, err
			}
			limit := maxPullBytes - written
			n, err := io.Copy(f, io.LimitReader(tr, limit+1))
			f.Close()
			if err != nil {
				return count, err
			}
			written += n
			if written > maxPullBytes {
				return count, fmt.Errorf("workspace exceeds %s; refusing to keep extracting", humanBytes(maxPullBytes))
			}
			count++
		case tar.TypeSymlink:
			if err := mkdirAllIn(root, filepath.ToSlash(filepath.Dir(name))); err != nil {
				return count, err
			}
			_ = root.Remove(name)
			// root.Symlink refuses a target that escapes the root.
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return count, fmt.Errorf("symlink %s -> %s: %w", name, hdr.Linkname, err)
			}
			count++
		default:
			// Hard links, devices, fifos: not worth the blast radius.
			continue
		}
	}
}

// safeName rejects absolute paths and any ".." component before os.Root ever
// sees them, so the error names the problem instead of surfacing as EPERM.
//
// It rejects rather than normalises on purpose. Cleaning "../../etc/passwd"
// into "etc/passwd" would stay inside the directory, but it would silently
// write a file the developer never asked for; an archive that talks about
// its parent is one paddock should refuse to unpack, not quietly reinterpret.
func safeName(name string) (string, error) {
	n := strings.TrimPrefix(filepath.ToSlash(name), "./")
	if n == "" || n == "." || n == "/" {
		return "", nil
	}
	bad := path.IsAbs(n) || strings.HasPrefix(n, "../") ||
		strings.Contains(n, "/../") || strings.HasSuffix(n, "/..") || n == ".."
	if bad || filepath.IsAbs(name) {
		return "", fmt.Errorf("refusing entry %q: it points outside the target directory", name)
	}
	clean := path.Clean(n)
	if clean == "." {
		return "", nil
	}
	return clean, nil
}

func mkdirAllIn(root *os.Root, dir string) error {
	if dir == "." || dir == "" || dir == "/" {
		return nil
	}
	parts := strings.Split(dir, "/")
	cur := ""
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if cur == "" {
			cur = p
		} else {
			cur += "/" + p
		}
		if err := root.Mkdir(cur, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
