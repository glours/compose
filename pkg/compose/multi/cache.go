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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ProjectMeta holds coordinator metadata for a compose project.
type ProjectMeta struct {
	ProjectName string `json:"project_name"`
	CoordSocket string `json:"coord_socket"` // unix socket path, e.g. /tmp/compose-coord-myapp.sock
	CoordPID    int    `json:"coord_pid"`    // PID of coord process (0 if external)
}

// MetaPath returns the path to the metadata file for the given project name
// (~/.docker/compose-mesh/projects/<name>.json).
func MetaPath(projectName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".docker", "compose-mesh", "projects", projectName+".json"), nil
}

// LoadMeta loads the coordinator metadata for the given project, or returns an
// error if the file does not exist or cannot be parsed.
func LoadMeta(projectName string) (*ProjectMeta, error) {
	path, err := MetaPath(projectName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta ProjectMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parsing coordinator metadata: %w", err)
	}
	return &meta, nil
}

// SaveMeta persists coordinator metadata to disk, creating parent directories
// as needed.
func SaveMeta(meta *ProjectMeta) error {
	path, err := MetaPath(meta.ProjectName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating metadata directory: %w", err)
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling coordinator metadata: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// DeleteMeta removes the coordinator metadata file for the given project.
// It is not an error if the file does not exist.
func DeleteMeta(projectName string) error {
	path, err := MetaPath(projectName)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
