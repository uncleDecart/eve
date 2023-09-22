// Copyright (c) 2023 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
)

type PatchEnvelopes struct {
	Envelopes []PatchEnvelopeInfo
}

// Key for pubsub
func PatchEnvelopeInfoKey() string {
	return "zedagent"
}

// PatchEnvelopeInfo - information
// about patch envelopes
type PatchEnvelopeInfo struct {
	AllowedApps []string
	PatchId     string
	BinaryBlobs []BinaryBlobCompleted
	VolumeRefs  []BinaryBlobVolumeRef
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
