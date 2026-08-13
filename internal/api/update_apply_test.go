package api

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStageUpdate proves the swap replaces exe + web/ with the extracted build, keeps a
// backup of each, and never touches sibling data files.
func TestStageUpdate(t *testing.T) {
	install := t.TempDir()
	exePath := filepath.Join(install, "zyper-bot.exe")
	mustWrite(t, exePath, "OLD-EXE")
	mustWrite(t, filepath.Join(install, "web", "index.html"), "OLD-INDEX")
	mustWrite(t, filepath.Join(install, "web", "app.js"), "OLD-APP")
	mustWrite(t, filepath.Join(install, "zyperbot.db"), "DATA") // must survive untouched

	// Extracted build, wrapped in a top folder like a real zip.
	ex := t.TempDir()
	top := filepath.Join(ex, "zyper-bot-1.4.0")
	mustWrite(t, filepath.Join(top, "zyper-bot.exe"), "NEW-EXE")
	mustWrite(t, filepath.Join(top, "web", "index.html"), "NEW-INDEX")
	mustWrite(t, filepath.Join(top, "web", "app.js"), "NEW-APP")

	if _, err := stageUpdate(exePath, ex); err != nil {
		t.Fatalf("stageUpdate: %v", err)
	}
	if got := read(t, exePath); got != "NEW-EXE" {
		t.Fatalf("exe not replaced: %q", got)
	}
	if got := read(t, filepath.Join(install, "web", "index.html")); got != "NEW-INDEX" {
		t.Fatalf("web/index.html not replaced: %q", got)
	}
	if got := read(t, filepath.Join(install, "zyperbot.db")); got != "DATA" {
		t.Fatalf("data file was touched: %q", got)
	}
	// Backups of the old exe and old web/ must exist for rollback / cleanup.
	entries, _ := os.ReadDir(install)
	var oldExe, oldWeb bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "zyper-bot.exe.old-") {
			oldExe = true
		}
		if strings.HasPrefix(e.Name(), "web.old-") {
			oldWeb = true
		}
	}
	if !oldExe || !oldWeb {
		t.Fatalf("missing backups: oldExe=%v oldWeb=%v", oldExe, oldWeb)
	}

	// CleanupStaleUpdate removes both backups but keeps the live install.
	CleanupStaleUpdate(install)
	if read(t, exePath) != "NEW-EXE" || read(t, filepath.Join(install, "web", "index.html")) != "NEW-INDEX" {
		t.Fatal("cleanup damaged the live install")
	}
	for _, e := range mustReadDir(t, install) {
		if strings.Contains(e, ".old-") {
			t.Fatalf("cleanup left a backup: %s", e)
		}
	}
}

// TestStageUpdateRejectsNonBuild fails cleanly when the archive is neither exe nor web.
func TestStageUpdateRejectsNonBuild(t *testing.T) {
	install := t.TempDir()
	exePath := filepath.Join(install, "zyper-bot.exe")
	mustWrite(t, exePath, "OLD-EXE")
	ex := t.TempDir()
	mustWrite(t, filepath.Join(ex, "README.md"), "not a build")
	if _, err := stageUpdate(exePath, ex); err == nil {
		t.Fatal("expected error for a non-build archive")
	}
	if read(t, exePath) != "OLD-EXE" {
		t.Fatal("exe must be untouched when staging fails")
	}
}

// TestUnzipRejectsZipSlip proves a malicious "../" entry can't escape the destination.
func TestUnzipRejectsZipSlip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "evil.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	w, _ := zw.Create("../escape.txt")
	_, _ = w.Write([]byte("pwn"))
	zw.Close()
	zf.Close()
	if err := unzip(zipPath, filepath.Join(dir, "dst")); err == nil {
		t.Fatal("expected zip-slip rejection")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); err == nil {
		t.Fatal("zip-slip wrote outside dst")
	}
}

// TestUnzipBackslashNames covers zips written by Windows PowerShell Compress-Archive,
// which stores "web\app.js" — the exact producer of our release asset. The full
// unzip → findByBase → stageUpdate pipeline must locate exe + web/ from such a zip.
func TestUnzipBackslashNames(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "build.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	for name, body := range map[string]string{
		"zyper-bot.exe":   "NEW-EXE",
		"web\\index.html": "NEW-INDEX",
		"web\\app.js":     "NEW-APP",
	} {
		w, _ := zw.Create(name) // backslash names, as Compress-Archive writes them
		_, _ = w.Write([]byte(body))
	}
	zw.Close()
	zf.Close()

	ex := filepath.Join(dir, "x")
	if err := unzip(zipPath, ex); err != nil {
		t.Fatalf("unzip: %v", err)
	}
	install := t.TempDir()
	exePath := filepath.Join(install, "zyper-bot.exe")
	mustWrite(t, exePath, "OLD-EXE")
	mustWrite(t, filepath.Join(install, "web", "index.html"), "OLD-INDEX")
	if _, err := stageUpdate(exePath, ex); err != nil {
		t.Fatalf("stageUpdate: %v", err)
	}
	if read(t, exePath) != "NEW-EXE" || read(t, filepath.Join(install, "web", "index.html")) != "NEW-INDEX" {
		t.Fatal("backslash-zip build did not stage correctly")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}
