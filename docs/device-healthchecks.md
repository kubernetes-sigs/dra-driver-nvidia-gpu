# NVMLDeviceHealthCheck Feature Gate (Alpha)

Note: This is an alpha feature and not recommended for production environments. 

The NVIDIA DRA driver supports GPU health monitoring using the [NVIDIA Management Library (NVML)](https://developer.nvidia.com/management-library-nvml) to check for [GPU XID errors](https://docs.nvidia.com/deploy/xid-errors/introduction.html) and determines if a GPU or MIG device is functioning properly.

GPU health checking is managed by the ``NVMLDeviceHealthCheck`` feature gate. This is currently an alpha feature and is disabled by default.

When enabled, the DRA Driver for GPUs continuously monitors GPUs for XID errors and assigns health statuses:
* Healthy - GPU is functioning normally. The GPU may have a non-critical XID error but is still available for workloads.
* Unhealthy - GPU has a critical XID error and is not suitable for workloads.

The DRA Driver removes  `unhealthy` devices from the available [ResourceSlices](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#resourceslice). 
After the device recovers you must restart the DRA Driver for the device to be marked as `Healthy` and added back into the available resource pool. 

## Enabling NVMLDeviceHealthCheck

To enable GPU health monitoring, deploy the DRA Driver for GPUs with the NVMLDeviceHealthCheck feature gate.
Add the NVIDIA Helm repo if you have not already, then install or upgrade with the feature gate enabled:

```
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia && helm repo update
helm upgrade --install nvidia-dra-driver-gpu nvidia/nvidia-dra-driver-gpu \
  --namespace nvidia-dra-driver-gpu \
  --create-namespace \
  --set featureGates.NVMLDeviceHealthCheck=true
```

Refer to the [Installation](https://github.com/NVIDIA/k8s-dra-driver-gpu/wiki/Installation) guide for full details on install prerequisites and configuration options. 

## Viewing Device Health Status

After enabling health checks, you can monitor health status in the kubelet logs.

1. Check kubelet plugin logs.
   Health status changes are logged in the kubelet plugin container, run `kubectl get pods -n nvidia-dra-driver-gpu` and find the `nvidia-dra-driver-gpu-kubelet-plugin-` pod name. 
   Replace <pod> with your actual pod name:

    ```
    kubectl logs nvidia-dra-driver-gpu-kubelet-plugin-<pod> \
    -n nvidia-dra-driver-gpu \
      -c gpus
    ```

2. List all ResourceSlices.
    View all ResourceSlices in the cluster to see which devices are available:

    ```
    kubectl get resourceslice
    ```

3. Inspect a specific ResourceSlice.
   View detailed information about a specific resource slice. Healthy devices are listed in the resource slice, while unhealthy devices are not listed:

    ```
    kubectl get resourceslice <resourceslice-name> -o yaml
    ```
    
Unhealthy GPUs will not appear in the resource slice list. After the device recovers and is marked healthy again, you must restart the DRA Driver for the device to be added back into the available resources pool.
