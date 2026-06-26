
/*
 * Copyright 1993-2022 NVIDIA Corporation.  All rights reserved.
 *
 * NVIDIA CORPORATION and its licensors retain all intellectual property
 * and proprietary rights in and to this software, related documentation
 * and any modifications thereto.  Any use, reproduction, disclosure or
 * distribution of this software and related documentation without an express
 * license agreement from NVIDIA CORPORATION is strictly prohibited.
 *
 */
#ifndef NV_FM_AGENT_H
#define NV_FM_AGENT_H
#ifdef __cplusplus
extern "C" {
#endif
#include "nv_fm_types.h"

#define FM_LIB_API   __attribute__ ((visibility ("default")))

/***************************************************************************************************/
/** @defgroup FMAPI_Admin Administrative
 *   
 *  This chapter describes the administration interfaces for Fabric Manager API interface library.
 *  It is the user's responsibility to call \ref fmLibInit() before calling any other methods, 
 *  and \ref fmLibShutdown() once Fabric Manager is no longer being used. The APIs in Administrative
 *  module can be broken down into following categories:
 *  @{
 */
/***************************************************************************************************/
/**
 * This method is used to initialize the Fabric Manager API interface library. This must be 
 * called before fmConnect()
 * 
 *  * @return 
 *        - \ref FM_ST_SUCCESS              if FM API interface library has been properly initialized
 *        - \ref FM_ST_IN_USE               FM API interface library is already in initialized state
 *        - \ref FM_ST_GENERIC_ERROR        A generic, unspecified error occurred
 */
fmReturn_t FM_LIB_API fmLibInit(void);
/**
 * This method is used to shut down the Fabric Manager API interface library. Any remote connections
 * established through fmConnect() will be shut down as well.
 *  
 * @return 
 *        - \ref FM_ST_SUCCESS           if FM API interface library has been properly shut down
 *        - \ref FM_ST_UNINITIALIZED     FM API interface library was not in initialized state
 */
fmReturn_t FM_LIB_API fmLibShutdown(void);
/**
 * This method is used to connect to a running instance of Fabric Manager. Fabric Manager instance is
 * started as part of system service or manually by the SysAdmin. This connection will be used by the
 * APIs to exchange information to the running Fabric Manager instance.
 *
 * @param connectParams IN : Valid IP address for the remote host engine to connect to.
 *                           If addressInfo is specified as x.x.x.x it will attempt to connect to the default
 *                           port specified by FM_CMD_PORT_NUMBER
 *                           If addressInfo is specified as x.x.x.x:yyyy it will attempt to connect to the
 *                           port specified by yyyy
 *                           To connect to an FM instance that was started with unix domain socket, 
 *                           fill the socket path in addressInfo member and set addressType to NV_FM_API_ADDR_TYPE_UNIX.
 *                           To connect to an FM instance on a vsock connection fill addressInfo with `<cid>:<port_number>`.
 *                           Supported address types are listed in nvFmApiAddrTypes enumeration. 
 *
 *                           For additional connection parameters. See \ref fmConnectParams_t for details.
 * @param pFmHandle     OUT : Fabric Manager API interface abstracted handle for subsequent API calls.
 *
 * @return
 *         - \ref FM_ST_SUCCESS                successfully connected to the FM instance
 *         - \ref FM_ST_CONNECTION_NOT_VALID   if the FM instance could not be reached
 *         - \ref FM_ST_UNINITIALIZED          if FM interface library has not been initialized with \ref fmLibInit.
 *         - \ref FM_ST_BADPARAM               if pFmHandle is NULL or provided IP Address/format is invalid
 *         - \ref FM_ST_VERSION_MISMATCH       if the expected and provided versions of connectParams do not match
 */
 fmReturn_t FM_LIB_API fmConnect(fmConnectParams_t *connectParams, fmHandle_t *pFmHandle);
/**
 * This method is used to disconnect from a Fabric Manager instance.
 *
 * @param pFmHandle IN:  Handle that came form \ref fmConnect
 *
 * @return
 *         - \ref FM_ST_SUCCESS             if we successfully disconnected from the FM instance
 *         - \ref FM_ST_UNINITIALIZED       if FM interface library has not been initialized with \ref fmLibInit
 *         - \ref FM_ST_BADPARAM            if pFmHandle is not a valid handle
 *         - \ref FM_ST_GENERIC_ERROR       if an unspecified internal error occurred
 */
fmReturn_t FM_LIB_API fmDisconnect(fmHandle_t pFmHandle);
/** @} */ // Closing for FMAPI_Admin
/******************************************************************************************************/
/** @defgroup FMAPI_FabricPartition Fabric Partition Related APIs
 *   
 *  This chapter describes the APIs for Fabric Partition management for Shared NVSwitch and vGPU Models 
 *  @{
 */
/*******************************************************************************************************/
/**
 * This method is used to query all the supported fabric partitions in an NVSwitch based system.
 * These fabric partitions allow users to assign specified GPUs to a guestOS as part of multitenancy
 * with necessary NVLink isolation.
 *
 * @param pFmHandle IN:  Handle that came form \ref fmConnect
 *
 * @param pFmFabricPartition  OUT: List of currently supported fabric partition information.
 *
 * @return
 *         - \ref FM_ST_SUCCESS             successfully queried the list of supported partitions
 *         - \ref FM_ST_UNINITIALIZED       if FM interface library has not been initialized with \ref fmLibInit
 *         - \ref FM_ST_BADPARAM            Invalid input parameters
 *         - \ref FM_ST_GENERIC_ERROR       if an unspecified internal error occurred
 *         - \ref FM_ST_NOT_SUPPORTED       requested feature is not supported or enabled
 *         - \ref FM_ST_NOT_CONFIGURED      Fabric Manager instance is initializing and no data
 *         - \ref FM_ST_VERSION_MISMATCH    if the expected and provided versions of pFmFabricPartition do not match
 */
fmReturn_t FM_LIB_API fmGetSupportedFabricPartitions(fmHandle_t pFmHandle, fmFabricPartitionList_t *pFmFabricPartition);
/**
 * This method is used to activate an available fabric partition in an NVSwitch based system.
 *
 * @param pFmHandle IN:  Handle that came form \ref fmConnect
 *
 * @param partitionId  IN: The partition id to be activated
 *
 * @return
 *         - \ref FM_ST_SUCCESS             Specified partition is activated successfully
 *         - \ref FM_ST_UNINITIALIZED       if FM interface library has not been initialized with \ref fmLibInit
 *         - \ref FM_ST_BADPARAM            if pFmHandle is not a valid handle or unsupported partition id
 *         - \ref FM_ST_GENERIC_ERROR       if an unspecified internal error occurred
 *         - \ref FM_ST_NOT_SUPPORTED       requested feature is not supported or enabled
 *         - \ref FM_ST_NOT_CONFIGURED      Fabric Manager instance is initializing and no data
 *         - \ref FM_ST_IN_USE              specified partition is already active
 *         - \ref FM_ST_NVLINK_ERROR        NVLink error/training failure occurred when activating the partition
 */
fmReturn_t FM_LIB_API fmActivateFabricPartition(fmHandle_t pFmHandle, fmFabricPartitionId_t partitionId);
/**
 * This method is used to activate an available fabric partition with VFs in an NVSwitch based system.
 *
 * @param pFmHandle IN:  Handle that came form \ref fmConnect
 *
 * @param partitionId  IN: The partition id to be activated
 *
 * @param vfList IN: List of VFs associated with physical GPUs in the partition.
 *                   Please note that the order of VFs should be associated with actual physical GPUs in the partition.
 *
 * @param numVfs IN: Number of VFs
 *
 * @return
 *         - \ref FM_ST_SUCCESS             Specified partition is activated successfully
 *         - \ref FM_ST_UNINITIALIZED       if FM interface library has not been initialized with \ref fmLibInit
 *         - \ref FM_ST_BADPARAM            if pFmHandle is not a valid handle or unsupported partition id
 *         - \ref FM_ST_GENERIC_ERROR       if an unspecified internal error occurred
 *         - \ref FM_ST_NOT_SUPPORTED       requested feature is not supported or enabled
 *         - \ref FM_ST_NOT_CONFIGURED      Fabric Manager instance is initializing and no data
 *         - \ref FM_ST_IN_USE              specified partition is already active
 *         - \ref FM_ST_NVLINK_ERROR        NVLink error/training failure occurred when activating the partition
 */
fmReturn_t FM_LIB_API fmActivateFabricPartitionWithVFs(fmHandle_t pFmHandle, fmFabricPartitionId_t partitionId, fmPciDevice_t *vfList, unsigned int numVfs);
/**
 * This method is used to deactivate a previously activated fabric partition in an NVSwitch based system.
 *
 * @param pFmHandle IN:  Handle that came form \ref fmConnect
 *
 * @param partitionId  IN: The partition id to be deactivated
 *
 * @return
 *         - \ref FM_ST_SUCCESS             Specified partition is deactivated successfully
 *         - \ref FM_ST_UNINITIALIZED       if FM interface library has not been initialized with \ref fmLibInit
 *         - \ref FM_ST_BADPARAM            if pFmHandle is not a valid handle or unsupported partition id
 *         - \ref FM_ST_GENERIC_ERROR       if an unspecified internal error occurred
 *         - \ref FM_ST_NOT_SUPPORTED       requested feature is not supported or enabled
 *         - \ref FM_ST_NOT_CONFIGURED      Fabric Manager instance is initializing and no data
 *         - \ref FM_ST_UNINITIALIZED       specified partition is not activated
 *         - \ref FM_ST_NVLINK_ERROR        NVLink error/training failure occurred when deactivating the partition
 */
fmReturn_t FM_LIB_API fmDeactivateFabricPartition(fmHandle_t pFmHandle, fmFabricPartitionId_t partitionId);
/**
 * This method is used to set a list of currently activated fabric partitions to Fabric Manager after its restart.
 * This call should be made with number of partitions as zero even if there is no active partitions
 * when Fabric Manager is restarted.
 *
 * @param pFmHandle IN:  Handle that came form \ref fmConnect
 *
 * @param pFmActivatedPartitionList  IN: List of currently activated fabric partition.
 * @return
 *         - \ref FM_ST_SUCCESS             Fabric Manager state is updated with active partition information
 *         - \ref FM_ST_UNINITIALIZED       if FM interface library has not been initialized with \ref fmLibInit
 *         - \ref FM_ST_BADPARAM            A bad parameter was passed.
  *        - \ref FM_ST_GENERIC_ERROR       if an unspecified internal error occurred
 *         - \ref FM_ST_NOT_SUPPORTED       Requested feature is not supported or enabled
 *         - \ref FM_ST_NOT_CONFIGURED      Fabric Manager is initializing and no data available.
 *         - \ref FM_ST_VERSION_MISMATCH    if the expected and provided versions of pFmActivatedPartitionList do not match
 */
fmReturn_t FM_LIB_API fmSetActivatedFabricPartitions(fmHandle_t pFmHandle, fmActivatedFabricPartitionList_t *pFmActivatedPartitionList);
/**
 * This method is used to query all GPUs and NVSwitches with failed NVLinks as part of Fabric Manager initialization
 *
 * This API is not supported when Fabric Manager is running in Shared NVSwitch
 * multi-tenancy resiliency restart (--restart) mode
 *
 * Note: On HGX H100 8-GPU based systems, NVLinks are trained at hardware level without higher level software coordination.
 * So, Fabric Manager will always return an empty failed NVLink device list for this call on those systems.
 *
 * @param pFmHandle IN:  Handle that came form \ref fmConnect
 *
 * @param pFmNvlinkFailedDevices  OUT: List of GPU or NVSwitch devices that have failed NVLinks
 *
 * @return
 *         - \ref FM_ST_SUCCESS             successfully queried the list of devices with failed NVLinks
 *         - \ref FM_ST_UNINITIALIZED       if FM interface library has not been initialized with \ref fmLibInit
 *         - \ref FM_ST_BADPARAM            Invalid input parameters
 *         - \ref FM_ST_GENERIC_ERROR       if an unspecified internal error occurred
 *         - \ref FM_ST_NOT_SUPPORTED       requested feature is not supported or enabled
 *         - \ref FM_ST_NOT_CONFIGURED      Fabric Manager instance is initializing and no data
 *         - \ref FM_ST_VERSION_MISMATCH    if the expected and provided versions of pFmFabricPartition do not match
 */
fmReturn_t FM_LIB_API fmGetNvlinkFailedDevices(fmHandle_t pFmHandle, fmNvlinkFailedDevices_t *pFmNvlinkFailedDevices);
/***************************************************************************************************/
/**
 * This method is used to query all the unsupported fabric partitions when Fabric Manager is
 * running in Shared NVSwitch multi-tenancy mode.
 *
 * @param pFmHandle IN:  Handle that came form \ref fmConnect
 *
 * @param pFmUnupportedFabricPartition  OUT: List of unsupported fabric partitions on the system.
 *
 * @return
 *         - \ref FM_ST_SUCCESS             successfully queried the list of unsupported partitions
 *         - \ref FM_ST_UNINITIALIZED       if FM interface library has not been initialized with \ref fmLibInit
 *         - \ref FM_ST_BADPARAM            Invalid input parameters
 *         - \ref FM_ST_GENERIC_ERROR       if an unspecified internal error occurred
 *         - \ref FM_ST_NOT_SUPPORTED       requested feature is not supported or enabled
 *         - \ref FM_ST_NOT_CONFIGURED      Fabric Manager instance is initializing and no data
 *         - \ref FM_ST_VERSION_MISMATCH    if the expected and provided versions of pFmUnupportedFabricPartition do not match
 */
fmReturn_t FM_LIB_API fmGetUnsupportedFabricPartitions(fmHandle_t pFmHandle,
                                                       fmUnsupportedFabricPartitionList_t *pFmUnupportedFabricPartition);
/** @} */ // Closing for FMAPI_FabricPartition
#ifdef __cplusplus
}
#endif

#endif /* NV_FM_AGENT_H */

