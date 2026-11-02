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

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	resourceapi "k8s.io/api/resource/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	drahealthv1 "k8s.io/kubelet/pkg/apis/dra-health/v1"
	drahealthv1alpha1 "k8s.io/kubelet/pkg/apis/dra-health/v1alpha1"
)

// These tests run the driver behind the real kubeletplugin helper and talk to
// it the way the kubelet does: over the DRA unix socket, through the
// versioned DRAResourceHealth gRPC API. They cover the helper's bridge from
// WatchHealthStatus to both the v1 and the v1alpha1 stream.

// startHelperForHealth starts the kubeletplugin helper for the driver with
// the same health service predicate NewDriver uses, and returns a client
// connection to its DRA socket.
func startHelperForHealth(t *testing.T, d *driver) *grpc.ClientConn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()

	helper, err := kubeletplugin.Start(ctx, d,
		kubeletplugin.KubeClient(k8sfake.NewSimpleClientset()),
		kubeletplugin.NodeName("node-a"),
		kubeletplugin.DriverName(DriverName),
		kubeletplugin.RegistrarDirectoryPath(dir),
		kubeletplugin.PluginDataDirectoryPath(dir),
		kubeletplugin.HealthService(d.deviceHealthMonitor != nil),
	)
	require.NoError(t, err)

	conn, err := grpc.NewClient("unix:"+filepath.Join(dir, "dra.sock"),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = conn.Close()
		cancel()
		helper.Stop()
	})
	return conn
}

func recvV1(t *testing.T, stream grpc.ServerStreamingClient[drahealthv1.NodeWatchResourcesResponse]) *drahealthv1.NodeWatchResourcesResponse {
	t.Helper()
	type result struct {
		resp *drahealthv1.NodeWatchResourcesResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := stream.Recv()
		ch <- result{resp, err}
	}()
	select {
	case r := <-ch:
		require.NoError(t, r.err)
		return r.resp
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a health message from the helper")
		return nil
	}
}

func v1HealthByDevice(resp *drahealthv1.NodeWatchResourcesResponse) map[string]drahealthv1.HealthStatus {
	out := make(map[string]drahealthv1.HealthStatus, len(resp.Devices))
	for _, dev := range resp.Devices {
		out[dev.Device.DeviceName] = dev.Health
	}
	return out
}

func TestWatchHealthStatus_V1StreamThroughHelper(t *testing.T) {
	d, devices := newTestHealthDriver(t)
	conn := startHelperForHealth(t, d)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := drahealthv1.NewDRAResourceHealthClient(conn).NodeWatchResources(ctx, &drahealthv1.NodeWatchResourcesRequest{})
	require.NoError(t, err)

	// Initial message: every device in the node's pool, all healthy.
	initial := recvV1(t, stream)
	require.Len(t, initial.Devices, 2)
	for _, dev := range initial.Devices {
		assert.Equal(t, "node-a", dev.Device.PoolName)
		assert.NotZero(t, dev.LastUpdatedTime)
	}
	assert.Equal(t, map[string]drahealthv1.HealthStatus{
		"gpu-0": drahealthv1.HealthStatus_HEALTHY,
		"gpu-1": drahealthv1.HealthStatus_HEALTHY,
	}, v1HealthByDevice(initial))

	// A fatal XID applied the way the NVML event consumer does it shows up
	// on the stream without waiting for the periodic resend.
	d.state.AddDeviceTaint(devices["gpu-0"], &resourceapi.DeviceTaint{
		Key: TaintKeyXID, Value: "79", Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := recvV1(t, stream)
		if v1HealthByDevice(resp)["gpu-0"] == drahealthv1.HealthStatus_UNHEALTHY {
			for _, dev := range resp.Devices {
				if dev.Device.DeviceName == "gpu-0" {
					assert.Equal(t, "fatal XID 79 reported by NVML", dev.Message)
				}
			}
			assert.Equal(t, drahealthv1.HealthStatus_HEALTHY, v1HealthByDevice(resp)["gpu-1"])
			break
		}
		require.True(t, time.Now().Before(deadline), "stream never reported gpu-0 as unhealthy")
	}

	// The stream keeps flowing while nothing changes (periodic resend).
	again := recvV1(t, stream)
	assert.Equal(t, drahealthv1.HealthStatus_UNHEALTHY, v1HealthByDevice(again)["gpu-0"])
}

func TestWatchHealthStatus_V1alpha1StreamThroughHelper(t *testing.T) {
	d, devices := newTestHealthDriver(t)
	d.state.AddDeviceTaint(devices["gpu-1"], &resourceapi.DeviceTaint{
		Key: TaintKeyUnmonitored, Effect: resourceapi.DeviceTaintEffectNone,
	})
	conn := startHelperForHealth(t, d)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := drahealthv1alpha1.NewDRAResourceHealthClient(conn).NodeWatchResources(ctx, &drahealthv1alpha1.NodeWatchResourcesRequest{})
	require.NoError(t, err)

	resp, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Devices, 2)
	got := map[string]drahealthv1alpha1.HealthStatus{}
	for _, dev := range resp.Devices {
		assert.Equal(t, "node-a", dev.Device.PoolName)
		got[dev.Device.DeviceName] = dev.Health
	}
	assert.Equal(t, map[string]drahealthv1alpha1.HealthStatus{
		"gpu-0": drahealthv1alpha1.HealthStatus_HEALTHY,
		"gpu-1": drahealthv1alpha1.HealthStatus_UNKNOWN,
	}, got)
}

// Without the NVML health monitor the service is not advertised, and a stray
// subscription is answered with Unimplemented so the kubelet stops watching.
func TestWatchHealthStatus_UnsupportedThroughHelper(t *testing.T) {
	d, _ := newTestHealthDriver(t)
	d.deviceHealthMonitor = nil
	conn := startHelperForHealth(t, d)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := drahealthv1.NewDRAResourceHealthClient(conn).NodeWatchResources(ctx, &drahealthv1.NodeWatchResourcesRequest{})
	require.NoError(t, err)

	_, err = stream.Recv()
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}
