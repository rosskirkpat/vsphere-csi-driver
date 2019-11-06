/*
Copyright 2019 The Kubernetes Authors.

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

package syncer

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"time"

	"github.com/davecgh/go-spew/spew"
	cnstypes "gitlab.eng.vmware.com/hatchway/govmomi/cns/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog"
	volumes "sigs.k8s.io/vsphere-csi-driver/pkg/common/cns-lib/volume"
	cnsvsphere "sigs.k8s.io/vsphere-csi-driver/pkg/common/cns-lib/vsphere"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/common"
	csitypes "sigs.k8s.io/vsphere-csi-driver/pkg/csi/types"
	k8s "sigs.k8s.io/vsphere-csi-driver/pkg/kubernetes"
	"sigs.k8s.io/vsphere-csi-driver/pkg/syncer/types"
)

// new Returns uninitialized metadataSyncInformer
func NewInformer() *metadataSyncInformer {
	return &metadataSyncInformer{}
}

// getFullSyncIntervalInMin return the FullSyncInterval
// If enviroment variable X_CSI_FULL_SYNC_INTERVAL_MINUTES is set and valid,
// return the interval value read from enviroment variable
// otherwise, use the default value 30 minutes
func getFullSyncIntervalInMin() int {
	fullSyncIntervalInMin := defaultFullSyncIntervalInMin
	if v := os.Getenv("X_CSI_FULL_SYNC_INTERVAL_MINUTES"); v != "" {
		if value, err := strconv.Atoi(v); err == nil {
			if value <= 0 {
				klog.Warningf("FullSync: fullSync interval set in env variable X_CSI_FULL_SYNC_INTERVAL_MINUTES %s is equal or less than 0, will use the default interval", v)
			} else if value > defaultFullSyncIntervalInMin {
				klog.Warningf("FullSync: fullSync interval set in env variable X_CSI_FULL_SYNC_INTERVAL_MINUTES %s is larger than max vlaue can be set, will use the default interval", v)
			} else {
				fullSyncIntervalInMin = value
				klog.V(2).Infof("FullSync: fullSync interval is set to %d minutes", fullSyncIntervalInMin)
			}
		} else {
			klog.Warningf("FullSync: fullSync interval set in env variable X_CSI_FULL_SYNC_INTERVAL_MINUTES %s is invalid, will use the default interval", v)
		}
	}
	return fullSyncIntervalInMin
}

// Initializes the Metadata Sync Informer
func (metadataSyncer *metadataSyncInformer) InitMetadataSyncer(clusterFlavor cnstypes.CnsClusterFlavor, configInfo *types.ConfigInfo) error {
	var err error
	klog.V(2).Infof("Initializing MetadataSyncer")
	metadataSyncer.configInfo = configInfo

	// Create the kubernetes client from config
	k8sClient, err := k8s.NewClient()
	if err != nil {
		klog.Errorf("Creating Kubernetes client failed. Err: %v", err)
		return err
	}
	metadataSyncer.clusterFlavor = clusterFlavor
	if metadataSyncer.clusterFlavor == cnstypes.CnsClusterFlavorGuest {
		// Initialize client to supervisor cluster
		// if metadata syncer is being initialized for guest clusters
		restClientConfig := k8s.GetRestClientConfig(metadataSyncer.configInfo.Cfg.GC.Endpoint, metadataSyncer.configInfo.Cfg.GC.Port)
		metadataSyncer.cnsOperatorClient, err = k8s.NewCnsVolumeMetadataClient(restClientConfig)
		if err != nil {
			klog.Errorf("Creating Supervisor client failed. Err: %v", err)
			return err
		}
	} else {
		// Initialize volume manager with vcenter credentials
		// if metadata syncer is being intialized for Vanilla or Supervisor clusters
		vCenter, err := types.GetVirtualCenterInstance(configInfo)
		if err != nil {
			return err
		}
		metadataSyncer.host = vCenter.Config.Host
		metadataSyncer.volumeManager = volumes.GetManager(vCenter)
	}

	// Initialize cnsDeletionMap used by Full Sync
	cnsDeletionMap = make(map[string]bool)
	// Initialize cnsCreationMap used by Full Sync
	cnsCreationMap = make(map[string]bool)

	ticker := time.NewTicker(time.Duration(getFullSyncIntervalInMin()) * time.Minute)
	// Trigger full sync
	go func() {
		for range ticker.C {
			klog.V(2).Infof("fullSync is triggered")
			if metadataSyncer.clusterFlavor == cnstypes.CnsClusterFlavorGuest {
				pvcsiFullSync(k8sClient, metadataSyncer)
			} else {
				csiFullSync(k8sClient, metadataSyncer)
			}
		}
	}()

	stopFullSync := make(chan bool, 1)
	// TODO: Remove channel when pvcsi metadata syncer is implemented
	<-(stopFullSync)

	// Set up kubernetes resource listeners for metadata syncer
	metadataSyncer.k8sInformerManager = k8s.NewInformer(k8sClient)
	metadataSyncer.k8sInformerManager.AddPVCListener(
		nil, // Add
		func(oldObj interface{}, newObj interface{}) { // Update
			pvcUpdated(oldObj, newObj, metadataSyncer)
		},
		func(obj interface{}) { // Delete
			pvcDeleted(obj, metadataSyncer)
		})
	metadataSyncer.k8sInformerManager.AddPVListener(
		nil, // Add
		func(oldObj interface{}, newObj interface{}) { // Update
			pvUpdated(oldObj, newObj, metadataSyncer)
		},
		func(obj interface{}) { // Delete
			pvDeleted(obj, metadataSyncer)
		})
	metadataSyncer.k8sInformerManager.AddPodListener(
		nil, // Add
		func(oldObj interface{}, newObj interface{}) { // Update
			podUpdated(oldObj, newObj, metadataSyncer)
		},
		func(obj interface{}) { // Delete
			podDeleted(obj, metadataSyncer)
		})
	metadataSyncer.pvLister = metadataSyncer.k8sInformerManager.GetPVLister()
	metadataSyncer.pvcLister = metadataSyncer.k8sInformerManager.GetPVCLister()
	klog.V(2).Infof("Initialized metadata syncer")
	stopCh := metadataSyncer.k8sInformerManager.Listen()
	<-(stopCh)

	return nil
}

// pvcUpdated updates persistent volume claim metadata on VC when pvc labels on K8S cluster have been updated
func pvcUpdated(oldObj, newObj interface{}, metadataSyncer *metadataSyncInformer) {
	// Get old and new pvc objects
	oldPvc, ok := oldObj.(*v1.PersistentVolumeClaim)
	if oldPvc == nil || !ok {
		return
	}
	newPvc, ok := newObj.(*v1.PersistentVolumeClaim)
	if newPvc == nil || !ok {
		return
	}

	if newPvc.Status.Phase != v1.ClaimBound {
		klog.V(3).Infof("PVCUpdated: New PVC not in Bound phase")
		return
	}

	// Get pv object attached to pvc
	pv, err := metadataSyncer.pvLister.Get(newPvc.Spec.VolumeName)
	if pv == nil || err != nil {
		klog.Errorf("PVCUpdated: Error getting Persistent Volume for pvc %s in namespace %s with err: %v", newPvc.Name, newPvc.Namespace, err)
		return
	}

	// Verify if pv is vsphere csi volume
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != csitypes.Name {
		klog.V(3).Infof("PVCUpdated: Not a Vsphere CSI Volume")
		return
	}

	// Verify is old and new labels are not equal
	if oldPvc.Status.Phase == v1.ClaimBound && reflect.DeepEqual(newPvc.Labels, oldPvc.Labels) {
		klog.V(3).Infof("PVCUpdated: Old PVC and New PVC labels equal")
		return
	}

	if metadataSyncer.clusterFlavor == cnstypes.CnsClusterFlavorGuest {
		// Invoke volume updated method for pvCSI
		pvcsiVolumeUpdated(newPvc, pv.Spec.CSI.VolumeHandle, metadataSyncer)
	} else {
		csiPVCUpdated(newPvc, pv, metadataSyncer)
	}
}

// pvDeleted deletes pvc metadata on VC when pvc has been deleted on K8s cluster
func pvcDeleted(obj interface{}, metadataSyncer *metadataSyncInformer) {
	pvc, ok := obj.(*v1.PersistentVolumeClaim)
	if pvc == nil || !ok {
		klog.Warningf("PVCDeleted: unrecognized object %+v", obj)
		return
	}
	klog.V(4).Infof("PVCDeleted: %+v", pvc)
	if pvc.Status.Phase != v1.ClaimBound {
		return
	}
	// Get pv object attached to pvc
	pv, err := metadataSyncer.pvLister.Get(pvc.Spec.VolumeName)
	if pv == nil || err != nil {
		klog.Errorf("PVCDeleted: Error getting Persistent Volume for pvc %s in namespace %s with err: %v", pvc.Name, pvc.Namespace, err)
		return
	}

	// Verify if pv is a vsphere csi volume
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != csitypes.Name {
		klog.V(3).Infof("PVCDeleted: Not a Vsphere CSI Volume")
		return
	}

	if metadataSyncer.clusterFlavor == cnstypes.CnsClusterFlavorGuest {
		// Invoke volume deleted method for pvCSI
		pvcsiVolumeDeleted(string(pvc.GetUID()), metadataSyncer)
	} else {
		csiPVCDeleted(pvc, pv, metadataSyncer)
	}
}

// pvUpdated updates volume metadata on VC when volume labels on K8S cluster have been updated
func pvUpdated(oldObj, newObj interface{}, metadataSyncer *metadataSyncInformer) {
	// Get old and new PV objects
	oldPv, ok := oldObj.(*v1.PersistentVolume)
	if oldPv == nil || !ok {
		klog.Warningf("PVUpdated: unrecognized old object %+v", oldObj)
		return
	}

	newPv, ok := newObj.(*v1.PersistentVolume)
	if newPv == nil || !ok {
		klog.Warningf("PVUpdated: unrecognized new object %+v", newObj)
		return
	}
	klog.V(4).Infof("PVUpdated: PV Updated from %+v to %+v", oldPv, newPv)

	// Verify if pv is a vsphere csi volume
	if oldPv.Spec.CSI == nil || newPv.Spec.CSI == nil || newPv.Spec.CSI.Driver != csitypes.Name {
		klog.V(3).Infof("PVUpdated: PV is not a Vsphere CSI Volume: %+v", newPv)
		return
	}
	// Return if new PV status is Pending or Failed
	if newPv.Status.Phase == v1.VolumePending || newPv.Status.Phase == v1.VolumeFailed {
		klog.V(3).Infof("PVUpdated: PV %s metadata is not updated since updated PV is in phase %s", newPv.Name, newPv.Status.Phase)
		return
	}
	// Return if labels are unchanged
	if oldPv.Status.Phase == v1.VolumeAvailable && reflect.DeepEqual(newPv.GetLabels(), oldPv.GetLabels()) {
		klog.V(3).Infof("PVUpdated: PV labels have not changed")
		return
	}
	if oldPv.Status.Phase == v1.VolumeBound && newPv.Status.Phase == v1.VolumeReleased && oldPv.Spec.PersistentVolumeReclaimPolicy == v1.PersistentVolumeReclaimDelete {
		klog.V(3).Infof("PVUpdated: Volume will be deleted by controller")
		return
	}
	if newPv.DeletionTimestamp != nil {
		klog.V(3).Infof("PVUpdated: PV already deleted")
		return
	}
	if metadataSyncer.clusterFlavor == cnstypes.CnsClusterFlavorGuest {
		// Invoke volume updated method for pvCSI
		pvcsiVolumeUpdated(newPv, newPv.Spec.CSI.VolumeHandle, metadataSyncer)
	} else {
		csiPVUpdated(newPv, oldPv, metadataSyncer)
	}
}

// pvDeleted deletes volume metadata on VC when volume has been deleted on K8s cluster
func pvDeleted(obj interface{}, metadataSyncer *metadataSyncInformer) {
	pv, ok := obj.(*v1.PersistentVolume)
	if pv == nil || !ok {
		klog.Warningf("PVDeleted: unrecognized object %+v", obj)
		return
	}
	klog.V(4).Infof("PVDeleted: Deleting PV: %+v", pv)

	// Verify if pv is a vsphere csi volume
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != csitypes.Name {
		klog.V(3).Infof("PVDeleted: Not a Vsphere CSI Volume: %+v", pv)
		return
	}

	if metadataSyncer.clusterFlavor == cnstypes.CnsClusterFlavorGuest {
		// Invoke volume deleted method for pvCSI
		pvcsiVolumeDeleted(string(pv.GetUID()), metadataSyncer)
	} else {
		csiPVDeleted(pv, metadataSyncer)
	}
}

// podUpdated updates pod metadata on VC when pod labels have been updated on K8s cluster
func podUpdated(oldObj, newObj interface{}, metadataSyncer *metadataSyncInformer) {
	// Get old and new pod objects
	oldPod, ok := oldObj.(*v1.Pod)
	if oldPod == nil || !ok {
		klog.Warningf("PodUpdated: unrecognized old object %+v", oldObj)
		return
	}
	newPod, ok := newObj.(*v1.Pod)
	if newPod == nil || !ok {
		klog.Warningf("PodUpdated: unrecognized new object %+v", newObj)
		return
	}

	// If old pod is in pending state and new pod is running, update metadata
	if oldPod.Status.Phase == v1.PodPending && newPod.Status.Phase == v1.PodRunning {

		klog.V(3).Infof("PodUpdated: Pod %s calling updatePodMetadata", newPod.Name)
		// Update pod metadata
		if errorList := updatePodMetadata(newPod, metadataSyncer, false); len(errorList) > 0 {
			klog.Errorf("PodUpdated: updatePodMetadata failed for pod %s with errors: ", newPod.Name)
			for _, err := range errorList {
				klog.Errorf("PodUpdated: %v", err)
			}
		}
	}
}

// pvDeleted deletes pod metadata on VC when pod has been deleted on K8s cluster
func podDeleted(obj interface{}, metadataSyncer *metadataSyncInformer) {
	// Get pod object
	pod, ok := obj.(*v1.Pod)
	if pod == nil || !ok {
		klog.Warningf("PodDeleted: unrecognized new object %+v", obj)
		return
	}

	if pod.Status.Phase == v1.PodPending {
		return
	}

	klog.V(3).Infof("PodDeleted: Pod %s calling updatePodMetadata", pod.Name)
	// Update pod metadata
	if errorList := updatePodMetadata(pod, metadataSyncer, true); len(errorList) > 0 {
		klog.Errorf("PodDeleted: updatePodMetadata failed for pod %s with errors: ", pod.Name)
		for _, err := range errorList {
			klog.Errorf("PodDeleted: %v", err)
		}

	}
}

// updatePodMetadata updates metadata for volumes attached to the pod
func updatePodMetadata(pod *v1.Pod, metadataSyncer *metadataSyncInformer, deleteFlag bool) []error {
	var errorList []error
	// Iterate through volumes attached to pod
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			pvcName := volume.PersistentVolumeClaim.ClaimName
			// Get pvc attached to pod
			pvc, err := metadataSyncer.pvcLister.PersistentVolumeClaims(pod.Namespace).Get(pvcName)
			if err != nil {
				msg := fmt.Sprintf("Error getting Persistent Volume Claim for volume %s with err: %v", volume.Name, err)
				errorList = append(errorList, errors.New(msg))
				continue
			}

			// Get pv object attached to pvc
			pv, err := metadataSyncer.pvLister.Get(pvc.Spec.VolumeName)
			if err != nil {
				msg := fmt.Sprintf("Error getting Persistent Volume for PVC %s in volume %s with err: %v", pvc.Name, volume.Name, err)
				errorList = append(errorList, errors.New(msg))
				continue
			}

			// Verify if pv is vsphere csi volume
			if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != csitypes.Name {
				klog.V(3).Infof("Not a vSphere CSI Volume")
				continue
			}
			var metadataList []cnstypes.BaseCnsEntityMetadata
			var podMetadata *cnstypes.CnsKubernetesEntityMetadata
			if deleteFlag == false {
				entityReference := cnsvsphere.CreateCnsKuberenetesEntityReference(string(cnstypes.CnsKubernetesEntityTypePVC), pvc.Name, pvc.Namespace)
				podMetadata = cnsvsphere.GetCnsKubernetesEntityMetaData(pod.Name, nil, deleteFlag, string(cnstypes.CnsKubernetesEntityTypePOD), pod.Namespace, metadataSyncer.configInfo.Cfg.Global.ClusterID, []cnstypes.CnsKubernetesEntityReference{entityReference})
			} else {
				podMetadata = cnsvsphere.GetCnsKubernetesEntityMetaData(pod.Name, nil, deleteFlag, string(cnstypes.CnsKubernetesEntityTypePOD), pod.Namespace, metadataSyncer.configInfo.Cfg.Global.ClusterID, nil)
			}
			metadataList = append(metadataList, cnstypes.BaseCnsEntityMetadata(podMetadata))
			containerCluster := cnsvsphere.GetContainerCluster(metadataSyncer.configInfo.Cfg.Global.ClusterID, metadataSyncer.configInfo.Cfg.VirtualCenter[metadataSyncer.host].User, metadataSyncer.clusterFlavor)

			updateSpec := &cnstypes.CnsVolumeMetadataUpdateSpec{
				VolumeId: cnstypes.CnsVolumeId{
					Id: pv.Spec.CSI.VolumeHandle,
				},
				Metadata: cnstypes.CnsVolumeMetadata{
					ContainerCluster:      containerCluster,
					ContainerClusterArray: []cnstypes.CnsContainerCluster{containerCluster},
					EntityMetadata:        metadataList,
				},
			}

			klog.V(4).Infof("Calling UpdateVolumeMetadata for volume %s with updateSpec: %+v", updateSpec.VolumeId.Id, spew.Sdump(updateSpec))
			if err := metadataSyncer.volumeManager.UpdateVolumeMetadata(updateSpec); err != nil {
				msg := fmt.Sprintf("UpdateVolumeMetadata failed for volume %s with err: %v", volume.Name, err)
				errorList = append(errorList, errors.New(msg))
			}
		}
	}
	return errorList
}

// csiPVCUpdated updates volume metadata for PVC objects on the VC in Vanilla k8s and supervisor cluster
func csiPVCUpdated(pvc *v1.PersistentVolumeClaim, pv *v1.PersistentVolume, metadataSyncer *metadataSyncInformer) {
	// Create updateSpec
	var metadataList []cnstypes.BaseCnsEntityMetadata
	entityReference := cnsvsphere.CreateCnsKuberenetesEntityReference(string(cnstypes.CnsKubernetesEntityTypePV), pv.Name, "")
	pvcMetadata := cnsvsphere.GetCnsKubernetesEntityMetaData(pvc.Name, pvc.Labels, false, string(cnstypes.CnsKubernetesEntityTypePVC), pvc.Namespace, metadataSyncer.configInfo.Cfg.Global.ClusterID, []cnstypes.CnsKubernetesEntityReference{entityReference})

	metadataList = append(metadataList, cnstypes.BaseCnsEntityMetadata(pvcMetadata))
	containerCluster := cnsvsphere.GetContainerCluster(metadataSyncer.configInfo.Cfg.Global.ClusterID, metadataSyncer.configInfo.Cfg.VirtualCenter[metadataSyncer.host].User, metadataSyncer.clusterFlavor)

	updateSpec := &cnstypes.CnsVolumeMetadataUpdateSpec{
		VolumeId: cnstypes.CnsVolumeId{
			Id: pv.Spec.CSI.VolumeHandle,
		},
		Metadata: cnstypes.CnsVolumeMetadata{
			ContainerCluster:      containerCluster,
			ContainerClusterArray: []cnstypes.CnsContainerCluster{containerCluster},
			EntityMetadata:        metadataList,
		},
	}

	klog.V(4).Infof("PVCUpdated: Calling UpdateVolumeMetadata with updateSpec: %+v", spew.Sdump(updateSpec))
	if err := metadataSyncer.volumeManager.UpdateVolumeMetadata(updateSpec); err != nil {
		klog.Errorf("PVCUpdated: UpdateVolumeMetadata failed with err %v", err)
	}
}

// csiPVCDeleted deletes volume metadata on VC when volume has been deleted on Vanilla k8s and supervisor cluster
func csiPVCDeleted(pvc *v1.PersistentVolumeClaim, pv *v1.PersistentVolume, metadataSyncer *metadataSyncInformer) {
	// Volume will be deleted by controller when reclaim policy is delete
	if pv.Spec.PersistentVolumeReclaimPolicy == v1.PersistentVolumeReclaimDelete {
		klog.V(3).Infof("PVCDeleted: Reclaim policy is delete")
		return
	}

	// If the PV reclaim policy is retain we need to delete PVC labels
	var metadataList []cnstypes.BaseCnsEntityMetadata
	pvcMetadata := cnsvsphere.GetCnsKubernetesEntityMetaData(pvc.Name, nil, true, string(cnstypes.CnsKubernetesEntityTypePVC), pvc.Namespace, metadataSyncer.configInfo.Cfg.Global.ClusterID, nil)
	metadataList = append(metadataList, cnstypes.BaseCnsEntityMetadata(pvcMetadata))

	containerCluster := cnsvsphere.GetContainerCluster(metadataSyncer.configInfo.Cfg.Global.ClusterID, metadataSyncer.configInfo.Cfg.VirtualCenter[metadataSyncer.host].User, metadataSyncer.clusterFlavor)
	updateSpec := &cnstypes.CnsVolumeMetadataUpdateSpec{
		VolumeId: cnstypes.CnsVolumeId{
			Id: pv.Spec.CSI.VolumeHandle,
		},
		Metadata: cnstypes.CnsVolumeMetadata{
			ContainerCluster:      containerCluster,
			ContainerClusterArray: []cnstypes.CnsContainerCluster{containerCluster},
			EntityMetadata:        metadataList,
		},
	}

	klog.V(4).Infof("PVCDeleted: Calling UpdateVolumeMetadata for volume %s with updateSpec: %+v", updateSpec.VolumeId.Id, spew.Sdump(updateSpec))
	if err := metadataSyncer.volumeManager.UpdateVolumeMetadata(updateSpec); err != nil {
		klog.Errorf("PVCDeleted: UpdateVolumeMetadata failed with err %v", err)
	}
}

// csiPVUpdated updates volume metadata on VC when volume labels on Vanilla k8s and supervisor cluster have been updated
func csiPVUpdated(newPv *v1.PersistentVolume, oldPv *v1.PersistentVolume, metadataSyncer *metadataSyncInformer) {
	var metadataList []cnstypes.BaseCnsEntityMetadata
	pvMetadata := cnsvsphere.GetCnsKubernetesEntityMetaData(newPv.Name, newPv.GetLabels(), false, string(cnstypes.CnsKubernetesEntityTypePV), "", metadataSyncer.configInfo.Cfg.Global.ClusterID, nil)
	metadataList = append(metadataList, cnstypes.BaseCnsEntityMetadata(pvMetadata))

	containerCluster := cnsvsphere.GetContainerCluster(metadataSyncer.configInfo.Cfg.Global.ClusterID, metadataSyncer.configInfo.Cfg.VirtualCenter[metadataSyncer.host].User, metadataSyncer.clusterFlavor)
	if oldPv.Status.Phase == v1.VolumePending && newPv.Status.Phase == v1.VolumeAvailable && newPv.Spec.StorageClassName == "" {
		// Static PV is Created
		var volumeType string
		if oldPv.Spec.CSI.FSType == common.NfsV4FsType || oldPv.Spec.CSI.FSType == common.NfsFsType {
			volumeType = common.FileVolumeType
		} else {
			volumeType = common.BlockVolumeType
		}
		klog.V(4).Infof("PVUpdated: observed static volume provisioning for the PV: %q with volumeType: %q", newPv.Name, volumeType)
		queryFilter := cnstypes.CnsQueryFilter{
			VolumeIds: []cnstypes.CnsVolumeId{{Id: oldPv.Spec.CSI.VolumeHandle}},
		}
		volumeOperationsLock.Lock()
		defer volumeOperationsLock.Unlock()
		queryResult, err := metadataSyncer.volumeManager.QueryVolume(queryFilter)
		if err != nil {
			klog.Errorf("PVUpdated: QueryVolume failed. error: %+v", err)
			return
		}
		if len(queryResult.Volumes) == 0 {
			klog.V(2).Infof("PVUpdated: Verified volume: %q is not marked as container volume in CNS. Calling CreateVolume with BackingID to mark volume as Container Volume.", oldPv.Spec.CSI.VolumeHandle)
			// Call CreateVolume for Static Volume Provisioning
			createSpec := &cnstypes.CnsVolumeCreateSpec{
				Name:       oldPv.Name,
				VolumeType: volumeType,
				Metadata: cnstypes.CnsVolumeMetadata{
					ContainerCluster:      containerCluster,
					ContainerClusterArray: []cnstypes.CnsContainerCluster{containerCluster},
					EntityMetadata:        metadataList,
				},
			}

			if volumeType == common.BlockVolumeType {
				createSpec.BackingObjectDetails = &cnstypes.CnsBlockBackingDetails{
					CnsBackingObjectDetails: cnstypes.CnsBackingObjectDetails{},
					BackingDiskId:           oldPv.Spec.CSI.VolumeHandle,
				}
			} else {
				createSpec.BackingObjectDetails = &cnstypes.CnsNfsFileShareBackingDetails{
					CnsFileBackingDetails: cnstypes.CnsFileBackingDetails{
						BackingFileId: oldPv.Spec.CSI.VolumeHandle,
					},
				}
			}
			klog.V(4).Infof("PVUpdated: vSphere CSI Driver is creating volume %q with create spec %+v", oldPv.Name, spew.Sdump(createSpec))
			_, err := metadataSyncer.volumeManager.CreateVolume(createSpec)
			if err != nil {
				klog.Errorf("PVUpdated: Failed to create disk %s with error %+v", oldPv.Name, err)
			} else {
				klog.V(2).Infof("PVUpdated: vSphere CSI Driver has successfully marked volume: %q as the container volume.", oldPv.Spec.CSI.VolumeHandle)
			}
			// Volume is successfully created so returning from here.
			return
		} else if queryResult.Volumes[0].VolumeId.Id == oldPv.Spec.CSI.VolumeHandle {
			klog.V(2).Infof("PVUpdated: Verified volume: %q is already marked as container volume in CNS.", oldPv.Spec.CSI.VolumeHandle)
			// Volume is already present in the CNS, so continue with the UpdateVolumeMetadata
		} else {
			klog.V(2).Infof("PVUpdated: Queried volume: %q is other than requested volume: %q.", oldPv.Spec.CSI.VolumeHandle, queryResult.Volumes[0].VolumeId.Id)
			// unknown Volume is returned from the CNS, so returning from here.
			return
		}
	}
	// call UpdateVolumeMetadata for all other cases
	updateSpec := &cnstypes.CnsVolumeMetadataUpdateSpec{
		VolumeId: cnstypes.CnsVolumeId{
			Id: newPv.Spec.CSI.VolumeHandle,
		},
		Metadata: cnstypes.CnsVolumeMetadata{
			ContainerCluster:      containerCluster,
			ContainerClusterArray: []cnstypes.CnsContainerCluster{containerCluster},
			EntityMetadata:        metadataList,
		},
	}

	klog.V(4).Infof("PVUpdated: Calling UpdateVolumeMetadata for volume %q with updateSpec: %+v", updateSpec.VolumeId.Id, spew.Sdump(updateSpec))
	if err := metadataSyncer.volumeManager.UpdateVolumeMetadata(updateSpec); err != nil {
		klog.Errorf("PVUpdated: UpdateVolumeMetadata failed with err %v", err)
		return
	}
	klog.V(4).Infof("PVUpdated: UpdateVolumeMetadata succeed for the volume %q with updateSpec: %+v", updateSpec.VolumeId.Id, spew.Sdump(updateSpec))
}

// csiPVDeleted deletes volume metadata on VC when volume has been deleted on Vanills k8s and supervisor cluster
func csiPVDeleted(pv *v1.PersistentVolume, metadataSyncer *metadataSyncInformer) {
	var deleteDisk bool
	if pv.Spec.ClaimRef != nil && (pv.Status.Phase == v1.VolumeAvailable || pv.Status.Phase == v1.VolumeReleased) && pv.Spec.PersistentVolumeReclaimPolicy == v1.PersistentVolumeReclaimDelete {
		klog.V(3).Infof("PVDeleted: Volume deletion will be handled by Controller")
		return
	}
	volumeOperationsLock.Lock()
	defer volumeOperationsLock.Unlock()

	if pv.Spec.CSI.FSType == common.NfsV4FsType || pv.Spec.CSI.FSType == common.NfsFsType {
		// TODO: Query CNS and Check if this is the last entity reference for the Volume, if Yes then call delete with
		// deleteDisk set to true.
		// Make sure to follow similar logic in the full sync.
		klog.V(4).Infof("PVDeleted: vSphere CSI Driver is calling UpdateVolumeMetadata to delete volume metadata references for PV: %q", pv.Name)
		var metadataList []cnstypes.BaseCnsEntityMetadata
		pvMetadata := cnsvsphere.GetCnsKubernetesEntityMetaData(pv.Name, nil, true, string(cnstypes.CnsKubernetesEntityTypePV), "", metadataSyncer.configInfo.Cfg.Global.ClusterID, nil)
		metadataList = append(metadataList, cnstypes.BaseCnsEntityMetadata(pvMetadata))

		containerCluster := cnsvsphere.GetContainerCluster(metadataSyncer.configInfo.Cfg.Global.ClusterID, metadataSyncer.configInfo.Cfg.VirtualCenter[metadataSyncer.host].User, metadataSyncer.clusterFlavor)
		updateSpec := &cnstypes.CnsVolumeMetadataUpdateSpec{
			VolumeId: cnstypes.CnsVolumeId{
				Id: pv.Spec.CSI.VolumeHandle,
			},
			Metadata: cnstypes.CnsVolumeMetadata{
				ContainerCluster:      containerCluster,
				ContainerClusterArray: []cnstypes.CnsContainerCluster{containerCluster},
				EntityMetadata:        metadataList,
			},
		}

		klog.V(4).Infof("PVDeleted: Calling UpdateVolumeMetadata for volume %s with updateSpec: %+v", updateSpec.VolumeId.Id, spew.Sdump(updateSpec))
		if err := metadataSyncer.volumeManager.UpdateVolumeMetadata(updateSpec); err != nil {
			klog.Errorf("PVDeleted: UpdateVolumeMetadata failed with err %v", err)
		}
	} else {
		if pv.Spec.ClaimRef == nil || pv.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimDelete {
			klog.V(4).Infof("PVDeleted: Setting DeleteDisk to false")
			deleteDisk = false
		} else {
			// We set delete disk=true for the case where PV status is failed after deletion of pvc
			// In this case, metadatasyncer will remove the volume
			klog.V(4).Infof("PVDeleted: Setting DeleteDisk to true")
			deleteDisk = true
		}
		klog.V(4).Infof("PVDeleted: vSphere CSI Driver is deleting volume %v with delete disk %v", pv, deleteDisk)
		if err := metadataSyncer.volumeManager.DeleteVolume(pv.Spec.CSI.VolumeHandle, deleteDisk); err != nil {
			klog.Errorf("PVDeleted: Failed to delete disk %s with error %+v", pv.Spec.CSI.VolumeHandle, err)
		}
	}
}
