package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var ssPIDRe = regexp.MustCompile(`pid=(\d+)`)

// cleanupStaleServesOnStartup stops reasonix serve processes left behind when
// the bridge restarts. serve is started with Setpgid, so systemd restart does
// not reap the child automatically.
func (a *App) cleanupStaleServesOnStartup() {
	bridgePID := os.Getpid()
	stale := findStaleReasonixServePIDs(bridgePID)
	ports := persistedServePorts(a.state)

	// Also reclaim listeners on persisted chat ports (covers cmdline renames).
	for port := range ports {
		for _, pid := range pidsListeningOnTCPPort(port) {
			if pid == bridgePID {
				continue
			}
			stale[pid] = struct{}{}
		}
	}

	if len(stale) == 0 {
		return
	}
	for pid := range stale {
		log.Printf("startup: stopping stale reasonix serve pid=%d", pid)
		terminateProcessGroup(pid, 8*time.Second)
	}
}

func persistedServePorts(st *stateStore) map[int]struct{} {
	out := map[int]struct{}{}
	records, err := st.load()
	if err != nil {
		return out
	}
	for _, r := range records {
		port := r.Port
		if port == 0 {
			port = portForChat(r.ChatID)
		}
		out[port] = struct{}{}
	}
	return out
}

func findStaleReasonixServePIDs(bridgePID int) map[int]struct{} {
	out := map[int]struct{}{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, ent := range entries {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 1 || pid == bridgePID {
			continue
		}
		cmd := readProcCmdline(pid)
		if !isReasonixServeCmd(cmd) {
			continue
		}
		ppid := readProcPPID(pid)
		if ppid == bridgePID {
			continue
		}
		out[pid] = struct{}{}
	}
	return out
}

func isReasonixServeCmd(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	return strings.Contains(cmd, "reasonix") && strings.Contains(cmd, "serve")
}

func readProcCmdline(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(b), "\x00", " ")
}

func readProcPPID(pid int) int {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	// comm can contain spaces/parens; fields after comm are stable.
	fields := strings.Fields(string(b))
	if len(fields) < 4 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[3])
	return ppid
}

func pidsListeningOnTCPPort(port int) []int {
	out, err := exec.Command("ss", "-tlnp", fmt.Sprintf("sport = :%d", port)).CombinedOutput()
	if err != nil {
		return nil
	}
	return parseSSPIDs(out)
}

func terminateProcessGroup(pid int, timeout time.Duration) {
	// serve uses Setpgid with pgid == pid.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	for i := 0; i < 20; i++ {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// parseSSPIDs is exported to tests for ss output parsing.
func parseSSPIDs(out []byte) []int {
	seen := map[int]struct{}{}
	var pids []int
	for _, m := range ssPIDRe.FindAllSubmatch(out, -1) {
		pid, err := strconv.Atoi(string(m[1]))
		if err != nil || pid <= 1 {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	return pids
}

