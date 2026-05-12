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
	"context"
	"fmt"
	"os"
	"runtime"

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

// withCoordClient temporarily overrides the docker client for this service to
// point at the compose-coord coordinator socket and calls fn. The original
// client is restored after fn returns. When the project carries no x-engine
// annotations fn is called immediately with no changes.
//
// This is safe because the Up flow is single-threaded: only one create call is
// in flight per composeService at a time.
func (s *composeService) withCoordClient(ctx context.Context, project *types.Project, fn func() error) error {
	if !multi.HasEngineAnnotations(project) {
		return fn()
	}

	coordCli, err := s.clientForProject(ctx, project)
	if err != nil {
		return err
	}

	// Temporarily swap dockerCli so all downstream apiClient() calls go to coord
	origCli := s.dockerCli
	s.dockerCli = &clientOverrideCli{
		Cli:    s.dockerCli,
		apiCli: coordCli,
	}
	defer func() {
		s.dockerCli = origCli
	}()

	return fn()
}

// clientOverrideCli wraps command.Cli and overrides the Client() method so that
// all Docker API calls go through the provided apiCli instead of the default one.
type clientOverrideCli struct {
	command.Cli
	apiCli client.APIClient
}

func (c *clientOverrideCli) Client() client.APIClient {
	return c.apiCli
}

// buildEnginesMap assembles the name→endpoint map that is passed to compose-coord.
// "default" always maps to the local Docker socket, regardless of the active
// context. This prevents a context switch (e.g. "docker offload start") from
// routing all non-annotated services to a non-local engine. Additional entries
// are derived from docker contexts whose names match the x-engine values used
// in the project's services.
func buildEnginesMap(project *types.Project, dockerCli command.Cli) map[string]string {
	engines := map[string]string{
		// Always use the local Docker socket as default, regardless of active context.
		// This prevents the active context (e.g. "offload") from hijacking all
		// services that don't have an x-engine annotation.
		"default": resolveLocalDockerHost(),
	}
	for _, svc := range project.Services {
		engineName := multi.EngineForService(svc)
		if engineName == "" {
			continue
		}
		if _, already := engines[engineName]; already {
			continue
		}
		if endpoint := contextEndpoint(dockerCli, engineName); endpoint != "" {
			engines[engineName] = endpoint
		}
	}
	return engines
}

// resolveLocalDockerHost returns the local Docker daemon socket path,
// independent of the active Docker context.
func resolveLocalDockerHost() string {
	// Respect DOCKER_HOST env var if explicitly set by the user
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	// Default Docker socket path
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
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

// newCoordClient returns a Docker SDK client that speaks to compose-coord over
// the given unix socket path.
func newCoordClient(socketPath string) (client.APIClient, error) {
	return client.New(
		client.WithHost("unix://" + socketPath),
	)
}
