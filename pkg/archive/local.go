package archive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
