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
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	dockercontext "github.com/docker/cli/cli/context/docker"
	"github.com/docker/cli/cli/context/store"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	"github.com/docker/compose/v5/pkg/mocks"
)

// stubContextStore is a minimal store.Store that returns a single named context
// with the given endpoint host.
type stubContextStore struct {
	contextName string
	host        string
}

func (s *stubContextStore) GetMetadata(name string) (store.Metadata, error) {
	if name != s.contextName {
		return store.Metadata{}, &stubNotFound{name: name}
	}
	return store.Metadata{
		Endpoints: map[string]any{
			dockercontext.DockerEndpoint: dockercontext.EndpointMeta{Host: s.host},
		},
	}, nil
}

func (s *stubContextStore) List() ([]store.Metadata, error) { return nil, nil }

func (s *stubContextStore) ListTLSFiles(name string) (map[string]store.EndpointFiles, error) {
	return nil, nil
}

func (s *stubContextStore) GetTLSData(contextName, endpointName, fileName string) ([]byte, error) {
	return nil, nil
}

func (s *stubContextStore) GetStorageInfo(contextName string) store.StorageInfo {
	return store.StorageInfo{}
}
func (s *stubContextStore) CreateOrUpdate(meta store.Metadata) error { return nil }
func (s *stubContextStore) Remove(name string) error                 { return nil }
func (s *stubContextStore) ResetTLSMaterial(name string, data *store.ContextTLSData) error {
	return nil
}

func (s *stubContextStore) ResetEndpointTLSMaterial(contextName string, endpointName string, data *store.EndpointTLSData) error {
	return nil
}

type stubNotFound struct{ name string }

func (e *stubNotFound) Error() string { return "context not found: " + e.name }

// projectWithXEngine returns a minimal project with one service that has an
// x-engine annotation pointing to the given engine name.
func projectWithXEngine(serviceName, engineName string) *types.Project {
	return &types.Project{
		Services: types.Services{
			serviceName: {
				Name: serviceName,
				Extensions: map[string]any{
					"x-engine": engineName,
				},
			},
		},
	}
}

// TestBuildEnginesMapDefaultIsAlwaysLocal verifies that the "default" engine in
// the map always resolves to the local Docker socket, regardless of what the
// active context's DaemonHost() would return. This guards against the bug where
// "docker offload start" switches the active context to "offload" and causes all
// non-annotated services to run on the offload engine instead of locally.
func TestBuildEnginesMapDefaultIsAlwaysLocal(t *testing.T) {
	// Ensure DOCKER_HOST is not set so we get the hard-coded default socket.
	t.Setenv("DOCKER_HOST", "")

	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	cli := mocks.NewMockCli(mockCtrl)
	// The offload context returns the offload endpoint when its host is queried.
	cli.EXPECT().ContextStore().Return(&stubContextStore{
		contextName: "offload",
		host:        "tcp://offload-engine:2375",
	}).AnyTimes()

	engines := buildEnginesMap(projectWithXEngine("db", "offload"), cli)

	// default must always be the local Unix socket, not the active context endpoint.
	assert.Equal(t, "unix:///var/run/docker.sock", engines["default"])
	// The offload context should be resolved from the context store.
	assert.Equal(t, "tcp://offload-engine:2375", engines["offload"])
}

// TestBuildEnginesMapRespectsDockerHostEnv verifies that when DOCKER_HOST is
// explicitly set, the default engine uses that value rather than the hard-coded
// socket path.
func TestBuildEnginesMapRespectsDockerHostEnv(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://custom-host:2376")

	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	cli := mocks.NewMockCli(mockCtrl)
	cli.EXPECT().ContextStore().Return((*stubContextStore)(nil)).AnyTimes()

	engines := buildEnginesMap(&types.Project{}, cli)

	assert.Equal(t, "tcp://custom-host:2376", engines["default"])
}

// TestBuildEnginesMapNoXEngine verifies that a project with no x-engine
// annotations still gets a valid "default" entry.
func TestBuildEnginesMapNoXEngine(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")

	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	// ContextStore should not be called when there are no x-engine annotations.
	cli := mocks.NewMockCli(mockCtrl)

	engines := buildEnginesMap(&types.Project{
		Services: types.Services{
			"web": {Name: "web"},
		},
	}, cli)

	assert.Equal(t, "unix:///var/run/docker.sock", engines["default"])
	_, hasOffload := engines["offload"]
	assert.Assert(t, !hasOffload)
}
