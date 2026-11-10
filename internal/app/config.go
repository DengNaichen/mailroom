package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Addr              string
	DataDir           string
	Demo              bool
	OpenAIKey         string
	Model             string
	OpenAIBase        string
	GoogleCredentials string
	PollSeconds       int
	AutoCodex         bool
	AllowCodexActions bool
	CodexBinary       string
}

func ConfigFromEnv() Config {
	home, _ := os.UserConfigDir()
	dir := env("MAILROOM_DATA_DIR", filepath.Join(home, "mailroom"))
	demo := os.Getenv("MAILROOM_DEMO") == "1"
	if demo {
		dir = env("MAILROOM_DATA_DIR", filepath.Join(home, "mailroom-demo"))
	}
	seconds, _ := strconv.Atoi(env("MAILROOM_POLL_SECONDS", "120"))
	if seconds < 30 {
		seconds = 30
	}
	return Config{Addr: env("MAILROOM_ADDR", "127.0.0.1:8787"), DataDir: dir, Demo: demo, OpenAIKey: os.Getenv("OPENAI_API_KEY"), Model: os.Getenv("OPENAI_MODEL"), OpenAIBase: "https://api.openai.com/v1", GoogleCredentials: env("GOOGLE_CREDENTIALS", "credentials.json"), PollSeconds: seconds, AutoCodex: os.Getenv("MAILROOM_AUTO_CODEX") == "1", AllowCodexActions: os.Getenv("MAILROOM_CODEX_ACTIONS") == "1", CodexBinary: env("CODEX_BINARY", "codex")}
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func atomicJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
func (c Config) Validate() error {
	if !strings.HasPrefix(c.Addr, "127.0.0.1:") {
		return errors.New("第一版仅监听 127.0.0.1；Linux 远程使用 SSH 端口转发")
	}
	return nil
}
