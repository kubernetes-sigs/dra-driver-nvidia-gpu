/*
 *  Copyright 2018-2020 NVIDIA Corporation.  All rights reserved.
 *
 * NVIDIA CORPORATION and its licensors retain all intellectual property
 * and proprietary rights in and to this software, related documentation
 * and any modifications thereto.  Any use, reproduction, disclosure or
 * distribution of this software and related documentation without an express
 * license agreement from NVIDIA CORPORATION is strictly prohibited.
 *
 */

#ifndef NV_FM_TYPES_H
#define NV_FM_TYPES_H

#ifdef __cplusplus
extern "C" {
#endif

/** @defgroup FMAPI_Common Common Structures
 *   
 *  This chapter describes the common structures for Fabric Manager API interface library.
 *  @{
 */

/** 
 * Return values for Fabric Manager API calls. 
 */
typedef enum fmReturn_enum
{
    FM_ST_SUCCESS                        =  0,  //!< The operation was successful
    FM_ST_BADPARAM                       = -1,  //!< A supplied argument is invalid
    FM_ST_GENERIC_ERROR                  = -2,  //!< A generic, unspecified error
    FM_ST_NOT_SUPPORTED                  = -3,  //!< The requested operation/feature not supported
    FM_ST_UNINITIALIZED                  = -4,  //!< Object is in undefined state/uninitialized
    FM_ST_TIMEOUT                        = -5,  //!< Requested operation timed out or user provided timeout passed
    FM_ST_VERSION_MISMATCH               = -6,  //!< Version mismatch between received and understood API
    FM_ST_IN_USE                         = -7,  //!< The requested operation cannot be performed because the resource is in use
    FM_ST_NOT_CONFIGURED                 = -8,  //!< Setting not configured
    FM_ST_CONNECTION_NOT_VALID           = -9,  //!< The connection to the FM instance is not valid any longer 
    FM_ST_NVLINK_ERROR                   = -10, //!< Requested operation failed due to NVLink error
    FM_ST_RESOURCE_BAD                   = -11, //!< Requested operation referenced a resource that does not exist
    FM_ST_RESOURCE_IN_USE                = -12, //!< Requested operation failed because one of the referenced resources is already in use
    FM_ST_RESOURCE_NOT_IN_USE            = -13, //!< Requested operation failed because one of the referenced resources is not in use
    FM_ST_RESOURCE_EXHAUSTED             = -14, //!< Requested operation failed because not enough of one of the resources could be allocated
    FM_ST_RESOURCE_NOT_READY             = -15, //!< Requested operation failed because one of the resources is not ready to be used
    FM_ST_PARTITION_EXISTS               = -16, //!< Partition has already been created, nothing to do
    FM_ST_PARTITION_ID_IN_USE            = -17, //!< Partition id already used by another partition, could not complete operation
    FM_ST_PARTITION_ID_NOT_IN_USE        = -18, //!< Partition id could not be found
    FM_ST_PARTITION_NAME_IN_USE          = -19, //!< Partition name is already used by another partition and name is supposed to be unique
    FM_ST_PARTITION_NAME_NOT_IN_USE      = -20, //!< Partition name is specified but the referenced partition could not be found
    FM_ST_PARTITION_ID_NAME_MISMATCH     = -21, //!< Valid partition id and name specified but they refer to different partitions  
    FM_ST_NOT_READY                      = -22, //!< Fabric Manager is not ready to serve GFM API requests.
    FM_ST_RESOURCE_USED_IN_THIS_PARTITION = -23, //!< Resource is already in use in this partition
    FM_ST_RESOURCE_USED_IN_ANOTHER_PARTITION = -24, //!< Resource is already in use in another partition
    FM_ST_PARTITION_MISWIRED_TRUNKS      = -25, //!< Partition has miswired trunks
    FM_ST_PARTITION_INSUFFICIENT_TRUNKS  = -26, //!< Partition has insufficient trunks
    FM_ST_PARTITION_MISSING_SWITCHES     = -27, //!< Partition has missing switches
    FM_ST_PARTITION_NETWORK_CONFIG_ERROR = -28, //!< Partition has network configuration error
    FM_ST_PARTITION_ROUTE_PROGRAMMING_ERROR = -29, //!< Partition has route programming error
} fmReturn_t;

typedef void *fmHandle_t;   //!< Identifier for Fabric Manager API interface Handle

/**
 * Max length of the FM string field
 */
#define FM_MAX_STR_LENGTH     256    

/**
 * Buffer size guaranteed to be large enough to hold UUID
 */
#define FM_UUID_BUFFER_SIZE     80

/**
 * Buffer size guaranteed to be large enough for pci bus id
 */
#define FM_DEVICE_PCI_BUS_ID_BUFFER_SIZE    32

/**
 * Creates a unique version number for each struct
 */
#define MAKE_FM_PARAM_VERSION(typeName,ver) (unsigned int)(sizeof(typeName) | ((ver)<<24))

/**
 * Default port number used by  FM interface library to exchange commands to FM instance
 */
#define FM_CMD_PORT_NUMBER 6666

/**
 * Buffer size guaranteed to be large enough to hold transaction id
 */
#define FM_TRANSACTION_ID_BUFFER_SIZE 40

/**
 * Connection options for fmConnect()
 */
typedef struct 
{
    unsigned int version;                   //!< Version number. Use fmConnectParams_version
    char addressInfo[FM_MAX_STR_LENGTH];    //!< IP address and port information
    unsigned int timeoutMs;                 /*!< When attempting to connect to the running FM instance, 
                                              how long should we wait in milliseconds before giving up */
    unsigned int addressIsUnixSocket;       /*!< Whether or not the passed-in address is a Unix domain socket
                                              filename (1) or a TCP/IP address (0) */
} fmConnectParams_v1;

/**
 * @enum nvFmApiAddrTypes
 * @brief an enumeration of supported address types
 */
enum nvFmApiAddrTypes
{
    NV_FM_API_ADDR_TYPE_UNKNOWN = 0,
    NV_FM_API_ADDR_TYPE_INET = 1,
    NV_FM_API_ADDR_TYPE_UNIX = 2,
    NV_FM_API_ADDR_TYPE_VSOCK = 3,
};

typedef struct 
{
    unsigned int version;                   //!< Version number. Use fmConnectParams_version
    char addressInfo[FM_MAX_STR_LENGTH];    /*!< identifier information interpreted based on addressType's value; 
                                                 when addressType == NV_FM_API_ADDR_TYPE_INET addressInfo is `<ipv4>:<port_number>`;
                                                 when addressType == NV_FM_API_ADDR_TYPE_UNIX addressInfo is `<unix_path>`;
                                                 when addressType == NV_FM_API_ADDR_TYPE_VSOCK addressInfo is `<cid>:<port_number>` */
    unsigned int timeoutMs;                 /*!< When attempting to connect to the running FM instance, 
                                              how long should we wait in milliseconds before giving up */
    unsigned int addressIsUnixSocket;       /*!< deprecated; use addressType instead */
    enum nvFmApiAddrTypes addressType;           /*!< The type of address in addressInfo */
} fmConnectParams_v2;

/**
 * Typedef for \ref fmConnectParams_t
 */
typedef fmConnectParams_v2 fmConnectParams_t;
 
/**
* Version 1 for \ref fmConnectParams_v1
*/
#define fmConnectV1Params_version1 MAKE_FM_PARAM_VERSION(fmConnectParams_v1, 1)

/**
* Version 2 for \ref fmConnectParams_v2
*/
#define fmConnectV2Params_version2 MAKE_FM_PARAM_VERSION(fmConnectParams_v2, 2)

/**
* Latest version for \ref fmConnectParams_t
*/
#define fmConnectParams_version fmConnectV2Params_version2

/**
 * Max number of GPUs supported by FM
 */    
#define FM_MAX_NUM_GPUS     16

/**
 * Max number of NVSwitches supported by FM
 */
#define FM_MAX_NUM_NVSWITCHES    12

/**
 * Max number of GPU/fabric partitions supported by FM
 */    
#define FM_MAX_FABRIC_PARTITIONS 64

typedef unsigned int fmFabricPartitionId_t; //!< Identifier to hold a unique id assigned to each partitions

/**
 * Max number of ports per NVLink device supported by FM
 */
#define FM_MAX_NUM_NVLINK_PORTS  64

/**
 * PCI Device (BDF) Information
 */
typedef struct
{
    unsigned int domain;                                //!< The PCI domain on which the device's bus resides, 0 to 0xffffffff
    unsigned int bus;                                   //!< The bus on which the device resides, 0 to 0xff
    unsigned int device;                                //!< The device's id on the bus, 0 to 31
    unsigned int function;                              //!< The function number of the device, 0 to 7 (Non-ARI) or 0 to 255 (ARI)
} fmPciDevice_t;

/**
 * Structure to store information about a GPU belonging to fabric partition
 */
typedef struct
{
    unsigned int physicalId;                            //!< physical id number of the GPU, same value as GPU Module ID
    char uuid[FM_UUID_BUFFER_SIZE];                     //!< GPU UUID information
    char pciBusId[FM_DEVICE_PCI_BUS_ID_BUFFER_SIZE];    //!< GPU PCI BDF information
    unsigned int numNvLinksAvailable;                   /*!< Number of NVLinks available for use on this GPU, this
                                                             can be fewer than the max due to hardware availability */
    unsigned int maxNumNvLinks;                         /*!< Max number of NVLinks available for this GPU under normal
                                                             operation (i.e. without any degradation) */
    unsigned int nvlinkLineRateMBps;                    //!< Per NVLink full speed line rate in Mega bytes per second
} fmFabricPartitionGpuInfo_t;

/**
 * Structure to store information about a fabric partition
 */
typedef struct
{
    fmFabricPartitionId_t partitionId;                      //!< a unique id assigned to reference this partition
    unsigned int isActive;                                  //!< partition active state. 1 means active, 0 mean not active.
    unsigned int numGpus;                                   //!< number of GPUs in this partition.
    fmFabricPartitionGpuInfo_t gpuInfo[FM_MAX_NUM_GPUS];    //!< detailed meta data of each GPUs assigned to this partition.
} fmFabricPartitionInfo_t;

/**
 * Structure to store information about all the supported fabric partitions
 */
typedef struct
{
    unsigned int version;                                               //!< version number. Use fmFabricPartitionList_version
    unsigned int numPartitions;                                         /*!< total number of partitions supported, this can be fewer
                                                                             than the max due to hardware availability */
    unsigned int maxNumPartitions;                                      /*!< max number of partitions that can be supported on
                                                                             this platform */
    fmFabricPartitionInfo_t partitionInfo[FM_MAX_FABRIC_PARTITIONS];    //!< detailed meta data of each partitions
} fmFabricPartitionList_v2;

/// Typedef for \ref fmFabricPartitionList_v2
typedef fmFabricPartitionList_v2 fmFabricPartitionList_t;
/// Version 1 for \ref fmFabricPartitionList_v2
#define fmFabricPartitionList_version2 MAKE_FM_PARAM_VERSION(fmFabricPartitionList_v2, 1)
/// Latest version for \ref fmFabricPartitionList_v2
#define fmFabricPartitionList_version fmFabricPartitionList_version2

/**
 * Structure to store information about all the activated fabric partitionIds
 */
typedef struct
{
    unsigned int version;                                         //!< version number. Use fmActivatedFabricPartitionList_version
    unsigned int numPartitions;                                   //!< number of partitions already activated
    fmFabricPartitionId_t partitionIds[FM_MAX_FABRIC_PARTITIONS]; //!< partitions that are already activated
} fmActivatedFabricPartitionList_v1;

/// Typedef for \ref fmActivatedFabricPartitionList_v1
typedef fmActivatedFabricPartitionList_v1 fmActivatedFabricPartitionList_t;
/// Version 1 for \ref fmActivatedFabricPartitionList_v1
#define fmActivatedFabricPartitionList_version1 MAKE_FM_PARAM_VERSION(fmActivatedFabricPartitionList_v1, 1)
/// Latest version for \ref fmActivatedFabricPartitionList_v1
#define fmActivatedFabricPartitionList_version fmActivatedFabricPartitionList_version1

/**
 * Structure to store information about a NVSwitch or GPU with failed NVLinks
 */
typedef struct
{
    char         uuid[FM_UUID_BUFFER_SIZE];                  //!< Device UUID information
    char         pciBusId[FM_DEVICE_PCI_BUS_ID_BUFFER_SIZE]; //!< Device PCI BDF information
    unsigned int numPorts;                                   //!< the number of ports that have failed NVLinks
    unsigned int portNum[FM_MAX_NUM_NVLINK_PORTS];           //!< port number that has failed NVLinks
} fmNvlinkFailedDeviceInfo_t;

/**
 * Structure to store a list of NVSwitches and GPUs with failed NVLinks
 */
typedef struct
{
    unsigned int                version;                          //!< version number. Use fmNvlinkFailedDevices_version
    unsigned int                numGpus;                          //!< number of GPUs with failed NVLinks
    unsigned int                numSwitches;                      //!< number of NVSwitches with failed NVLinks
    fmNvlinkFailedDeviceInfo_t  gpuInfo[FM_MAX_NUM_GPUS];         //!< list of GPU with failed NVLinks
    fmNvlinkFailedDeviceInfo_t  switchInfo[FM_MAX_NUM_NVSWITCHES];//!< list of NVSwitch with failed NVLinks
} fmNvlinkFailedDevices_v1;

/// Typedef for \ref fmNvlinkFailedDevices_v1
typedef fmNvlinkFailedDevices_v1 fmNvlinkFailedDevices_t;
/// Version 1 for \ref fmNvlinkFailedDevices_v1
#define fmNvlinkFailedDevices_version1 MAKE_FM_PARAM_VERSION(fmNvlinkFailedDevices_v1, 1)
/// Latest version for \ref fmNvlinkFailedDevices_v1
#define fmNvlinkFailedDevices_version fmNvlinkFailedDevices_version1

/**
 * Structure to store information about a unsupported fabric partition
 */
typedef struct
{
    fmFabricPartitionId_t partitionId;                    //!< a unique id assigned to reference this partition
    unsigned int numGpus;                                 //!< number of GPUs in this partition
    unsigned int gpuPhysicalIds[FM_MAX_NUM_GPUS];         //!< physicalId of each GPU assigned to this partition.
} fmUnsupportedFabricPartitionInfo_t;

/**
 * Structure to store information about all the unsupported fabric partitions
 */
typedef struct
{
    unsigned int version;                                   //!< version number. Use fmFabricPartitionList_version
    unsigned int numPartitions;                             //!< total number of unsupported partitions
    fmUnsupportedFabricPartitionInfo_t partitionInfo[FM_MAX_FABRIC_PARTITIONS];  /*!< detailed information of each
                                                                                      unsupported partition*/
} fmUnsupportedFabricPartitionList_v1;

/// Typedef for \ref fmUnsupportedFabricPartitionList_v1
typedef fmUnsupportedFabricPartitionList_v1 fmUnsupportedFabricPartitionList_t;
/// Version 1 for \ref fmUnsupportedFabricPartitionList_v1
#define fmUnsupportedFabricPartitionList_version1 MAKE_FM_PARAM_VERSION(fmUnsupportedFabricPartitionList_v1, 1)
/// Latest version for \ref fmUnsupportedFabricPartitionList_v1
#define fmUnsupportedFabricPartitionList_version fmUnsupportedFabricPartitionList_version1

/** @} */ // Closing for FMAPI_Common Common Structures
#ifdef __cplusplus
}
#endif

#endif /* NV_FM_TYPES_H */

