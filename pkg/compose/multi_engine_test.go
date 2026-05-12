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

// TestBuildEnginesMapDefaultIsCurrentContext verifies that the "default" engine
// in the map is whatever the active Docker context's DaemonHost() returns.
// Services without x-engine run on the currently active context, exactly like
// regular compose up — no special-casing, no hardcoded socket paths.
func TestBuildEnginesMapDefaultIsCurrentContext(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	const currentContextEndpoint = "tcp://current-context:2375"

	mockAPIClient := mocks.NewMockAPIClient(mockCtrl)
	mockAPIClient.EXPECT().DaemonHost().Return(currentContextEndpoint).AnyTimes()

	cli := mocks.NewMockCli(mockCtrl)
	cli.EXPECT().Client().Return(mockAPIClient).AnyTimes()
	cli.EXPECT().ContextStore().Return(&stubContextStore{
		contextName: "offload",
		host:        "tcp://offload-engine:2375",
	}).AnyTimes()

	engines := buildEnginesMap(projectWithXEngine("db", "offload"), cli)

	// default must be whatever the active context's DaemonHost() returns.
	assert.Equal(t, currentContextEndpoint, engines["default"])
	// The offload context should be resolved from the context store.
	assert.Equal(t, "tcp://offload-engine:2375", engines["offload"])
}

// TestBuildEnginesMapNoXEngine verifies that a project with no x-engine
// annotations still gets a valid "default" entry sourced from the active context.
func TestBuildEnginesMapNoXEngine(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	const currentContextEndpoint = "unix:///var/run/docker.sock"

	mockAPIClient := mocks.NewMockAPIClient(mockCtrl)
	mockAPIClient.EXPECT().DaemonHost().Return(currentContextEndpoint).AnyTimes()

	cli := mocks.NewMockCli(mockCtrl)
	cli.EXPECT().Client().Return(mockAPIClient).AnyTimes()

	engines := buildEnginesMap(&types.Project{
		Services: types.Services{
			"web": {Name: "web"},
		},
	}, cli)

	assert.Equal(t, currentContextEndpoint, engines["default"])
	_, hasOffload := engines["offload"]
	assert.Assert(t, !hasOffload)
}
