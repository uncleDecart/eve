// Copyright (c) 2023 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package zedrouter_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lf-edge/eve/pkg/pillar/base"
	"github.com/lf-edge/eve/pkg/pillar/cmd/zedrouter"
	"github.com/lf-edge/eve/pkg/pillar/types"
	"github.com/onsi/gomega"
	uuid "github.com/satori/go.uuid"
	"github.com/sirupsen/logrus"
)

func TestPatchEnvelopes(t *testing.T) {
	t.Parallel()

	g := gomega.NewGomegaWithT(t)

	logger := logrus.StandardLogger()
	log := base.NewSourceLogObject(logger, "petypes", 1234)
	peStore := zedrouter.NewPatchEnvelopes(log)

	u := "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	u1, _ := uuid.FromString(u)
	filecontent := "blobfilecontent"
	path, err := os.MkdirTemp("", "testFolder")
	f := filepath.Join(path, "blobfile")
	g.Expect(err).To(gomega.BeNil())

	err = os.WriteFile(f, []byte(filecontent), 0600)
	g.Expect(err).To(gomega.BeNil())

	defer os.Remove(f)

	volumeStatuses := []types.VolumeStatus{
		{
			VolumeID:     u1,
			State:        types.INSTALLED,
			FileLocation: f,
		},
	}

	peInfo := []types.PatchEnvelopeInfo{
		{
			PatchID:     "PatchId1",
			AllowedApps: []string{u},
			BinaryBlobs: []types.BinaryBlobCompleted{
				{
					FileName:     "TestFileName",
					FileSha:      "TestFileSha",
					FileMetadata: "TestFileMetadata",
					URL:          "./testurl",
				},
			},
			VolumeRefs: []types.BinaryBlobVolumeRef{
				{
					FileName:     "VolTestFileName",
					ImageName:    "VolTestImageName",
					FileMetadata: "VolTestFileMetadata",
					ImageID:      u,
				},
			},
		},
	}

	peStore.Wg.Add(1)
	go func() {
		for _, vs := range volumeStatuses {
			peStore.VolumeStatusCh <- zedrouter.PatchEnvelopesVsCh{
				Vs:     vs,
				Action: zedrouter.PatchEnvelopesVsChActionPut,
			}
		}
	}()

	peStore.Wg.Add(1)
	go func() {
		peStore.PatchEnvelopeInfoCh <- peInfo
	}()

	peStore.Wg.Wait()

	g.Expect(peStore.Get(u).Envelopes).To(gomega.BeEquivalentTo(
		[]types.PatchEnvelopeInfo{
			{
				PatchID:     "PatchId1",
				AllowedApps: []string{u},
				BinaryBlobs: []types.BinaryBlobCompleted{
					{
						FileName:     "TestFileName",
						FileSha:      "TestFileSha",
						FileMetadata: "TestFileMetadata",
						URL:          "./testurl",
					},
					{
						FileName: "VolTestFileName",
						//pragma: allowlist nextline secret
						FileSha:      "2c096be52e6f8510b4deac978f700dd103f144539e4ccdede5b075ce55dca980",
						FileMetadata: "VolTestFileMetadata",
						URL:          f,
					},
				},
				VolumeRefs: []types.BinaryBlobVolumeRef{
					{
						FileName:     "VolTestFileName",
						ImageName:    "VolTestImageName",
						FileMetadata: "VolTestFileMetadata",
						ImageID:      u,
					},
				},
			},
		}))
}
