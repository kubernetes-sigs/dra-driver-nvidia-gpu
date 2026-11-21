/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type eventRequest struct {
	method string
	path   string
	event  *corev1.Event
}

func TestEmitCheckpointCorruptionEvent(t *testing.T) {
	requests := make(chan eventRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		event := &corev1.Event{}
		if err := json.NewDecoder(r.Body).Decode(event); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		requests <- eventRequest{method: r.Method, path: r.URL.Path, event: event}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(event); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)

	client, err := kubernetes.NewForConfig(&rest.Config{
		Host: server.URL,
		ContentConfig: rest.ContentConfig{
			ContentType:        "application/json",
			AcceptContentTypes: "application/json",
		},
	})
	require.NoError(t, err)

	EmitCheckpointCorruptionEvent(
		context.Background(),
		client,
		"gpu.nvidia.com",
		"node-a",
		"gpu-kubelet-plugin-abcde",
		"dra-driver-nvidia-gpu",
		"Corrupt checkpoint detected (checksum verification failed)",
	)

	var request eventRequest
	select {
	case request = <-requests:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Event API request")
	}
	assert.Equal(t, http.MethodPost, request.method)
	assert.Equal(t, "/api/v1/namespaces/dra-driver-nvidia-gpu/events", request.path)

	event := request.event
	assert.Equal(t, "gpu-kubelet-plugin-abcde-checkpoint-corrupted", event.Name)
	assert.Equal(t, "dra-driver-nvidia-gpu", event.Namespace)
	assert.Equal(t, corev1.EventTypeWarning, event.Type)
	assert.Equal(t, "CheckpointCorrupted", event.Reason)
	assert.Equal(t, "Corrupt checkpoint detected (checksum verification failed)", event.Message)
	assert.Equal(t, int32(1), event.Count)
	assert.Equal(t, "gpu.nvidia.com", event.Source.Component)
	assert.Equal(t, "node-a", event.Source.Host)
	assert.Equal(t, "v1", event.InvolvedObject.APIVersion)
	assert.Equal(t, "Pod", event.InvolvedObject.Kind)
	assert.Equal(t, "gpu-kubelet-plugin-abcde", event.InvolvedObject.Name)
	assert.Equal(t, "dra-driver-nvidia-gpu", event.InvolvedObject.Namespace)
}
