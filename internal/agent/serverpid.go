package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ServerPIDInfo records a process-group leader's identity on disk so that a
// freshly started daemon can reap orphaned subprocesses left behind by a
// crashed predecessor. The file is written after the subprocess starts and
// deleted after its group is reaped.
//
// Two kinds of leader are recorded: managed servers (opencode, rovodev), whose
// Port is set, and native agent leaders (claude, codex, copilot, pi, acp:*),
// whose Port is zero. Both are group leaders via Setpgid, which is what makes
// a single group kill on the recorded PID sufficient to reap the whole tree
// the leader spawned.
type ServerPIDInfo struct {
	PID            int       `json:"pid"`
	Owner          string    `json:"owner,omitempty"`
	OwnerPID       int       `json:"owner_pid,omitempty"`
	OwnerStartedAt time.Time `json:"owner_started_at,omitempty"`
	Agent          string    `json:"agent"`
	Bin            string    `json:"bin"`
	Port           int       `json:"port"`
	StartedAt      time.Time `json:"started_at"`
}

const (
	ServerPIDOwnerDaemon = "daemon"
	ServerPIDOwnerWizard = "wizard"
)

var (
	serverPIDsDirMu  sync.RWMutex
	serverPIDsDir    string
	serverPIDOwner   string
	processStartedAt = time.Now().UTC()

	renameServerPIDFile       = os.Rename
	sleepServerPIDRenameRetry = func() { time.Sleep(2 * time.Millisecond) }
	isTransientPIDRenameError = isTransientPIDReplaceError
)

// SetServerPIDsDir configures where managed-server PID files are written.
// Callers (typically the daemon at startup) should point this at
// paths.ServerPIDsDir(). Empty string disables PID tracking, which is the
// default for processes that don't own a long-running daemon identity.
func SetServerPIDsDir(dir string) {
	owner := ""
	if dir != "" {
		owner = ServerPIDOwnerDaemon
	}
	SetServerPIDsDirForOwner(dir, owner)
}

func SetServerPIDsDirForOwner(dir, owner string) {
	serverPIDsDirMu.Lock()
	defer serverPIDsDirMu.Unlock()
	serverPIDsDir = dir
	serverPIDOwner = owner
}

func currentServerPIDsDir() string {
	serverPIDsDirMu.RLock()
	defer serverPIDsDirMu.RUnlock()
	return serverPIDsDir
}

func currentServerPIDOwner() string {
	serverPIDsDirMu.RLock()
	defer serverPIDsDirMu.RUnlock()
	return serverPIDOwner
}

// CurrentServerPIDsDir returns the configured directory for PID tracking
// files, or "" if tracking is disabled.
func CurrentServerPIDsDir() string { return currentServerPIDsDir() }

func CurrentServerPIDOwner() string { return currentServerPIDOwner() }

func CurrentProcessStartedAt() time.Time { return processStartedAt }

// writeServerPIDFile serializes info into a uniquely named file under dir
// and returns the file path. When dir is empty the call is a no-op and the
// empty string is returned so callers can treat "no tracking" uniformly.
// Failures are logged but not surfaced because PID tracking is best-effort
// and shouldn't block a server from starting.
func writeServerPIDFile(dir string, info ServerPIDInfo) string {
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("create server pid dir", "dir", dir, "error", err)
		return ""
	}
	name := fmt.Sprintf("%s-%d.json", pidFileAgentSegment(info.Agent), info.PID)
	path := filepath.Join(dir, name)
	data, err := json.Marshal(info)
	if err != nil {
		slog.Warn("marshal server pid", "error", err)
		return ""
	}
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		slog.Warn("create server pid temp file", "dir", dir, "error", err)
		return ""
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		slog.Warn("write server pid temp file", "path", tmpPath, "error", err)
		return ""
	}
	if err := tmp.Close(); err != nil {
		slog.Warn("close server pid temp file", "path", tmpPath, "error", err)
		return ""
	}
	if err := replaceServerPIDFile(tmpPath, path); err != nil {
		slog.Warn("write server pid file", "path", path, "error", err)
		return ""
	}
	tmpPath = ""
	return path
}

func replaceServerPIDFile(tmpPath, path string) error {
	const maxAttempts = 50

	for attempt := 0; ; attempt++ {
		err := renameServerPIDFile(tmpPath, path)
		if err == nil {
			return nil
		}
		if attempt >= maxAttempts-1 || !isTransientPIDRenameError(err) {
			return err
		}
		sleepServerPIDRenameRetry()
	}
}

// pidFileAgentSegment reduces an agent name to characters that are legal in a
// filename on every supported OS. Agent names are not all bare words - the ACP
// adapter reports "acp:<target>", and ':' cannot appear in a Windows filename,
// so an unsanitized name would silently disable crash recovery for exactly the
// adapters whose names are most structured. The record itself keeps the true
// name; only the filename is folded.
func pidFileAgentSegment(agent string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '_', r == '-':
			return r
		default:
			return '-'
		}
	}, agent)
	if safe == "" {
		return "agent"
	}
	return safe
}

// removeServerPIDFile deletes path, silently ignoring missing files.
func removeServerPIDFile(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("remove server pid file", "path", path, "error", err)
	}
}
