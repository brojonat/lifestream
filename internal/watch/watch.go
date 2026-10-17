// Package watch notices when local files change. It fingerprints the
// watched directories on a timer and bumps the store's stamp when the
// fingerprint moves, which wakes every open page. Polling is deliberate: it
// is portable, needs no extra dependency, and a few seconds of latency is
// fine for files a person edits.
package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/brojonat/lifestream/internal/state"
)

// Fingerprint hashes the path, size, and mtime of every regular file under
// the given directories. Missing directories contribute nothing.
func Fingerprint(dirs []string) string {
	h := sha256.New()
	for _, dir := range dirs {
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if info, err := d.Info(); err == nil {
				fmt.Fprintf(h, "%s %d %d\n", path, info.Size(), info.ModTime().UnixNano())
			}
			return nil
		})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Run polls until ctx ends, touching the store whenever the fingerprint
// changes.
func Run(ctx context.Context, dirs []string, store *state.Store, interval time.Duration, log *slog.Logger) {
	if len(dirs) == 0 {
		return
	}
	last, _ := store.Fingerprint(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if fp := Fingerprint(dirs); fp != last {
			if err := store.Touch(ctx, fp); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Warn("touch", "error", err)
			} else {
				log.Debug("watched files changed")
				last = fp
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
