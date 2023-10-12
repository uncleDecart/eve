// Copyright (c) 2023 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package zedrouter

import (
	"encoding/json"
	"fmt"
	"sync"

	uuid "github.com/satori/go.uuid"

	"github.com/lf-edge/eve/pkg/pillar/base"
	"github.com/lf-edge/eve/pkg/pillar/types"
	fileutils "github.com/lf-edge/eve/pkg/pillar/utils/file"
)

// PatchEnvelopes is a structure representing
// Patch Envelopes exposed to App instances via metadata server
// for more info check docs/PATCH-ENVELOPES.md
type PatchEnvelopes struct {
	sync.Mutex
	VolumeStatusCh      chan PatchEnvelopesVsCh
	PatchEnvelopeInfoCh chan []types.PatchEnvelopeInfo

	envelopes        []types.PatchEnvelopeInfo
	completedVolumes []types.VolumeStatus
	log              *base.LogObject
}

// PatchEnvelopesVsChAction specifies action for patch envelope volume status
type PatchEnvelopesVsChAction uint8

const (
	// PatchEnvelopesVsChActionDelete -- delete reference with volume
	PatchEnvelopesVsChActionDelete PatchEnvelopesVsChAction = iota
	// PatchEnvelopesVsChActionPut -- create or update reference with volume
	PatchEnvelopesVsChActionPut
)

// PatchEnvelopesVsCh is a wrapper for VolumeStatus specifying action
// for external Patch Envelope
type PatchEnvelopesVsCh struct {
	Vs     types.VolumeStatus
	Action PatchEnvelopesVsChAction
}

// NewPatchEnvelopes returns PatchEnvelopes structure
func NewPatchEnvelopes(log *base.LogObject) *PatchEnvelopes {
	pe := &PatchEnvelopes{
		VolumeStatusCh:      make(chan PatchEnvelopesVsCh),
		PatchEnvelopeInfoCh: make(chan []types.PatchEnvelopeInfo),

		log: log,
	}

	go pe.processMessages()

	return pe
}

// Get returns list of Patch Envelopes available for this app instance
func (pes *PatchEnvelopes) Get(appUUID string) types.PatchEnvelopeInfoList {
	var res []types.PatchEnvelopeInfo

	for _, envelope := range pes.envelopes {
		for _, allowedUUID := range envelope.AllowedApps {
			if allowedUUID == appUUID {
				res = append(res, envelope)
				break
			}
		}
	}

	return types.PatchEnvelopeInfoList{
		Envelopes: res,
	}
}

func (pes *PatchEnvelopes) processMessages() {
	for {
		select {
		case volumeStatus := <-pes.VolumeStatusCh:
			pes.updateVolumeStatus(volumeStatus)
		case newPatchEnvelopeInfo := <-pes.PatchEnvelopeInfoCh:
			pes.updateEnvelopes(newPatchEnvelopeInfo)
		}
	}
}

func (pes *PatchEnvelopes) updateVolumeStatus(volumeStatus PatchEnvelopesVsCh) {
	switch volumeStatus.Action {
	case PatchEnvelopesVsChActionPut:
		pes.updateExternalPatches(volumeStatus.Vs)
	case PatchEnvelopesVsChActionDelete:
		pes.deleteExternalPatches(volumeStatus.Vs)
	}
}

func (pes *PatchEnvelopes) updateExternalPatches(vs types.VolumeStatus) {
	if vs.State < types.CREATED_VOLUME {
		return
	}

	pes.Lock()
	defer pes.Unlock()

	volumeExists := false
	for i := range pes.completedVolumes {
		if pes.completedVolumes[i].VolumeID == vs.VolumeID {
			pes.completedVolumes[i] = vs
			volumeExists = true
			break
		}
	}
	if !volumeExists {
		pes.completedVolumes = append(pes.completedVolumes, vs)
	}

	if err := pes.processVolumeStatus(vs); err != nil {
		pes.log.Errorf("Failed to update external patches %v", err)
	}
}

func (pes *PatchEnvelopes) deleteExternalPatches(vs types.VolumeStatus) {
	pes.Lock()
	defer pes.Unlock()

	i := 0
	for _, vol := range pes.completedVolumes {
		if vol.VolumeID == vs.VolumeID {
			break
		}
		i++
	}

	pes.completedVolumes[i] = pes.completedVolumes[len(pes.completedVolumes)-1]
	pes.completedVolumes = pes.completedVolumes[:len(pes.completedVolumes)-1]
}

func (pes *PatchEnvelopes) updateEnvelopes(peInfo []types.PatchEnvelopeInfo) {
	pes.Lock()
	defer pes.Unlock()

	pes.envelopes = peInfo
	if err := pes.processVolumeStatusList(pes.completedVolumes); err != nil {
		pes.log.Errorf("Failed to update external patches %v", err)
	}
}

func (pes *PatchEnvelopes) processVolumeStatus(vs types.VolumeStatus) error {
	for i := range pes.envelopes {
		for j := range pes.envelopes[i].VolumeRefs {
			volUUID, err := uuid.FromString(pes.envelopes[i].VolumeRefs[j].ImageID)
			if err != nil {
				return err
			}
			if volUUID == vs.VolumeID {
				blob, err := blobFromVolRef(pes.envelopes[i].VolumeRefs[j], vs)
				if err != nil {
					return err
				}

				if idx := types.CompletedBinaryBlobIdxByName(pes.envelopes[i].BinaryBlobs, blob.FileName); idx != -1 {
					pes.envelopes[i].BinaryBlobs[idx] = *blob
				} else {
					pes.envelopes[i].BinaryBlobs = append(pes.envelopes[i].BinaryBlobs, *blob)
				}
			}
		}
	}
	return nil
}

func (pes *PatchEnvelopes) processVolumeStatusList(volumeStatuses []types.VolumeStatus) error {
	for _, vs := range volumeStatuses {
		if err := pes.processVolumeStatus(vs); err != nil {
			return err
		}
	}
	return nil
}

func blobFromVolRef(vr types.BinaryBlobVolumeRef, vs types.VolumeStatus) (*types.BinaryBlobCompleted, error) {
	sha, err := fileutils.ComputeShaFile(vs.FileLocation)
	if err != nil {
		return nil, err
	}
	return &types.BinaryBlobCompleted{
		FileName:         vr.FileName,
		FileMetadata:     vr.FileMetadata,
		ArtifactMetadata: vr.ArtifactMetadata,
		FileSha:          fmt.Sprintf("%x", sha),
		URL:              vs.FileLocation,
	}, nil
}

// PatchEnvelopeInfo contains fields that we don't want to expose to app instance (like AllowedApps), so we use
// peInfoToDisplay and patchEnvelopesJSONFOrAppInstance to marshal PatchEnvelopeInfoList in a format, which is
// suitable for app instance.
// We cannot use json:"-" structure tag to omit AllowedApps from json marshaling since we use PatchEnvelopeInfo between
// zedagent and zedrouter to communicate new PatchEnvelopes from EdgeDevConfig. This communication is done via pubSub,
// which uses json marshaling to communicate structures between processes. And using json:"-" will make AllowedApps "magically"
// disappear on zedrouter
type peInfoToDisplay struct {
	PatchID     string
	BinaryBlobs []types.BinaryBlobCompleted
	VolumeRefs  []types.BinaryBlobVolumeRef
}

// PatchEnvelopesJSONForAppInstance returns json representation
// of Patch Envelopes list which are shown to app instances
func PatchEnvelopesJSONForAppInstance(pe types.PatchEnvelopeInfoList) ([]byte, error) {
	toDisplay := make([]peInfoToDisplay, len(pe.Envelopes))

	for i, envelope := range pe.Envelopes {
		toDisplay[i] = peInfoToDisplay{
			PatchID:     envelope.PatchID,
			BinaryBlobs: envelope.BinaryBlobs,
			VolumeRefs:  envelope.VolumeRefs,
		}
	}

	return json.Marshal(toDisplay)
}
