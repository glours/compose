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
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// CoordSocket returns the Docker API socket path for the coordinator.
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

// IsRunning checks if the coord process is alive and its socket is reachable.
func IsRunning(meta *ProjectMeta) bool {
	if meta == nil || meta.CoordSocket == "" {
		return false
	}
	// Check if the process is still alive (best-effort: PID 0 means external)
	if meta.CoordPID > 0 {
		proc, err := os.FindProcess(meta.CoordPID)
		if err != nil {
			return false
		}
		// Signal 0 checks if the process exists without sending an actual signal
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return false
		}
	}
	// Verify the socket is accessible
	conn, err := net.DialTimeout("unix", meta.CoordSocket, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// SpawnCoord starts compose-coord as a background process.
// engines is a map of name→endpoint, e.g.
//
//	{"default": "unix:///var/run/docker.sock", "local-vm": "tcp://192.168.64.10:2375"}
func SpawnCoord(ctx context.Context, projectName string, engines map[string]string) (*ProjectMeta, error) {
	socketPath := fmt.Sprintf("/tmp/compose-coord-%s.sock", projectName)

	// Remove stale socket if present
	_ = os.Remove(socketPath)

	args := []string{
		fmt.Sprintf("--project=%s", projectName),
		fmt.Sprintf("--listen=unix://%s", socketPath),
	}
	for name, endpoint := range engines {
		args = append(args, fmt.Sprintf("--engine=%s=%s", name, endpoint))
	}

	cmd := exec.CommandContext(ctx, "compose-coord", args...) //nolint:gosec
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Detach from this process group so the coordinator survives the CLI exiting
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting compose-coord: %w", err)
	}

	meta := &ProjectMeta{
		ProjectName: projectName,
		CoordSocket: socketPath,
		CoordPID:    cmd.Process.Pid,
	}
	if err := SaveMeta(meta); err != nil {
		// Non-fatal: coordinator is still running
		_ = err
	}

	// Wait up to 10 seconds for the coordinator to be ready
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := WaitForReady(waitCtx, socketPath); err != nil {
		return nil, fmt.Errorf("coordinator did not become ready: %w", err)
	}

	return meta, nil
}

// WaitForReady polls the coord socket until /_ping returns 200 or ctx is cancelled.
func WaitForReady(ctx context.Context, socketPath string) error {
	transport := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}
	httpClient := &http.Client{Transport: transport}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for coordinator socket %s: %w", socketPath, ctx.Err())
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/_ping", http.NoBody)
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
