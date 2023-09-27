// Copyright (c) 2023 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package zedagent

import (
	"encoding/hex"
	"fmt"

	"crypto/sha256"

	zconfig "github.com/lf-edge/eve-api/go/config"
	"github.com/lf-edge/eve/pkg/pillar/persistcache"
	"github.com/lf-edge/eve/pkg/pillar/types"
)

func parsePatchEnvelopes(ctx *getconfigContext, config *zconfig.EdgeDevConfig) {
	parsePatchEnvelopesImpl(ctx, config, types.PersistCachePatchEnvelopes)
}

func parsePatchEnvelopesImpl(ctx *getconfigContext, config *zconfig.EdgeDevConfig,
	persistCacheFilepath string) {
	log.Tracef("Parsing patchEnvelope from configuration")

	patchEnvelopes := config.GetPatchEnvelopes()
	result := types.PatchEnvelopes{}
	for _, pe := range patchEnvelopes {
		peInfo := types.PatchEnvelopeInfo{
			AllowedApps: pe.GetAppInstIdsAllowed(),
			PatchId:     pe.GetUuid(),
		}
		for _, a := range pe.GetArtifacts() {
			err := addBinaryBlobToPatchEnvelope(&peInfo, a, persistCacheFilepath)
			if err != nil {
				log.Errorf("Failed to compose binary blob for patch envelope %v", err)
				return
			}
		}

		result.Envelopes = append(result.Envelopes, peInfo)
	}

	publishPatchEnvelopes(ctx, result)
}

func publishPatchEnvelopes(ctx *getconfigContext, patchEnvelopes types.PatchEnvelopes) {
	key := patchEnvelopes.Key()
	pub := ctx.pubPatchEnvelopeInfo

	pub.Publish(key, patchEnvelopes)

	log.Tracef("publishPatchEnvelopes(%s) done\n", key)
}

func addBinaryBlobToPatchEnvelope(pe *types.PatchEnvelopeInfo, artifact *zconfig.EveBinaryArtifact, persistCacheFilepath string) error {
	format := artifact.GetFormat()

	switch format {
	case zconfig.EVE_OPAQUE_OBJECT_CATEGORY_BINARYBLOB:
		binaryArtifact := artifact.GetVolumeRef()
		if binaryArtifact == nil {
			return fmt.Errorf("ExternalOpaqueBinaryBlob is empty, type indicates it should be present")
		}
		volumeRef, err := getBinaryBlobVolumeRef(binaryArtifact)
		if err != nil {
			return err
		}
		pe.VolumeRefs = append(pe.VolumeRefs, *volumeRef)
		return nil
	case zconfig.EVE_OPAQUE_OBJECT_CATEGORY_SECRET:
	case zconfig.EVE_OPAQUE_OBJECT_CATEGORY_BASE64:
		inlineArtifact := artifact.GetInline()
		if inlineArtifact == nil {
			return fmt.Errorf("InlineOpaqueBase64data is empty, type indicates it should be present")
		}
		binaryBlob, err := cacheInlineBase64Artifact(inlineArtifact, persistCacheFilepath)
		if err != nil {
			return err
		}
		pe.BinaryBlobs = append(pe.BinaryBlobs, *binaryBlob)
		return nil
	}

	return fmt.Errorf("Unknown EveBinaryArtifact format")
}

// cacheInlineBinaryArtifact stores inline artifact as file and
// returns path to it to be served by HTTP server
func cacheInlineBase64Artifact(artifact *zconfig.InlineOpaqueBase64Data, persistCacheFilepath string) (*types.BinaryBlobCompleted, error) {
	pc, err := persistcache.New(persistCacheFilepath)

	if err != nil {
		return nil, err
	}

	metadata := artifact.GetBase64MetaData()
	data := artifact.GetBase64Data()

	// We want write inline data to a file to serve it from http server
	url, err := pc.Put(artifact.GetFileNameToUse(), []byte(data))
	if err != nil {
		return nil, err
	}

	shaBytes := sha256.Sum256([]byte(data))
	return &types.BinaryBlobCompleted{
		FileName:     artifact.GetFileNameToUse(),
		FileSha:      hex.EncodeToString(shaBytes[:]),
		FileMetadata: metadata,
		Url:          url,
	}, nil
}

func getBinaryBlobVolumeRef(artifact *zconfig.ExternalOpaqueBinaryBlob) (*types.BinaryBlobVolumeRef, error) {
	// Since Volumes will be handled by volumemgr we can only provide
	// reference for now. It will be updated once download is completed
	// down the processing pipeline
	return &types.BinaryBlobVolumeRef{
		ImageName:    artifact.GetImageName(),
		FileName:     artifact.GetFileNameToUse(),
		FileMetadata: artifact.GetBlobMetaData(),
		ImageId:      artifact.GetImageId(),
	}, nil
}
