package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type signalPersister struct {
	persisted chan struct{}
}

func (p *signalPersister) PersistConfig(context.Context) error {
	p.persisted <- struct{}{}
	return nil
}

func (p *signalPersister) PersistAuthFiles(context.Context, string, ...string) error { return nil }

// LoadConfig rewrites a plaintext secret-key as a bcrypt hash. The write-back's own
// change event loads the normalized file once, records it, and later events skip it.
func TestConfigReloadSettlesOnTheNormalizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	authDir := filepath.Join(dir, "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	body := fmt.Sprintf("port: 8080\nauth-dir: %q\nremote-management:\n  secret-key: \"plain-fixture-secret\"\n", authDir)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var callbacks atomic.Int32
	persister := &signalPersister{persisted: make(chan struct{}, 4)}
	w := &Watcher{
		configPath:     path,
		authDir:        authDir,
		storePersister: persister,
		reloadCallback: func(*config.Config) { callbacks.Add(1) },
	}

	w.reloadConfigIfChanged()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "plain-fixture-secret") {
		t.Fatal("secret-key was not normalized")
	}

	// The normalization write-back's event.
	w.reloadConfigIfChanged()
	if got := callbacks.Load(); got != 2 {
		t.Fatalf("callbacks = %d, want 2 (load, then load the normalized file)", got)
	}
	sum := sha256.Sum256(data)
	w.clientsMutex.RLock()
	recorded := w.lastConfigHash
	w.clientsMutex.RUnlock()
	if recorded != hex.EncodeToString(sum[:]) {
		t.Fatal("normalized file is not recorded")
	}

	w.reloadConfigIfChanged()
	if got := callbacks.Load(); got != 2 {
		t.Fatalf("callbacks = %d after an unchanged event, want 2", got)
	}
	select {
	case <-persister.persisted:
	case <-time.After(5 * time.Second):
		t.Fatal("config was not persisted")
	}
}
