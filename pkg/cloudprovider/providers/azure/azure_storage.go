/*
Copyright 2016 The Kubernetes Authors.

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

package azure

import (
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/arm/compute"
	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/types"
)

const (
	maxLUN = 64 // max number of LUNs per VM
)

// AttachDisk attaches a vhd to vm
// the vhd must exist, can be identified by diskName, diskURI, and lun.
func (az *Cloud) AttachDisk(diskName, diskURI string, nodeName types.NodeName, lun int32, cachingMode compute.CachingTypes) error {
	vm, exists, err := az.getVirtualMachine(nodeName)
	if err != nil {
		return err
	} else if !exists {
		return cloudprovider.InstanceNotFound
	}
	disks := *vm.Properties.StorageProfile.DataDisks
	disks = append(disks,
		compute.DataDisk{
			Name: &diskName,
			Vhd: &compute.VirtualHardDisk{
				URI: &diskURI,
			},
			Lun:          &lun,
			Caching:      cachingMode,
			CreateOption: "attach",
		})

	newVM := compute.VirtualMachine{
		Location: vm.Location,
		Properties: &compute.VirtualMachineProperties{
			StorageProfile: &compute.StorageProfile{
				DataDisks: &disks,
			},
		},
	}
	vmName := mapNodeNameToVMName(nodeName)
	_, err = az.VirtualMachinesClient.CreateOrUpdate(az.ResourceGroup, vmName, newVM, nil)
	if err != nil {
		glog.Errorf("azure attach failed, err: %v", err)
		detail := err.Error()
		if strings.Contains(detail, "Code=\"AcquireDiskLeaseFailed\"") {
			// if lease cannot be acquired, immediately detach the disk and return the original error
			glog.Infof("failed to acquire disk lease, try detach")
			az.DetachDiskByName(diskName, diskURI, nodeName)
		}
	} else {
		glog.V(4).Infof("azure attach succeeded")
	}
	return err
}

// DisksAreAttached checks if a list of volumes are attached to the node with the specified NodeName
func (az *Cloud) DisksAreAttached(diskNames []string, nodeName types.NodeName) (map[string]bool, error) {
	attached := make(map[string]bool)
	for _, diskName := range diskNames {
		attached[diskName] = false
	}
	vm, exists, err := az.getVirtualMachine(nodeName)
	if !exists {
		// if host doesn't exist, no need to detach
		glog.Warningf("Cannot find node %q, DisksAreAttached will assume disks %v are not attached to it.",
			nodeName, diskNames)
		return attached, nil
	} else if err != nil {
		return attached, err
	}

	disks := *vm.Properties.StorageProfile.DataDisks
	for _, disk := range disks {
		for _, diskName := range diskNames {
			if disk.Name != nil && diskName != "" && *disk.Name == diskName {
				attached[diskName] = true
			}
		}
	}

	return attached, nil
}

// DetachDiskByName detaches a vhd from host
// the vhd can be identified by diskName or diskURI
func (az *Cloud) DetachDiskByName(diskName, diskURI string, nodeName types.NodeName) error {
	vm, exists, err := az.getVirtualMachine(nodeName)
	if err != nil || !exists {
		// if host doesn't exist, no need to detach
		glog.Warningf("cannot find node %s, skip detaching disk %s", nodeName, diskName)
		return nil
	}

	disks := *vm.Properties.StorageProfile.DataDisks
	for i, disk := range disks {
		if (disk.Name != nil && diskName != "" && *disk.Name == diskName) || (disk.Vhd.URI != nil && diskURI != "" && *disk.Vhd.URI == diskURI) {
			// found the disk
			glog.V(4).Infof("detach disk: name %q uri %q", diskName, diskURI)
			disks = append(disks[:i], disks[i+1:]...)
			break
		}
	}
	newVM := compute.VirtualMachine{
		Location: vm.Location,
		Properties: &compute.VirtualMachineProperties{
			StorageProfile: &compute.StorageProfile{
				DataDisks: &disks,
			},
		},
	}
	vmName := mapNodeNameToVMName(nodeName)
	_, err = az.VirtualMachinesClient.CreateOrUpdate(az.ResourceGroup, vmName, newVM, nil)
	if err != nil {
		glog.Errorf("azure disk detach failed, err: %v", err)
	} else {
		glog.V(4).Infof("azure disk detach succeeded")
	}
	return err
}

// GetDiskLun finds the lun on the host that the vhd is attached to, given a vhd's diskName and diskURI
func (az *Cloud) GetDiskLun(diskName, diskURI string, nodeName types.NodeName) (int32, error) {
	vm, exists, err := az.getVirtualMachine(nodeName)
	if err != nil {
		return -1, err
	} else if !exists {
		return -1, cloudprovider.InstanceNotFound
	}
	disks := *vm.Properties.StorageProfile.DataDisks
	for _, disk := range disks {
		if disk.Lun != nil && (disk.Name != nil && diskName != "" && *disk.Name == diskName) || (disk.Vhd.URI != nil && diskURI != "" && *disk.Vhd.URI == diskURI) {
			// found the disk
			glog.V(4).Infof("find disk: lun %d name %q uri %q", *disk.Lun, diskName, diskURI)
			return *disk.Lun, nil
		}
	}
	return -1, fmt.Errorf("Cannot find Lun for disk %s", diskName)
}

// GetNextDiskLun searches all vhd attachment on the host and find unused lun
// return -1 if all luns are used
func (az *Cloud) GetNextDiskLun(nodeName types.NodeName) (int32, error) {
	vm, exists, err := az.getVirtualMachine(nodeName)
	if err != nil {
		return -1, err
	} else if !exists {
		return -1, cloudprovider.InstanceNotFound
	}
	used := make([]bool, maxLUN)
	disks := *vm.Properties.StorageProfile.DataDisks
	for _, disk := range disks {
		if disk.Lun != nil {
			used[*disk.Lun] = true
		}
	}
	for k, v := range used {
		if !v {
			return int32(k), nil
		}
	}
	return -1, fmt.Errorf("All Luns are used")
}
