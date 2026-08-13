package api

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"zyperbot/internal/config"
	"zyperbot/internal/logger"
)

// maxAssetBytes caps a downloaded release build (a padded ceiling — the app + web/ zip is
// a few MB). Guards against a mistaken/huge asset filling the disk.
const maxAssetBytes = 300 << 20 // 300 MB

// updateRepo returns the user's configured update source, or the canonical default so the
// auto-check / download works out of the box.
func (s *Server) updateRepo() string {
	repo := ""
	if v, err := s.st.GetSetting("app.config"); err == nil && v != "" {
		var m map[string]any
		if json.Unmarshal([]byte(v), &m) == nil {
			if rr, ok := m["updateRepo"].(string); ok {
				repo = strings.TrimSpace(rr)
			}
		}
	}
	if repo == "" {
		repo = defaultUpdateRepo
	}
	return repo
}

// POST /api/update/apply — download the latest release's Windows build zip, extract it,
// and stage the new zyper-bot.exe + web/ over the running install. The exe is swapped via
// Windows' allow-rename-of-running-image; data (db, vault.key, logs, .env) is untouched.
// On success the UI prompts the user to restart.
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	repo := s.updateRepo()
	body, status, err := githubGET(r.Context(), "https://api.github.com/repos/"+repo+"/releases/latest")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if status == http.StatusNotFound {
		writeErr(w, http.StatusBadGateway, "no published release to download — the latest tag has no attached build. Attach a .zip build asset to a GitHub Release.")
		return
	}
	if status != http.StatusOK {
		writeErr(w, http.StatusBadGateway, "github "+http.StatusText(status))
		return
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if json.Unmarshal(body, &rel) != nil {
		writeErr(w, http.StatusBadGateway, "bad release json")
		return
	}
	latest := strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
	if !semverGreater(latest, config.Version) {
		writeJSON(w, http.StatusOK, map[string]any{"applied": false, "latest": latest, "message": "already on the latest version"})
		return
	}
	assetURL, assetName := "", ""
	for _, a := range rel.Assets {
		if !strings.HasSuffix(strings.ToLower(a.Name), ".zip") {
			continue
		}
		lc := strings.ToLower(a.Name)
		if assetURL == "" || strings.Contains(lc, "win") { // prefer a windows-named zip, else the first zip
			assetURL, assetName = a.URL, a.Name
		}
	}
	if assetURL == "" {
		writeErr(w, http.StatusBadGateway, "release "+rel.TagName+" has no downloadable .zip build asset")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	summary, err := downloadAndStage(ctx, assetURL)
	if err != nil {
		s.log.API(logger.ERROR, "update apply failed", map[string]any{"version": latest, "asset": assetName, "err": err.Error()})
		writeErr(w, http.StatusInternalServerError, "update: "+err.Error())
		return
	}
	s.log.API(logger.INFO, "update staged", map[string]any{"version": latest, "asset": assetName, "staged": summary})
	writeJSON(w, http.StatusOK, map[string]any{"applied": true, "needsRestart": true, "version": latest, "message": "installed " + summary})
}

// POST /api/update/restart — relaunch the (now-updated) exe and exit this process, so the
// user gets the new build without hunting for the shortcut. Best-effort convenience; if it
// fails the UI still tells them to reopen manually.
func (s *Server) handleUpdateRestart(w http.ResponseWriter, r *http.Request) {
	exe, err := os.Executable()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusInternalServerError, "relaunch: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	// Give the response a beat to flush, then exit; the freshly spawned process opens a
	// new window. os.Exit (not a graceful shutdown) is fine — the new build owns the data.
	go func() { time.Sleep(500 * time.Millisecond); os.Exit(0) }()
}

// downloadAndStage downloads the build zip, extracts it under the install dir, and stages
// the new exe + web/. All temp work happens in a .update-tmp beside the exe (same volume,
// so the final rename/copy can't cross devices) and is removed afterward.
func downloadAndStage(ctx context.Context, url string) (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	if rp, e := filepath.EvalSymlinks(exePath); e == nil {
		exePath = rp
	}
	work := filepath.Join(filepath.Dir(exePath), ".update-tmp")
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	zipPath := filepath.Join(work, "build.zip")
	if err := downloadFile(ctx, url, zipPath); err != nil {
		return "", err
	}
	exDir := filepath.Join(work, "x")
	if err := unzip(zipPath, exDir); err != nil {
		return "", err
	}
	return stageUpdate(exePath, exDir)
}

// stageUpdate swaps the new exe and web/ found under extractedDir into exePath's install
// dir. Split from the HTTP/download path so the risky file swap is unit-testable against a
// temp install dir. The current exe is renamed aside first (Windows permits renaming a
// running image, but not overwriting it in place); on any failure every step is rolled back.
func stageUpdate(exePath, extractedDir string) (string, error) {
	exeDir := filepath.Dir(exePath)
	newExe := findByBase(extractedDir, filepath.Base(exePath))
	newWeb := ""
	if idx := findByBase(extractedDir, "index.html"); idx != "" {
		newWeb = filepath.Dir(idx)
	}
	if newExe == "" && newWeb == "" {
		return "", errors.New("archive has neither " + filepath.Base(exePath) + " nor web/index.html — not a build zip")
	}

	suffix := ".old-" + strconv.FormatInt(time.Now().Unix(), 10)
	var done []string
	exeBak := ""
	if newExe != "" {
		exeBak = exePath + suffix
		if err := os.Rename(exePath, exeBak); err != nil {
			return "", fmt.Errorf("set aside current exe (write-protected or AV lock?): %w", err)
		}
		if err := copyFile(newExe, exePath); err != nil {
			_ = os.Rename(exeBak, exePath) // rollback
			return "", fmt.Errorf("write new exe: %w", err)
		}
		done = append(done, filepath.Base(exePath))
	}
	if newWeb != "" {
		webDir := filepath.Join(exeDir, "web")
		webBak := webDir + suffix
		hadWeb := false
		if _, err := os.Stat(webDir); err == nil {
			hadWeb = true
			if err := os.Rename(webDir, webBak); err != nil {
				rollbackExe(exePath, exeBak, newExe != "")
				return "", fmt.Errorf("set aside current web/: %w", err)
			}
		}
		if err := copyTree(newWeb, webDir); err != nil {
			if hadWeb {
				_ = os.RemoveAll(webDir)
				_ = os.Rename(webBak, webDir)
			}
			rollbackExe(exePath, exeBak, newExe != "")
			return "", fmt.Errorf("write new web/: %w", err)
		}
		done = append(done, "web/")
	}
	return strings.Join(done, " + "), nil
}

func rollbackExe(exePath, exeBak string, did bool) {
	if did && exeBak != "" {
		_ = os.Remove(exePath)
		_ = os.Rename(exeBak, exePath)
	}
}

// CleanupStaleUpdate removes leftovers from a previous update (the renamed-aside old exe,
// the backed-up old web/, and any interrupted temp dir). Best-effort; called at startup.
func CleanupStaleUpdate(exeDir string) {
	_ = os.RemoveAll(filepath.Join(exeDir, ".update-tmp"))
	entries, err := os.ReadDir(exeDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".old-") { // "zyper-bot.exe.old-…" and "web.old-…"
			_ = os.RemoveAll(filepath.Join(exeDir, e.Name()))
		}
	}
}

// --- small file helpers ---

// findByBase returns the path of the first entry under root whose base name equals `base`
// (case-insensitive), or "" if none. Walks so it finds files inside a top-level folder the
// zip may wrap everything in (e.g. "zyper-bot-1.3.2/…").
func findByBase(root, base string) string {
	want := strings.ToLower(base)
	found := ""
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if strings.ToLower(d.Name()) == want {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(p, target)
	})
}

// downloadFile streams url to path, aborting if it exceeds maxAssetBytes.
func downloadFile(ctx context.Context, url, path string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("download %s", resp.Status)
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(resp.Body, maxAssetBytes+1)); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if fi, e := os.Stat(path); e == nil && fi.Size() > maxAssetBytes {
		return fmt.Errorf("asset larger than %d MB — refusing", maxAssetBytes>>20)
	}
	return nil
}

// unzip extracts a zip into dst, rejecting any entry that would escape dst (zip-slip).
func unzip(zipPath, dst string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		// Windows PowerShell's Compress-Archive stores backslash separators; normalize to
		// forward slashes so extraction is correct on any OS (not just Windows).
		name := strings.ReplaceAll(f.Name, "\\", "/")
		target := filepath.Join(dst, filepath.FromSlash(name))
		// Zip-slip: every extracted path must stay under dst.
		if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe zip path: %s", f.Name)
		}
		if f.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		// Bound per-file extraction too, so a zip-bomb entry can't fill the disk.
		if _, err := io.Copy(out, io.LimitReader(rc, maxAssetBytes+1)); err != nil {
			out.Close()
			rc.Close()
			return err
		}
		out.Close()
		rc.Close()
	}
	return nil
}
