/*
Copyright The Kubernetes Authors.

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

package main

import (
	"context"
	"fmt"
	"slices"

	resourceapi "k8s.io/api/resource/v1"
	resourcev1beta2 "k8s.io/api/resource/v1beta2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientset "k8s.io/client-go/kubernetes"
	resourcev1client "k8s.io/client-go/kubernetes/typed/resource/v1"
	resourcev1beta2client "k8s.io/client-go/kubernetes/typed/resource/v1beta2"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

const (
	// deviceTaintRuleKindLabelKey tells apart the DeviceTaintRules the
	// controller manages.
	deviceTaintRuleKindLabelKey = "resource.nvidia.com/computeDomainTaintRule"
	deviceTaintRuleKindChannel  = "Channel"
)

func channelTaintRuleName(uid string, channel int) string {
	return fmt.Sprintf("computedomain-%s-channel-%d", uid, channel)
}

// EnsureChannelTaintRules taints a host-managed ComputeDomain's reserved
// channels on all nodes, so that only its claims, which tolerate the taint,
// can be allocated them. Existing rules are kept, so this is safe to repeat.
// The legacy shared channel is never tainted.
func (m *ChannelReservationManager) EnsureChannelTaintRules(ctx context.Context, cd *nvapi.ComputeDomain) error {
	uid := string(cd.UID)
	for _, channel := range cd.Status.Channels {
		if channel == legacySharedChannel {
			continue
		}
		rule := &resourceapi.DeviceTaintRule{
			ObjectMeta: metav1.ObjectMeta{
				Name: channelTaintRuleName(uid, channel),
				Labels: map[string]string{
					computeDomainLabelKey:       uid,
					deviceTaintRuleKindLabelKey: deviceTaintRuleKindChannel,
				},
			},
			Spec: resourceapi.DeviceTaintRuleSpec{
				DeviceSelector: &resourceapi.DeviceTaintSelector{
					Driver: ptr.To(DriverName),
					Device: ptr.To(channelDeviceName(channel)),
				},
				Taint: resourceapi.DeviceTaint{
					Key:    imex.ChannelOwnerTaintKey,
					Value:  uid,
					Effect: resourceapi.DeviceTaintEffectNoSchedule,
				},
			},
		}
		if _, err := m.config.deviceTaintRules.Create(ctx, rule); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("error creating DeviceTaintRule for channel %d: %w", channel, err)
		}
	}
	return nil
}

// DeleteTaintRules deletes all DeviceTaintRules of a ComputeDomain.
func (m *ChannelReservationManager) DeleteTaintRules(ctx context.Context, uid string) error {
	err := m.config.deviceTaintRules.DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", computeDomainLabelKey, uid),
	})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("error deleting DeviceTaintRules: %w", err)
	}
	return nil
}

// deviceTaintRuleClient serves DeviceTaintRules as v1 types from the API
// version picked at startup. The version-adapting DRA client is avoided: its
// fallback (v1beta2 on Kubernetes 1.36) would apply to all resource types,
// and its v1beta2 UpdateStatus panics.
type deviceTaintRuleClient interface {
	Create(ctx context.Context, rule *resourceapi.DeviceTaintRule) (*resourceapi.DeviceTaintRule, error)
	Get(ctx context.Context, name string) (*resourceapi.DeviceTaintRule, error)
	List(ctx context.Context, opts metav1.ListOptions) ([]resourceapi.DeviceTaintRule, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error
}

// newDeviceTaintRuleClient prefers resource.k8s.io/v1 (Kubernetes 1.37+) over
// v1beta2 (Kubernetes 1.36).
func newDeviceTaintRuleClient(clientset coreclientset.Interface) (deviceTaintRuleClient, error) {
	serves := func(groupVersion string) bool {
		resources, err := clientset.Discovery().ServerResourcesForGroupVersion(groupVersion)
		if err != nil {
			klog.V(4).Infof("Discovery of %s failed: %v", groupVersion, err)
			return false
		}
		return slices.ContainsFunc(resources.APIResources, func(r metav1.APIResource) bool {
			return r.Name == "devicetaintrules"
		})
	}

	if serves(resourceapi.SchemeGroupVersion.String()) {
		klog.Infof("Using %s for DeviceTaintRules", resourceapi.SchemeGroupVersion)
		return &v1DeviceTaintRuleClient{client: clientset.ResourceV1().DeviceTaintRules()}, nil
	}
	if serves(resourcev1beta2.SchemeGroupVersion.String()) {
		klog.Infof("Using %s for DeviceTaintRules", resourcev1beta2.SchemeGroupVersion)
		return &v1beta2DeviceTaintRuleClient{client: clientset.ResourceV1beta2().DeviceTaintRules()}, nil
	}
	return nil, fmt.Errorf("the cluster serves DeviceTaintRules neither in %s nor in %s; enable the DRADeviceTaintRules feature gate (and the API version via --runtime-config if needed) on the kube-apiserver",
		resourceapi.SchemeGroupVersion, resourcev1beta2.SchemeGroupVersion)
}

type v1DeviceTaintRuleClient struct {
	client resourcev1client.DeviceTaintRuleInterface
}

func (c *v1DeviceTaintRuleClient) Create(ctx context.Context, rule *resourceapi.DeviceTaintRule) (*resourceapi.DeviceTaintRule, error) {
	return c.client.Create(ctx, rule, metav1.CreateOptions{})
}

func (c *v1DeviceTaintRuleClient) Get(ctx context.Context, name string) (*resourceapi.DeviceTaintRule, error) {
	return c.client.Get(ctx, name, metav1.GetOptions{})
}

func (c *v1DeviceTaintRuleClient) List(ctx context.Context, opts metav1.ListOptions) ([]resourceapi.DeviceTaintRule, error) {
	list, err := c.client.List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (c *v1DeviceTaintRuleClient) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	return c.client.Delete(ctx, name, opts)
}

func (c *v1DeviceTaintRuleClient) DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error {
	return c.client.DeleteCollection(ctx, opts, listOpts)
}

type v1beta2DeviceTaintRuleClient struct {
	client resourcev1beta2client.DeviceTaintRuleInterface
}

func (c *v1beta2DeviceTaintRuleClient) Create(ctx context.Context, rule *resourceapi.DeviceTaintRule) (*resourceapi.DeviceTaintRule, error) {
	created, err := c.client.Create(ctx, deviceTaintRuleToV1beta2(rule), metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return deviceTaintRuleFromV1beta2(created), nil
}

func (c *v1beta2DeviceTaintRuleClient) Get(ctx context.Context, name string) (*resourceapi.DeviceTaintRule, error) {
	rule, err := c.client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return deviceTaintRuleFromV1beta2(rule), nil
}

func (c *v1beta2DeviceTaintRuleClient) List(ctx context.Context, opts metav1.ListOptions) ([]resourceapi.DeviceTaintRule, error) {
	list, err := c.client.List(ctx, opts)
	if err != nil {
		return nil, err
	}
	rules := make([]resourceapi.DeviceTaintRule, 0, len(list.Items))
	for i := range list.Items {
		rules = append(rules, *deviceTaintRuleFromV1beta2(&list.Items[i]))
	}
	return rules, nil
}

func (c *v1beta2DeviceTaintRuleClient) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	return c.client.Delete(ctx, name, opts)
}

func (c *v1beta2DeviceTaintRuleClient) DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error {
	return c.client.DeleteCollection(ctx, opts, listOpts)
}

// The conversions cover the fields the controller sets, as status is server-owned.
func deviceTaintRuleToV1beta2(rule *resourceapi.DeviceTaintRule) *resourcev1beta2.DeviceTaintRule {
	out := &resourcev1beta2.DeviceTaintRule{
		ObjectMeta: rule.ObjectMeta,
		Spec: resourcev1beta2.DeviceTaintRuleSpec{
			Taint: resourcev1beta2.DeviceTaint{
				Key:       rule.Spec.Taint.Key,
				Value:     rule.Spec.Taint.Value,
				Effect:    resourcev1beta2.DeviceTaintEffect(rule.Spec.Taint.Effect),
				TimeAdded: rule.Spec.Taint.TimeAdded,
			},
		},
	}
	if s := rule.Spec.DeviceSelector; s != nil {
		out.Spec.DeviceSelector = &resourcev1beta2.DeviceTaintSelector{Driver: s.Driver, Pool: s.Pool, Device: s.Device}
	}
	return out
}

func deviceTaintRuleFromV1beta2(rule *resourcev1beta2.DeviceTaintRule) *resourceapi.DeviceTaintRule {
	out := &resourceapi.DeviceTaintRule{
		ObjectMeta: rule.ObjectMeta,
		Spec: resourceapi.DeviceTaintRuleSpec{
			Taint: resourceapi.DeviceTaint{
				Key:       rule.Spec.Taint.Key,
				Value:     rule.Spec.Taint.Value,
				Effect:    resourceapi.DeviceTaintEffect(rule.Spec.Taint.Effect),
				TimeAdded: rule.Spec.Taint.TimeAdded,
			},
		},
	}
	if s := rule.Spec.DeviceSelector; s != nil {
		out.Spec.DeviceSelector = &resourceapi.DeviceTaintSelector{Driver: s.Driver, Pool: s.Pool, Device: s.Device}
	}
	return out
}
