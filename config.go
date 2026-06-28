package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	minimaxModelAlias    = "blackbox/minimax-m2.7"
	minimaxUpstreamModel = "openrouter/minimax-m2-thinking"
	kimiModelAlias       = "blackbox/kimi-k2.6"
	kimiUpstreamModel    = "moonshotai/kimi-k2.6"
	kimiFallbackModel    = "gpt-4o-mini"
)

type config struct {
	host               string
	port               int
	upstreamBaseURL    string
	upstreamTimeout    time.Duration
	blackboxUserID     string
	blackboxGatewayKey string
	bridgeAPIKey       string
	maxRequestBytes    int64
	maxImageBytes      int64
	maxConcurrent      int
	reasonixWorkspace  string
}

func loadConfig() (config, error) {
	userID, err := resolveBlackboxUserID()
	if err != nil {
		return config{}, err
	}

	workspace := strings.TrimSpace(os.Getenv("REASONIX_WORKSPACE"))
	if workspace == "" && runtime.GOOS == "windows" {
		home, homeErr := os.UserHomeDir()
		if homeErr == nil {
			workspace = filepath.Join(home, "AppData", "Roaming", "reasonix", "global-workspace")
		}
	}

	return config{
		host:               envString("HOST", "127.0.0.1"),
		port:               envInt("PORT", 39281),
		upstreamBaseURL:    strings.TrimRight(envString("BLACKBOX_UPSTREAM_BASE_URL", "https://oi-vscode-server-985058387028.europe-west1.run.app"), "/"),
		upstreamTimeout:    time.Duration(envInt("UPSTREAM_TIMEOUT_MS", 180000)) * time.Millisecond,
		blackboxUserID:     userID,
		blackboxGatewayKey: envString("BLACKBOX_GATEWAY_API_KEY", "xxx"),
		bridgeAPIKey:       strings.TrimSpace(os.Getenv("BRIDGE_API_KEY")),
		maxRequestBytes:    envInt64("MAX_REQUEST_BYTES", 25*1024*1024),
		maxImageBytes:      envInt64("MAX_IMAGE_BYTES", 10*1024*1024),
		maxConcurrent:      envInt("MAX_CONCURRENT", 4),
		reasonixWorkspace:  workspace,
	}, nil
}

func resolveBlackboxUserID() (string, error) {
	if userID := strings.TrimSpace(os.Getenv("BLACKBOX_USER_ID")); userID != "" {
		return userID, nil
	}
	if runtime.GOOS != "windows" {
		return "", fmt.Errorf("BLACKBOX_USER_ID is required outside Windows")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	dbPath := envString("BLACKBOX_STATE_DB", filepath.Join(home, "AppData", "Roaming", "Code", "User", "globalStorage", "state.vscdb"))
	python := envString("PYTHON", "python")
	script := strings.Join([]string{
		"import json, sqlite3, sys",
		"con = sqlite3.connect(sys.argv[1])",
		"row = con.execute('select value from ItemTable where key=?', ('Blackboxapp.blackboxagent',)).fetchone()",
		"con.close()",
		"state = json.loads(row[0]) if row and row[0] else {}",
		"print(state.get('blackbox_userId') or state.get('userId') or '')",
	}, "; ")
	out, err := exec.Command(python, "-c", script, dbPath).Output()
	if err != nil {
		return "", fmt.Errorf("read BLACKBOX state: %w", err)
	}
	userID := strings.TrimSpace(string(out))
	if userID == "" {
		return "", fmt.Errorf("BLACKBOX user id was not found in VS Code global storage")
	}
	return userID, nil
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envInt64(name string, fallback int64) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
