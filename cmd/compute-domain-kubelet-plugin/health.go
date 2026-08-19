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
	"fmt"
	"net"
	"net/url"
	"strconv"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"

	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/common"
)

type healthcheck struct {
	grpc_health_v1.UnimplementedHealthServer

	server *grpc.Server
	wg     sync.WaitGroup

	regClient registerapi.RegistrationClient
	draClient drapb.DRAPluginClient
}

func setupHealthcheckPrimitives(ctx context.Context, config *Config) (*healthcheck, error) {
	port := config.flags.healthcheckPort
	if port < 0 {
		return nil, nil
	}

	// Bind on all available interfaces.
	addr := net.JoinHostPort("", strconv.Itoa(port))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	rollingUpdatePodUID := config.RollingUpdatePodUID()

	regSockPath := (&url.URL{
		Scheme: "unix",
		Path:   common.RegistrarSocketPath(config.flags.kubeletRegistrarDirectoryPath, DriverName, rollingUpdatePodUID),
	}).String()

	klog.V(6).Infof("Connect to registration socket at %s", regSockPath)
	regConn, err := grpc.NewClient(
		regSockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("error connecting to registration socket: %w", err)
	}

	draSockPath := (&url.URL{
		Scheme: "unix",
		Path:   common.DRASocketPath(config.DriverPluginPath(), rollingUpdatePodUID),
	}).String()

	klog.V(6).Infof("Connect to plugin socket at %s", draSockPath)
	draConn, err := grpc.NewClient(
		draSockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("error connecting to plugin socket: %w", err)
	}

	server := grpc.NewServer()
	healthcheck := &healthcheck{
		server:    server,
		regClient: registerapi.NewRegistrationClient(regConn),
		draClient: drapb.NewDRAPluginClient(draConn),
	}
	grpc_health_v1.RegisterHealthServer(server, healthcheck)

	healthcheck.wg.Add(1)
	go func() {
		defer healthcheck.wg.Done()
		klog.Infof("Starting healthcheck server on %s", lis.Addr().String())
		if err := server.Serve(lis); err != nil {
			// Note(JP): let's review if this should be fatal
			klog.Errorf("failed to start healthcheck server: %v", err)
		}
	}()

	return healthcheck, nil
}

func (h *healthcheck) Stop() {
	if h.server != nil {
		klog.Info("Stopping healthcheck server")
		h.server.GracefulStop()
	}
	h.wg.Wait()
}

// Check implements [grpc_health_v1.HealthServer.Check].
func (h *healthcheck) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	knownServices := map[string]struct{}{"": {}, "liveness": {}}
	if _, known := knownServices[req.GetService()]; !known {
		return nil, status.Error(codes.NotFound, "unknown service")
	}

	status := &grpc_health_v1.HealthCheckResponse{
		Status: grpc_health_v1.HealthCheckResponse_NOT_SERVING,
	}

	// This simulates the kubelet reaching out to the plugin for discovery
	// (towards registering it).
	info, err := h.regClient.GetInfo(ctx, &registerapi.InfoRequest{})
	if err != nil {
		klog.ErrorS(err, "failed to call GetInfo on registration socket")
		return status, nil
	}
	klog.V(7).Infof("Successfully invoked GetInfo: %v", info)

	// This simulates the kubelet reaching out to the plugin
	_, err = h.draClient.NodePrepareResources(ctx, &drapb.NodePrepareResourcesRequest{})
	if err != nil {
		klog.ErrorS(err, "failed to call NodePrepareResources on plugin socket")
		return status, nil
	}
	klog.V(7).Info("Successfully invoked NodePrepareResources")

	status.Status = grpc_health_v1.HealthCheckResponse_SERVING
	return status, nil
}
