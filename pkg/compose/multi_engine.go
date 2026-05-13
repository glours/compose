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

package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	dockercontext "github.com/docker/cli/cli/context/docker"
	"github.com/moby/moby/client"

	"github.com/docker/compose/v5/pkg/compose/multi"
)

// clientForProject returns a Docker API client suitable for the given project.
// When the project has no x-engine annotations the standard dockerCli client is
// returned unchanged (zero-impact on the single-engine code path).
// When x-engine annotations are present the method ensures a compose-coord
// coordinator is running and returns a client pointed at its unix socket.
func (s *composeService) clientForProject(ctx context.Context, project *types.Project) (client.APIClient, error) {
	if !multi.HasEngineAnnotations(project) {
		return s.dockerCli.Client(), nil
	}

	// Load or create coordinator metadata
	meta, err := multi.LoadMeta(project.Name)
	if err != nil || !multi.IsRunning(meta) {
		engines := buildEnginesMap(project, s.dockerCli)
		meta, err = multi.SpawnCoord(ctx, project.Name, engines)
		if err != nil {
			return nil, fmt.Errorf("starting coordinator for project %q: %w", project.Name, err)
		}
	}

	return newCoordClient(meta.CoordSocket)
}

// initCoordClient ensures s.coordClient is initialised for the given project.
// When the project carries no x-engine annotations this is a no-op.
// The client is stored on the composeService and reused for subsequent calls;
// it is safe for concurrent reads once set (it is set exactly once at the start
// of the Create/Up flow, before any per-service goroutines are spawned).
func (s *composeService) initCoordClient(ctx context.Context, project *types.Project) error {
	if !multi.HasEngineAnnotations(project) {
		return nil
	}
	coordCli, err := s.clientForProject(ctx, project)
	if err != nil {
		return err
	}
	s.coordClient = coordCli

	// Send the project service→engine map to the coordinator so it can route
	// image pulls and container creates to the correct engine even when the
	// com.docker.compose.engine label is absent (e.g. during image pull).
	return s.sendProjectConfig(ctx, project)
}

// sendProjectConfig posts the service→engine routing map for the project to
// the coordinator's POST /compose/project endpoint.  This is a best-effort
// call: if the coordinator is not running or unreachable the error is logged
// and swallowed so the normal compose flow is not interrupted.
func (s *composeService) sendProjectConfig(ctx context.Context, project *types.Project) error {
	meta, err := multi.LoadMeta(project.Name)
	if err != nil || !multi.IsRunning(meta) {
		// Coordinator not running or metadata missing — skip silently.
		return nil
	}

	// Build the service→engine map.
	services := make(map[string]string, len(project.Services))
	for name, svc := range project.Services {
		engine := multi.EngineForService(svc)
		if engine == "" {
			engine = "default"
		}
		services[name] = engine
	}

	payload, err := json.Marshal(map[string]any{
		"project":  project.Name,
		"services": services,
	})
	if err != nil {
		return fmt.Errorf("marshalling project config: %w", err)
	}

	// The coordinator listens on a tcp:// address; strip the scheme for use in
	// an http:// URL.
	host := strings.TrimPrefix(meta.CoordSocket, "tcp://")
	url := fmt.Sprintf("http://%s/compose/project", host)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("building project config request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending project config to coordinator: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	return nil
}

// apiClientForService returns the Docker API client to use when creating or
// starting a container for the given service.
//
// Services annotated with x-engine are routed through the coordinator so that
// the coordinator can place the container on the correct engine. All other
// services (including provider services which have no containers at all) use
// the standard Docker client.
func (s *composeService) apiClientForService(service types.ServiceConfig) client.APIClient {
	if s.coordClient != nil && multi.EngineForService(service) != "" {
		return s.coordClient
	}
	return s.apiClient()
}

// buildEnginesMap assembles the name→endpoint map that is passed to compose-coord.
// "default" maps to the endpoint of the currently active Docker context, exactly
// as plain compose up behaves. The coordinator will update this to the correct
// local endpoint via POST /compose/engines once the provider captures it.
// Additional entries are derived from docker contexts whose names match the
// x-engine values used in the project's services.
func buildEnginesMap(project *types.Project, dockerCli command.Cli) map[string]string {
	engines := map[string]string{
		"default": dockerCli.Client().DaemonHost(),
	}
	for _, svc := range project.Services {
		if engine := multi.EngineForService(svc); engine != "" {
			if endpoint := contextEndpoint(dockerCli, engine); endpoint != "" {
				engines[engine] = endpoint
			}
		}
	}
	return engines
}

// contextEndpoint looks up a docker context by name and returns its host endpoint,
// or "" if the context cannot be found.
func contextEndpoint(dockerCli command.Cli, contextName string) string {
	st := dockerCli.ContextStore()
	if st == nil {
		return ""
	}
	meta, err := st.GetMetadata(contextName)
	if err != nil {
		return ""
	}
	epMeta, err := dockercontext.EndpointFromContext(meta)
	if err != nil {
		return ""
	}
	return epMeta.Host
}

// newCoordClient returns a Docker SDK client that speaks to compose-coord at
// the given address. coordAddr may be "tcp://host:port" or "unix:///path".
func newCoordClient(coordAddr string) (client.APIClient, error) {
	return client.New(
		client.WithHost(coordAddr),
	)
}
