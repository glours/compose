/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package multi

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CoordSocket returns the Docker API address for the coordinator.
// If no coordinator is running for this project it spawns one.
// The coordinator binary must be on PATH as "compose-coord".
func CoordSocket(ctx context.Context, meta *ProjectMeta, engines map[string]string) (string, error) {
	if meta != nil && IsRunning(meta) {
		return meta.CoordSocket, nil
	}
	spawned, err := SpawnCoord(ctx, meta.ProjectName, engines)
	if err != nil {
		return "", err
	}
	return spawned.CoordSocket, nil
}

// IsRunning checks if the coordinator's address is reachable.
// The PID check is intentionally omitted: a stale PID file may point to a
// dead or replaced process, so the socket dial is the authoritative check.
func IsRunning(meta *ProjectMeta) bool {
	if meta == nil || meta.CoordSocket == "" {
		return false
	}
	// Primary check: can we actually reach the coordinator?
	addr := meta.CoordSocket
	network := "tcp"
	if strings.HasPrefix(addr, "unix://") {
		addr = strings.TrimPrefix(addr, "unix://")
		network = "unix"
	} else if strings.HasPrefix(addr, "tcp://") {
		addr = strings.TrimPrefix(addr, "tcp://")
	}
	conn, err := net.DialTimeout(network, addr, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// killExistingCoord kills any running compose-coord process for the given project.
// This prevents coordinator accumulation when project metadata is deleted and
// compose up is re-run.
func killExistingCoord(projectName string) {
	// Find processes matching "compose-coord --project=<name>"
	// Use pgrep or scan /proc on Linux
	out, err := exec.Command("pgrep", "-f", "compose-coord.*--project="+projectName).Output() //nolint:gosec
	if err != nil {
		return // no matching processes
	}
	for _, pidStr := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		_ = proc.Signal(syscall.SIGTERM)
	}
	// Give processes a moment to exit
	time.Sleep(500 * time.Millisecond)
}

// findFreePort finds a free TCP port on localhost by binding to port 0 and
// immediately releasing it.
func findFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

// SpawnCoord starts compose-coord as a background process.
// engines is a map of name→endpoint, e.g.
//
//	{"default": "unix:///var/run/docker.sock", "local-vm": "tcp://192.168.64.10:2375"}
func SpawnCoord(ctx context.Context, projectName string, engines map[string]string) (*ProjectMeta, error) {
	// Kill any existing coordinator for this project to prevent accumulation
	killExistingCoord(projectName)

	port, err := findFreePort()
	if err != nil {
		return nil, fmt.Errorf("finding free port for coordinator: %w", err)
	}
	listenAddr := fmt.Sprintf("127.0.0.1:%d", port)
	coordAddr := fmt.Sprintf("tcp://%s", listenAddr)

	args := []string{
		fmt.Sprintf("--project=%s", projectName),
		fmt.Sprintf("--listen=%s", listenAddr), // TCP host:port, not unix://
	}
	for name, endpoint := range engines {
		args = append(args, fmt.Sprintf("--engine=%s=%s", name, endpoint))
	}

	var stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "compose-coord", args...) //nolint:gosec
	// Detach from this process group so the coordinator survives the CLI exiting
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Write coord logs to a temp file for debugging (stdout + stderr)
	logPath := fmt.Sprintf("/tmp/compose-coord-%s.log", projectName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile // capture stderr too (slog writes to stderr by default)
		defer func() { _ = logFile.Close() }()
	} else {
		cmd.Stderr = &stderrBuf // fallback: capture stderr for error reporting
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting compose-coord: %w", err)
	}

	meta := &ProjectMeta{
		ProjectName: projectName,
		CoordSocket: coordAddr, // e.g. tcp://127.0.0.1:54321
		CoordPID:    cmd.Process.Pid,
	}
	if err := SaveMeta(meta); err != nil {
		// Non-fatal: coordinator is still running
		_ = err
	}

	// Wait up to 10 seconds for the coordinator to be ready
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := WaitForReady(waitCtx, coordAddr); err != nil {
		// Include stderr in error message for debuggability (used when log file creation failed)
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return nil, fmt.Errorf("coordinator did not become ready: %w\ncoord stderr: %s", err, stderr)
		}
		return nil, fmt.Errorf("coordinator did not become ready: %w (logs: %s)", err, logPath)
	}

	return meta, nil
}

// StopCoord sends SIGTERM to the coordinator process for the given project and
// removes the project metadata file. It is a best-effort cleanup call and is
// called by the Down flow after all containers and networks have been removed.
func StopCoord(projectName string) error {
	meta, err := LoadMeta(projectName)
	if err != nil {
		// No metadata file — coordinator was never started or already cleaned up.
		return nil
	}

	// Send SIGTERM to the coordinator process.
	if meta.CoordPID > 0 {
		if proc, findErr := os.FindProcess(meta.CoordPID); findErr == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
	}

	// Also kill by name in case the PID is stale.
	killExistingCoord(projectName)

	// Remove project metadata file.
	return DeleteMeta(projectName)
}

// WaitForReady polls the coord address until /_ping returns 200 or ctx is cancelled.
// coordAddr may be "tcp://host:port" or "unix:///path/to/socket".
func WaitForReady(ctx context.Context, coordAddr string) error {
	var pingURL string
	var transport *http.Transport

	if strings.HasPrefix(coordAddr, "tcp://") {
		host := strings.TrimPrefix(coordAddr, "tcp://")
		pingURL = fmt.Sprintf("http://%s/_ping", host)
		transport = &http.Transport{}
	} else {
		socketPath := strings.TrimPrefix(coordAddr, "unix://")
		pingURL = "http://localhost/_ping"
		transport = &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		}
	}

	httpClient := &http.Client{Transport: transport}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for coordinator at %s: %w", coordAddr, ctx.Err())
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, pingURL, http.NoBody)
			if err != nil {
				continue
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}
