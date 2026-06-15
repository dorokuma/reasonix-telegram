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
	stale := findStaleReasonixServePIDs(bridgePID, a.cfg.ReasonixBin)
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

func findStaleReasonixServePIDs(bridgePID int, reasonixBin string) map[int]struct{} {
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
		if !isReasonixServeCmd(cmd, reasonixBin) {
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

func isReasonixServeCmd(cmd string, reasonixBin string) bool {
	// cmd is read from /proc/PID/cmdline with nulls replaced by spaces.
	// The first token is the binary path; the rest are arguments.
	cmd = strings.TrimSpace(cmd)
	parts := strings.Fields(cmd)
	if len(parts) < 2 {
		return false
	}
	bin := parts[0]
	// Check the binary path ends with the expected name (or /name).
	// Then verify the first argument is "serve".
	return (strings.HasSuffix(bin, "/"+reasonixBin) || bin == reasonixBin) && parts[1] == "serve"
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
	// Format: pid (comm) state ppid ... — comm can contain spaces/parens.
	s := string(b)
	if idx := strings.LastIndex(s, ")"); idx >= 0 {
		fields := strings.Fields(s[idx+1:])
		if len(fields) >= 2 {
			ppid, _ := strconv.Atoi(fields[1])
			return ppid
		}
	}
	return 0
}

func pidsListeningOnTCPPort(port int) []int {
	out, err := exec.Command("ss", "-tlnp", fmt.Sprintf("sport = :%d", port)).CombinedOutput()
	if err != nil {
		return nil
	}
	return parseSSPIDs(out)
}

func terminateProcessGroup(pid int, timeout time.Duration) {
	// Snapshot the process start time from /proc before signalling, so we can
	// detect PID reuse if the process dies and the kernel recycles the PID.
	startTime := readProcStartTime(pid)
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		// Verify the PID hasn't been recycled: the process at pid should still
		// have the same start_time as when we started.
		if readProcStartTime(pid) != startTime {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	for i := 0; i < 20; i++ {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if readProcStartTime(pid) != startTime {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readProcStartTime returns the start_time field (field 22) from /proc/PID/stat.
// This value is monotonically increasing per PID and changes only when the PID
// is recycled, so it can be used to detect PID reuse.
func readProcStartTime(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	// comm can contain spaces and parens; start_time is field 22 (0-indexed: 21).
	// Skip the comm field (enclosed in parens) to get stable field positions.
	s := string(b)
	commEnd := strings.LastIndex(s, ")")
	if commEnd < 0 {
		return ""
	}
	fields := strings.Fields(s[commEnd+1:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19] // start_time is field 22 (0-based index 21, after 2 fields before comm)
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

