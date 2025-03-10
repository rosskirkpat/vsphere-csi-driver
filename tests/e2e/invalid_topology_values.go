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

package e2e

import (
	"context"
	"fmt"
	"strings"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	fnodes "k8s.io/kubernetes/test/e2e/framework/node"
	fpv "k8s.io/kubernetes/test/e2e/framework/pv"
)

// Constants to store invalid/non-existing region and zone
const (
	NonExistingRegion = "NonExistingRegion"
	NonExistingZone   = "NonExistingZone"
)

var _ = ginkgo.Describe("[csi-topology-vanilla] Topology-Aware-Provisioning-With-Invalid-Zone-And-Region", func() {
	f := framework.NewDefaultFramework("e2e-vsphere-topology-aware-provisioning")
	var (
		client            clientset.Interface
		namespace         string
		allowedTopologies []v1.TopologySelectorLabelRequirement
		nodeList          *v1.NodeList
		pvclaim           *v1.PersistentVolumeClaim
		storageclass      *storagev1.StorageClass
		err               error
		regionZoneValue   string
	)
	ginkgo.BeforeEach(func() {
		client = f.ClientSet
		namespace = f.Namespace.Name
		bootstrap()
		nodeList, err = fnodes.GetReadySchedulableNodes(f.ClientSet)
		framework.ExpectNoError(err, "Unable to find ready and schedulable Node")
		if !(len(nodeList.Items) > 0) {
			framework.Failf("Unable to find ready and schedulable Node")
		}
		// Preparing allowedTopologies using topologies with shared and non shared datastores
		regionZoneValue = GetAndExpectStringEnvVar(envRegionZoneWithSharedDS)
		_, _, allowedTopologies = topologyParameterForStorageClass(regionZoneValue)
	})

	/*
		Test to verify provisioning with Zone/Region without any nodes in that zone/region.
		Provisioning should fail.

		Steps
		1. Create a Storage Class with non existent region specified in “AllowedTopologies”.
		2. Create a PVC with above SC.
		3. Provisioning should fail.
		4. Delete PVC
		5. Delete SC
	*/
	ginkgo.It("Verify provisioning fails with region and zone having no nodes specified in the storage class", func() {
		var cancel context.CancelFunc
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		topologyWithNoNodes := NonExistingRegion + ":" + NonExistingZone
		_, _, allowedTopologies = topologyParameterForStorageClass(topologyWithNoNodes)
		storageclass, pvclaim, err = createPVCAndStorageClass(client,
			namespace, nil, nil, "", allowedTopologies, "", false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			err = client.StorageV1().StorageClasses().Delete(ctx, storageclass.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fpv.DeletePersistentVolumeClaim(client, pvclaim.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()
		ginkgo.By("Expect claim to fail provisioning volume within the topology")
		framework.ExpectError(fpv.WaitForPersistentVolumeClaimPhase(v1.ClaimBound,
			client, pvclaim.Namespace, pvclaim.Name, framework.PollShortTimeout, pollTimeoutShort))
		// Get the event list and verify if it contains expected error message
		eventList, _ := client.CoreV1().Events(pvclaim.Namespace).List(ctx, metav1.ListOptions{})
		gomega.Expect(eventList.Items).NotTo(gomega.BeEmpty())
		actualErrMsg := eventList.Items[len(eventList.Items)-1].Message
		framework.Logf(fmt.Sprintf("Actual failure message: %+q", actualErrMsg))
		expectedErrMsg := "failed to get shared datastores for topology requirement"
		framework.Logf(fmt.Sprintf("Expected failure message: %+q", expectedErrMsg))
		gomega.Expect(strings.Contains(actualErrMsg, expectedErrMsg)).To(gomega.BeTrue(),
			fmt.Sprintf("actualErrMsg: %q does not contain expectedErrMsg: %q", actualErrMsg, expectedErrMsg))

	})

	/*
		Test to verify provisioning with non existent Region specified in Storage Class.
		Provisioning should fail.

		Steps
		1. Create a Storage Class with non existent region specified in “AllowedTopologies”.
		2. Create a PVC with above SC.
		3. Provisioning should fail.
		4. Delete PVC
		5. Delete SC
	*/
	ginkgo.It("Verify provisioning fails with non existing region specified in the storage class", func() {
		// Topology value = <NonExistingRegion>:<zone-with-shared-datastore>
		var cancel context.CancelFunc
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		regionZone := strings.Split(regionZoneValue, ":")
		inputZone := regionZone[1]
		topologyNonExistingRegion := NonExistingRegion + ":" + inputZone
		_, _, allowedTopologies = topologyParameterForStorageClass(topologyNonExistingRegion)
		storageclass, pvclaim, err = createPVCAndStorageClass(client,
			namespace, nil, nil, "", allowedTopologies, "", false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			err = client.StorageV1().StorageClasses().Delete(ctx, storageclass.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fpv.DeletePersistentVolumeClaim(client, pvclaim.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By("Expect claim to fail provisioning volume within the topology")
		framework.ExpectError(fpv.WaitForPersistentVolumeClaimPhase(v1.ClaimBound,
			client, pvclaim.Namespace, pvclaim.Name, pollTimeoutShort, framework.PollShortTimeout))
		// Get the event list and verify if it contains expected error message
		eventList, _ := client.CoreV1().Events(pvclaim.Namespace).List(ctx, metav1.ListOptions{})
		gomega.Expect(eventList.Items).NotTo(gomega.BeEmpty())
		actualErrMsg := eventList.Items[len(eventList.Items)-1].Message
		framework.Logf(fmt.Sprintf("Actual failure message: %+q", actualErrMsg))
		expectedErrMsg := "failed to get shared datastores for topology requirement"
		framework.Logf(fmt.Sprintf("Expected failure message: %+q", expectedErrMsg))
		gomega.Expect(strings.Contains(actualErrMsg, expectedErrMsg)).To(gomega.BeTrue(),
			fmt.Sprintf("actualErrMsg: %q does not contain expectedErrMsg: %q", actualErrMsg, expectedErrMsg))
	})

	/*
		Test to verify provisioning with non existent Zone with valid Region specified in Storage Class.
		Provisioning should fail.

		Steps
		1. Create a Storage Class with valid region but non existent zone specified in “AllowedTopologies”.
		2. Create a PVC with above SC.
		3. Provisioning should fail.
		4. Delete PVC
		5. Delete SC
	*/
	ginkgo.It("Verify provisioning fails with valid region and non existing zone specified in the storage class", func() {
		var cancel context.CancelFunc
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Topology value = <region-with-shared-datastore>:<NonExisitingZone>
		regionZone := strings.Split(regionZoneValue, ":")
		inputRegion := regionZone[0]
		topologyNonExistingZone := inputRegion + ":" + NonExistingZone
		_, _, allowedTopologies = topologyParameterForStorageClass(topologyNonExistingZone)
		storageclass, pvclaim, err = createPVCAndStorageClass(client,
			namespace, nil, nil, "", allowedTopologies, "", false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			err = client.StorageV1().StorageClasses().Delete(ctx, storageclass.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fpv.DeletePersistentVolumeClaim(client, pvclaim.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()
		ginkgo.By("Expect claim to fail provisioning volume within the topology")
		framework.ExpectError(fpv.WaitForPersistentVolumeClaimPhase(v1.ClaimBound, client,
			pvclaim.Namespace, pvclaim.Name, pollTimeoutShort, framework.PollShortTimeout))
		// Get the event list and verify if it contains expected error message

		eventList, _ := client.CoreV1().Events(pvclaim.Namespace).List(ctx, metav1.ListOptions{})
		gomega.Expect(eventList.Items).NotTo(gomega.BeEmpty())
		actualErrMsg := eventList.Items[len(eventList.Items)-1].Message
		framework.Logf(fmt.Sprintf("Actual failure message: %+q", actualErrMsg))
		expectedErrMsg := "failed to get shared datastores for topology requirement"
		framework.Logf(fmt.Sprintf("Expected failure message: %+q", expectedErrMsg))
		gomega.Expect(strings.Contains(actualErrMsg, expectedErrMsg)).To(gomega.BeTrue(),
			fmt.Sprintf("actualErrMsg: %q does not contain expectedErrMsg: %q", actualErrMsg, expectedErrMsg))
	})
})
