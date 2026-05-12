package archive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// Archiver saves raw items to the filesystem for future AI training/indexing.
type Archiver struct {
	BaseDir string
}

func NewLocalArchiver(baseDir string) *Archiver {
	return &Archiver{BaseDir: baseDir}
}

// Archive saves a slice of items to a daily JSON file.
func (a *Archiver) Archive(botName string, items []core.Item) error {
	if len(items) == 0 {
		return nil
	}

	day := time.Now().Format("2006-01-02")
	dir := filepath.Join(a.BaseDir, botName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	path := filepath.Join(dir, day+".jsonl") // JSON Lines format
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	for _, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			continue
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return err
		}
	}

	return nil
}

// Rotate removes JSONL backups older than retainDays. Returns the count
// of removed files. retainDays <= 0 or empty BaseDir is a no-op.
//
// This is intentionally conservative: it only touches .jsonl files
// under BaseDir and never traverses symlinks. Empty subdirectories are
// left in place so future writes do not race a mkdir.
func (a *Archiver) Rotate(retainDays int) (int, error) {
	if retainDays <= 0 || a.BaseDir == "" {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -retainDays)
	removed := 0
	err := filepath.Walk(a.BaseDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if info == nil || info.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err == nil {
				removed++
			}
		}
		return nil
	})
	return removed, err
}

// RecordHeartbeat updates a small file indicating the bot is alive.
// When dir is empty this is a no-op so callers can disable heartbeats
// without conditional logic at the call site.
func RecordHeartbeat(dir, botName string) {
	if dir == "" || botName == "" {
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	path := filepath.Join(dir, botName)
	_ = os.WriteFile(path, []byte(fmt.Sprintf("%d", time.Now().Unix())), 0644)
}
