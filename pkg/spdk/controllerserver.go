/*
Copyright (c) Arm Limited and Contributors.

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

package spdk

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog"

	csicommon "github.com/spdk/spdk-csi/pkg/csi-common"
	"github.com/spdk/spdk-csi/pkg/util"
)

var errVolumeInCreation = status.Error(codes.Internal, "volume in creation")

type controllerServer struct {
	*csicommon.DefaultControllerServer
	spdkNodes   map[string]util.SpdkNode // all spdk nodes in cluster
	volumeLocks *util.VolumeLocks
}

type spdkVolume struct {
	lvolID   string
	nodeName string
}

func (cs *controllerServer) CreateVolume(_ context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	volumeID := req.GetName()
	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()

	csiVolume, err := cs.createVolume(req)
	if err != nil {
		klog.Errorf("failed to create volume, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	volumeInfo, err := cs.publishVolume(csiVolume.GetVolumeId())
	if err != nil {
		klog.Errorf("failed to publish volume, volumeID: %s err: %v", volumeID, err)
		cs.deleteVolume(csiVolume.GetVolumeId()) //nolint:errcheck // we can do little
		return nil, status.Error(codes.Internal, err.Error())
	}
	// copy volume info. node needs these info to contact target(ip, port, nqn, ...)
	if csiVolume.VolumeContext == nil {
		csiVolume.VolumeContext = volumeInfo
	} else {
		for k, v := range volumeInfo {
			csiVolume.VolumeContext[k] = v
		}
	}

	return &csi.CreateVolumeResponse{Volume: csiVolume}, nil
}

func (cs *controllerServer) DeleteVolume(_ context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()
	// no harm if volume already unpublished
	err := cs.unpublishVolume(volumeID)
	switch {
	case errors.Is(err, util.ErrVolumeUnpublished):
		// unpublished but not deleted in last request?
		klog.Warningf("volume not published: %s", volumeID)
	case errors.Is(err, util.ErrVolumeDeleted):
		// deleted in previous request?
		klog.Warningf("volume already deleted: %s", volumeID)
	case err != nil:
		klog.Errorf("failed to unpublish volume, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	// no harm if volume already deleted
	err = cs.deleteVolume(volumeID)
	if errors.Is(err, util.ErrJSONNoSuchDevice) {
		// deleted in previous request?
		klog.Warningf("volume not exists: %s", volumeID)
	} else if err != nil {
		klog.Errorf("failed to delete volume, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	// make sure we support all requested caps
	for _, cap := range req.VolumeCapabilities {
		supported := false
		for _, accessMode := range cs.Driver.GetVolumeCapabilityAccessModes() {
			if cap.GetAccessMode().GetMode() == accessMode.GetMode() {
				supported = true
				break
			}
		}
		if !supported {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: ""}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
		},
	}, nil
}

func (cs *controllerServer) CreateSnapshot(_ context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	volumeID := req.GetSourceVolumeId()
	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()

	snapshotName := req.GetName()
	spdkVol, err := getSPDKVol(volumeID)
	if err != nil {
		klog.Errorf("failed to get spdk volume, volumeID: %s err: %v", volumeID, err)
		return nil, err
	}

	snapshotID, err := cs.spdkNodes[spdkVol.nodeName].CreateSnapshot(spdkVol.lvolID, snapshotName)
	if err != nil {
		klog.Errorf("failed to create snapshot, volumeID: %s snapshotName: %s err: %v", volumeID, snapshotName, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	volInfo, err := cs.spdkNodes[spdkVol.nodeName].VolumeInfo(spdkVol.lvolID)
	if err != nil {
		klog.Errorf("failed to get volume info, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	size, err := strconv.ParseInt(volInfo["lvolSize"], 10, 64)
	if err != nil {
		klog.Errorf("failed to parse volume size, lvolSize: %s err: %v", volInfo["lvolSize"], err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	creationTime := timestamppb.Now()
	snapshotData := csi.Snapshot{
		SizeBytes:      size,
		SnapshotId:     fmt.Sprintf("%s:%s", spdkVol.nodeName, snapshotID),
		SourceVolumeId: spdkVol.lvolID,
		CreationTime:   creationTime,
		ReadyToUse:     true,
	}

	return &csi.CreateSnapshotResponse{
		Snapshot: &snapshotData,
	}, nil
}

func (cs *controllerServer) DeleteSnapshot(_ context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	snapshotID := req.GetSnapshotId()
	unlock := cs.volumeLocks.Lock(snapshotID)
	defer unlock()

	spdkVol, err := getSPDKVol(snapshotID)
	if err != nil {
		klog.Errorf("failed to get spdk volume, snapshotID: %s err: %v", snapshotID, err)
		return nil, err
	}

	err = cs.spdkNodes[spdkVol.nodeName].DeleteVolume(spdkVol.lvolID)
	if err != nil {
		klog.Errorf("failed to delete snapshot, snapshotID: %s err: %v", snapshotID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.DeleteSnapshotResponse{}, nil
}

func (cs *controllerServer) createVolume(req *csi.CreateVolumeRequest) (*csi.Volume, error) {
	size := req.GetCapacityRange().GetRequiredBytes()
	if size == 0 {
		klog.Warningln("invalid volume size, resize to 1G")
		size = 1024 * 1024 * 1024
	}
	sizeMiB := util.ToMiB(size)
	vol := csi.Volume{
		CapacityBytes: sizeMiB * 1024 * 1024,
		VolumeContext: req.GetParameters(),
		ContentSource: req.GetVolumeContentSource(),
	}

	// check all SPDK nodes to see if the volume has already been created
	for nodeName, node := range cs.spdkNodes {
		lvStores, err := node.LvStores()
		if err != nil {
			return nil, fmt.Errorf("get lvstores of node:%s failed: %w", nodeName, err)
		}
		for lvsIdx := range lvStores {
			volumeID, err := node.GetVolume(req.GetName(), lvStores[lvsIdx].Name)
			if err == nil {
				vol.VolumeId = fmt.Sprintf("%s:%s", nodeName, volumeID)
				return &vol, nil
			}
		}
	}
	// schedule a SPDK node/lvstore to create the volume.
	// if volume content source is specified, using the same node/lvstore as the source volume.
	var err error
	var volumeID string
	if req.GetVolumeContentSource() == nil {
		// schedule suitable node:lvstore
		nodeName, lvstore, err2 := cs.schedule(sizeMiB)
		if err2 != nil {
			return nil, err2
		}
		// TODO: re-schedule on ErrJSONNoSpaceLeft per optimistic concurrency control
		// create a new volume
		volumeID, err = cs.spdkNodes[nodeName].CreateVolume(req.GetName(), lvstore, sizeMiB)
		// in the subsequent DeleteVolume() request, a nodeName needs to be specified,
		// but the current CSI mechanism only passes the VolumeId to DeleteVolume().
		// therefore, the nodeName is included as part of the VolumeId.
		vol.VolumeId = fmt.Sprintf("%s:%s", nodeName, volumeID)
	} else {
		// find the node/lvstore of the specified content source volume
		nodeName, lvstore, sourceLvolID, err2 := cs.getSnapshotInfo(req.GetVolumeContentSource())
		if err2 != nil {
			return nil, err2
		}
		// create a volume cloned from the source volume
		volumeID, err = cs.spdkNodes[nodeName].CloneVolume(req.GetName(), lvstore, sourceLvolID)
		vol.VolumeId = fmt.Sprintf("%s:%s", nodeName, volumeID)
	}

	if err != nil {
		return nil, err
	}
	return &vol, nil
}

func getSPDKVol(csiVolumeID string) (*spdkVolume, error) {
	// extract spdkNodeName and spdkLvolID from csiVolumeID
	// csiVolumeID: node001:8e2dcb9d-3a79-4362-965e-fdb0cd3f4b8d
	// spdkNodeName: node001
	// spdklvolID: 8e2dcb9d-3a79-4362-965e-fdb0cd3f4b8d

	ids := strings.Split(csiVolumeID, ":")
	if len(ids) == 2 {
		return &spdkVolume{
			nodeName: ids[0],
			lvolID:   ids[1],
		}, nil
	}
	return nil, fmt.Errorf("missing nodeName in volume: %s", csiVolumeID)
}

func (cs *controllerServer) publishVolume(volumeID string) (map[string]string, error) {
	spdkVol, err := getSPDKVol(volumeID)
	if err != nil {
		return nil, err
	}
	err = cs.spdkNodes[spdkVol.nodeName].PublishVolume(spdkVol.lvolID)
	if err != nil {
		return nil, err
	}

	volumeInfo, err := cs.spdkNodes[spdkVol.nodeName].VolumeInfo(spdkVol.lvolID)
	if err != nil {
		cs.unpublishVolume(volumeID) //nolint:errcheck // we can do little
		return nil, err
	}
	return volumeInfo, nil
}

func (cs *controllerServer) deleteVolume(volumeID string) error {
	spdkVol, err := getSPDKVol(volumeID)
	if err != nil {
		return err
	}
	return cs.spdkNodes[spdkVol.nodeName].DeleteVolume(spdkVol.lvolID)
}

func (cs *controllerServer) unpublishVolume(volumeID string) error {
	spdkVol, err := getSPDKVol(volumeID)
	if err != nil {
		return err
	}
	return cs.spdkNodes[spdkVol.nodeName].UnpublishVolume(spdkVol.lvolID)
}

func (cs *controllerServer) getSnapshotInfo(vcs *csi.VolumeContentSource) (
	nodeName, lvstore, sourceLvolID string, err error,
) {
	snapshotSource := vcs.GetSnapshot()

	if snapshotSource == nil {
		err = fmt.Errorf("invalid volume content source, only snapshot source is supported")
		return
	}
	snapSpdkVol, err := getSPDKVol(snapshotSource.GetSnapshotId())
	if err != nil {
		return
	}
	nodeName = snapSpdkVol.nodeName
	sourceLvolID = snapSpdkVol.lvolID

	sourceLvolInfo, err := cs.spdkNodes[nodeName].VolumeInfo(sourceLvolID)
	if err != nil {
		return
	}
	lvstore = sourceLvolInfo["lvstore"]
	return
}

// simplest volume scheduler: find first node:lvstore with enough free space
func (cs *controllerServer) schedule(sizeMiB int64) (nodeName, lvstore string, err error) {
	for name, spdkNode := range cs.spdkNodes {
		// retrieve latest lvstore info from spdk node
		lvstores, err := spdkNode.LvStores()
		if err != nil {
			klog.Errorf("failed to get lvstores from node %s: %s", spdkNode.Info(), err.Error())
			continue
		}
		// check if lvstore has enough free space
		for i := range lvstores {
			lvstore := &lvstores[i]
			if lvstore.FreeSizeMiB > sizeMiB {
				return name, lvstore.Name, nil
			}
		}
		klog.Infof("not enough free space from node %s", spdkNode.Info())
	}

	return "", "", fmt.Errorf("failed to find node with enough free space")
}

func newControllerServer(d *csicommon.CSIDriver) (*controllerServer, error) {
	server := controllerServer{
		DefaultControllerServer: csicommon.NewDefaultControllerServer(d),
		spdkNodes:               map[string]util.SpdkNode{},
		volumeLocks:             util.NewVolumeLocks(),
	}

	// get spdk node configs, see deploy/kubernetes/config-map.yaml
	//nolint:tagliatelle // not using json:snake case
	var config struct {
		Nodes []struct {
			Name       string `json:"name"`
			URL        string `json:"rpcURL"`
			TargetType string `json:"targetType"`
			TargetAddr string `json:"targetAddr"`
		} `json:"Nodes"`
	}
	configFile := util.FromEnv("SPDKCSI_CONFIG", "/etc/spdkcsi-config/config.json")
	err := util.ParseJSONFile(configFile, &config)
	if err != nil {
		return nil, err
	}

	// get spdk node secrets, see deploy/kubernetes/secret.yaml
	//nolint:tagliatelle // not using json:snake case
	var secret struct {
		Tokens []struct {
			Name     string `json:"name"`
			UserName string `json:"username"`
			Password string `json:"password"`
		} `json:"rpcTokens"`
	}
	secretFile := util.FromEnv("SPDKCSI_SECRET", "/etc/spdkcsi-secret/secret.json")
	err = util.ParseJSONFile(secretFile, &secret)
	if err != nil {
		return nil, err
	}

	// create spdk nodes
	for i := range config.Nodes {
		node := &config.Nodes[i]
		tokenFound := false
		// find secret per node
		for j := range secret.Tokens {
			token := &secret.Tokens[j]
			if token.Name == node.Name {
				tokenFound = true
				spdkNode, err := util.NewSpdkNode(node.URL, token.UserName, token.Password, node.TargetType, node.TargetAddr)
				if err != nil {
					klog.Errorf("failed to create spdk node %s: %s", node.Name, err.Error())
				} else {
					klog.Infof("spdk node created: name=%s, url=%s", node.Name, node.URL)
					server.spdkNodes[node.Name] = spdkNode
				}
				break
			}
		}
		if !tokenFound {
			klog.Errorf("failed to find secret for spdk node %s", node.Name)
		}
	}
	if len(server.spdkNodes) == 0 {
		return nil, fmt.Errorf("no valid spdk node found")
	}

	return &server, nil
}
