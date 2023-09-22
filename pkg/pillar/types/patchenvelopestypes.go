// Copyright (c) 2023 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"archive/zip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	uuid "github.com/satori/go.uuid"
)

type PatchEnvelopes struct {
	sync.Mutex
	VolumeStatusCh      chan PatchEnvelopesVsCh
	PatchEnvelopeInfoCh chan []PatchEnvelopeInfo
	Wg                  sync.WaitGroup

	envelopes        []PatchEnvelopeInfo
	completedVolumes []VolumeStatus
}

type PatchEnvelopesVsChAction uint8

const (
	PatchEnvelopesVsChActionDelete PatchEnvelopesVsChAction = iota
	PatchEnvelopesVsChActionPut
)

type PatchEnvelopesVsCh struct {
	Vs     VolumeStatus
	Action PatchEnvelopesVsChAction
}

func NewPatchEnvelopes() *PatchEnvelopes {
	pe := &PatchEnvelopes{
		VolumeStatusCh:      make(chan PatchEnvelopesVsCh),
		PatchEnvelopeInfoCh: make(chan []PatchEnvelopeInfo),
	}

	go pe.processMessages()

	return pe
}

func (pes *PatchEnvelopes) Get(appUuid string) []PatchEnvelopeInfo {
	var res []PatchEnvelopeInfo

	for _, envelope := range pes.envelopes {
		for _, allowedUuid := range envelope.AllowedApps {
			if allowedUuid == appUuid {
				res = append(res, envelope)
				break
			}
		}
	}

	return res
}

func (pes *PatchEnvelopes) processMessages() {
	for {
		select {
		case volumeStatus := <-pes.VolumeStatusCh:
			switch volumeStatus.Action {
			case PatchEnvelopesVsChActionPut:
				pes.updateExternalPatches(volumeStatus.Vs)
			case PatchEnvelopesVsChActionDelete:
				pes.deleteExternalPatches(volumeStatus.Vs)
			}
			pes.Wg.Done()
		case newPatchEnvelopeInfo := <-pes.PatchEnvelopeInfoCh:
			pes.updateEnvelopes(newPatchEnvelopeInfo)
			pes.Wg.Done()
		}
	}
}

func (pes *PatchEnvelopes) updateExternalPatches(vs VolumeStatus) {
	if vs.State != INSTALLED {
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

	// TODO: handle error here
	pes.processVolumeStatus(vs)
}

func (pes *PatchEnvelopes) deleteExternalPatches(vs VolumeStatus) {
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

func (pes *PatchEnvelopes) updateEnvelopes(peInfo []PatchEnvelopeInfo) {
	pes.Lock()
	defer pes.Unlock()

	pes.envelopes = peInfo
	pes.processVolumeStatusList(pes.completedVolumes)
}

func (pes *PatchEnvelopes) processVolumeStatus(vs VolumeStatus) error {
	for i := range pes.envelopes {
		for j := range pes.envelopes[i].VolumeRefs {
			volUuid, err := uuid.FromString(pes.envelopes[i].VolumeRefs[j].ImageId)
			if err != nil {
				return err
			}
			if volUuid == vs.VolumeID {
				blob, err := blobFromVolRef(pes.envelopes[i].VolumeRefs[j], vs)
				if err != nil {
					return err
				}

				if idx := CompletedBinaryBlobIdxByName(pes.envelopes[i].BinaryBlobs, blob.FileName); idx != -1 {
					pes.envelopes[i].BinaryBlobs[idx] = *blob
				} else {
					pes.envelopes[i].BinaryBlobs = append(pes.envelopes[i].BinaryBlobs, *blob)
				}
			}
		}
	}
	return nil
}

func (pes *PatchEnvelopes) processVolumeStatusList(volumeStatuses []VolumeStatus) error {
	for _, vs := range volumeStatuses {
		if err := pes.processVolumeStatus(vs); err != nil {
			return err
		}
	}
	return nil
}

func calculateSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	hashBytes := hash.Sum(nil)

	hashString := fmt.Sprintf("%x", hashBytes)

	return hashString, nil
}

func blobFromVolRef(vr BinaryBlobVolumeRef, vs VolumeStatus) (*BinaryBlobCompleted, error) {
	sha, err := calculateSHA256(vs.FileLocation)
	if err != nil {
		return nil, err
	}
	return &BinaryBlobCompleted{
		FileName:     vr.FileName,
		FileMetadata: vr.FileMetadata,
		FileSha:      sha,
		Url:          vs.FileLocation,
	}, nil
}

// PatchEnvelopeInfo - information
// about patch envelopes
type PatchEnvelopeInfo struct {
	AllowedApps []string
	PatchId     string
	BinaryBlobs []BinaryBlobCompleted
	VolumeRefs  []BinaryBlobVolumeRef
}

// Key for pubsub
func PatchEnvelopeInfoKey() string {
	return "zedagent"
}
func FindPatchEnvelopesByApp(pe []PatchEnvelopeInfo, appUuid string) []PatchEnvelopeInfo {
	var res []PatchEnvelopeInfo

	for _, envelope := range pe {
		for _, allowedUuid := range envelope.AllowedApps {
			if allowedUuid == appUuid {
				res = append(res, envelope)
				break
			}
		}
	}

	return res
}

func FindPatchEnvelopeById(pe []PatchEnvelopeInfo, patchId string) *PatchEnvelopeInfo {
	for _, pe := range pe {
		if pe.PatchId == patchId {
			return &pe
		}
	}
	return nil
}

func GetZipArchive(root string, pe PatchEnvelopeInfo) (string, error) {
	zipFilename := filepath.Join(root, pe.PatchId+".zip")
	zipFile, err := os.Create(zipFilename)
	if err != nil {
		return "", err
	}
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)
	defer zipWriter.Close()

	for _, b := range pe.BinaryBlobs {
		// We only want to archive binary blobs which are ready
		file, err := os.Open(b.Url)
		if err != nil {
			return "", err
		}
		defer file.Close()

		baseName := filepath.Base(b.Url)
		zipEntry, err := zipWriter.Create(baseName)
		if err != nil {
			return "", err
		}

		_, err = io.Copy(zipEntry, file)
		if err != nil {
			return "", err
		}

	}

	return zipFilename, nil
}

type BinaryBlobCompleted struct {
	FileName     string `json:"file-name"`
	FileSha      string `json:"file-sha"`
	FileMetadata string `json:"file-meta-data"`
	Url          string `json:"url"`
}

func CompletedBinaryBlobIdxByName(blobs []BinaryBlobCompleted, name string) int {
	for i := range blobs {
		if blobs[i].FileName == name {
			return i
		}
	}
	return -1
}

type BinaryBlobVolumeRef struct {
	FileName     string `json:"file-name"`
	ImageName    string `json:"image-name"`
	FileMetadata string `json:"file-meta-data"`
	ImageId      string `json:"image-id"`
}
