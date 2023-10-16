// Copyright (c) 2023 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package zedrouter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/lf-edge/eve/pkg/pillar/types"
	"github.com/lf-edge/eve/pkg/pillar/zedcloud"
	uuid "github.com/satori/go.uuid"

	"github.com/lf-edge/eve/pkg/pillar/persistcache"
	"github.com/lf-edge/eve/pkg/pillar/utils"
	fileutils "github.com/lf-edge/eve/pkg/pillar/utils/file"
)

// Provides a json file
type networkHandler struct {
	zedrouter *zedrouter
}

// Provides a LF-terminated text
type externalIPHandler struct {
	zedrouter *zedrouter
}

// Provides a LF-terminated text
type hostnameHandler struct {
	zedrouter *zedrouter
}

// Provides links for OpenStack metadata/userdata
type openstackHandler struct {
	zedrouter *zedrouter
}

// Let's application to report various metadata back to the cloud. At the
// moment used for:
//   - k3s cluster kubeconfig
//   - reporting custom application status
type appInstMetaHandler struct {
	zedrouter       *zedrouter
	maxResponseLen  int
	publishDataType types.AppInstMetaDataType
}

// Provides geographic location of the device.
type locationInfoHandler struct {
	zedrouter *zedrouter
}

// Provides information about cellular connectivity of the device
// (modems, SIM cards, network providers, etc.).
type wwanStatusHandler struct {
	zedrouter *zedrouter
}

// Provides cellular metrics (signal strength, packet counters).
type wwanMetricsHandler struct {
	zedrouter *zedrouter
}

// Provides a signing service
type signerHandler struct {
	zedrouter   *zedrouter
	zedcloudCtx *zedcloud.ZedCloudContext
}

// Provides the /opt/zededa/bin/diag output as text
type diagHandler struct {
	zedrouter   *zedrouter
	zedcloudCtx *zedcloud.ZedCloudContext
}

// AppInfoHandler provides information about available patches for the application
type AppInfoHandler struct {
	zedrouter *zedrouter
}

// AppCustomBlobsHandler serves the AppCustom binary blobs
type AppCustomBlobsHandler struct {
	zedrouter *zedrouter
}

type middlewareKeys int

const (
	patchEnvelopesContextKey middlewareKeys = iota
	appInstanceIPContextKey
)

// ServeHTTP for networkHandler provides a json return
func (hdl networkHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdl.zedrouter.log.Tracef("networkHandler.ServeHTTP")
	remoteIP := net.ParseIP(strings.Split(r.RemoteAddr, ":")[0])
	externalIP, code := hdl.zedrouter.getExternalIPForApp(remoteIP)
	var ipStr string
	var hostname string
	// Avoid returning the string <nil>
	if !isEmptyIP(externalIP) {
		ipStr = externalIP.String()
	}
	anStatus := hdl.zedrouter.lookupAppNetworkStatusByAppIP(remoteIP)
	if anStatus != nil {
		hostname = anStatus.UUIDandVersion.UUID.String()
	}

	enInfoObj, err := hdl.zedrouter.subEdgeNodeInfo.Get("global")
	if err != nil {
		errorLine := fmt.Sprintf("cannot fetch edge node information: %s", err)
		hdl.zedrouter.log.Error(errorLine)
		http.Error(w, errorLine, http.StatusInternalServerError)
		return
	}
	enInfo := enInfoObj.(types.EdgeNodeInfo)
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(code)
	resp, _ := json.Marshal(map[string]interface{}{
		"caller-ip":         r.RemoteAddr,
		"external-ipv4":     ipStr,
		"hostname":          hostname, // Do not delete this line for backward compatibility
		"app-instance-uuid": hostname,
		"device-uuid":       enInfo.DeviceID,
		"device-name":       enInfo.DeviceName,
		"project-name":      enInfo.ProjectName,
		"project-uuid":      enInfo.ProjectID,
		"enterprise-name":   enInfo.EnterpriseName,
		"enterprise-id":     enInfo.EnterpriseID,
		// TBD: add public-ipv4 when controller tells us
	})
	w.Write(resp)
}

// ServeHTTP for externalIPHandler provides a text IP address
func (hdl externalIPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdl.zedrouter.log.Tracef("externalIPHandler.ServeHTTP")
	remoteIP := net.ParseIP(strings.Split(r.RemoteAddr, ":")[0])
	externalIP, code := hdl.zedrouter.getExternalIPForApp(remoteIP)
	w.WriteHeader(code)
	w.Header().Add("Content-Type", "text/plain")
	// Avoid returning the string <nil>
	if !isEmptyIP(externalIP) {
		resp := []byte(externalIP.String() + "\n")
		w.Write(resp)
	}
}

// ServeHTTP for hostnameHandler returns text
func (hdl hostnameHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdl.zedrouter.log.Tracef("hostnameHandler.ServeHTTP")
	remoteIP := net.ParseIP(strings.Split(r.RemoteAddr, ":")[0])
	anStatus := hdl.zedrouter.lookupAppNetworkStatusByAppIP(remoteIP)
	w.Header().Add("Content-Type", "text/plain")
	if anStatus == nil {
		w.WriteHeader(http.StatusNoContent)
		hdl.zedrouter.log.Errorf("No AppNetworkStatus for %s",
			remoteIP.String())
	} else {
		w.WriteHeader(http.StatusOK)
		resp := []byte(anStatus.UUIDandVersion.UUID.String() + "\n")
		w.Write(resp)
	}
}

// ServeHTTP for openstackHandler metadata service
func (hdl openstackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdl.zedrouter.log.Tracef("openstackHandler ServeHTTP request: %s", r.URL.String())
	dirname, filename := path.Split(strings.TrimSuffix(r.URL.Path, "/"))
	dirname = strings.TrimSuffix(dirname, "/")
	remoteIP := net.ParseIP(strings.Split(r.RemoteAddr, ":")[0])
	anStatus := hdl.zedrouter.lookupAppNetworkStatusByAppIP(remoteIP)
	var hostname string
	var id string
	if anStatus != nil {
		hostname = anStatus.DisplayName
		id = anStatus.UUIDandVersion.UUID.String()
	} else {
		errorLine := fmt.Sprintf("no AppNetworkStatus for %s",
			remoteIP.String())
		hdl.zedrouter.log.Error(errorLine)
		http.Error(w, errorLine, http.StatusNotImplemented)
		return
	}
	anConfig := hdl.zedrouter.lookupAppNetworkConfig(anStatus.Key())
	if anConfig == nil {
		errorLine := fmt.Sprintf("no AppNetworkConfig for %s",
			anStatus.Key())
		hdl.zedrouter.log.Error(errorLine)
		http.Error(w, errorLine, http.StatusNotImplemented)
		return
	}
	if anConfig.MetaDataType != types.MetaDataOpenStack {
		errorLine := fmt.Sprintf("no MetaDataOpenStack for %s",
			anStatus.Key())
		hdl.zedrouter.log.Tracef(errorLine)
		http.Error(w, errorLine, http.StatusNotFound)
		return
	}
	switch filename {
	case "openstack":
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "latest")
	case "meta_data.json":
		keys := hdl.zedrouter.getSSHPublicKeys(anConfig)
		var keysMap []map[string]string
		publicKeys := make(map[string]string)
		for ind, key := range keys {
			keysMap = append(keysMap, map[string]string{
				"data": fmt.Sprintf("%s\n", key),
				"type": "ssh",
				"name": fmt.Sprintf("key-%d", ind),
			})
			publicKeys[fmt.Sprintf("key-%d", ind)] = fmt.Sprintf("%s\n", key)
		}
		resp, _ := json.Marshal(map[string]interface{}{
			"uuid":         id,
			"hostname":     hostname,
			"name":         hostname,
			"launch_index": 0,
			"keys":         keysMap,
			"public_keys":  publicKeys,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(resp)
	case "network_data.json":
		resp, _ := json.Marshal(map[string]interface{}{
			"services": []string{},
			"networks": []string{},
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(resp)
	case "user_data":
		userData, err := hdl.zedrouter.getCloudInitUserData(anConfig)
		if err != nil {
			errorLine := fmt.Sprintf("cannot get userData for %s: %v",
				anStatus.Key(), err)
			hdl.zedrouter.log.Error(errorLine)
			http.Error(w, errorLine, http.StatusInternalServerError)
			return
		}
		ud, err := base64.StdEncoding.DecodeString(userData)
		if err != nil {
			errorLine := fmt.Sprintf("cannot decode userData for %s: %v",
				anStatus.Key(), err)
			hdl.zedrouter.log.Error(errorLine)
			http.Error(w, errorLine, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/yaml")
		w.WriteHeader(http.StatusOK)
		w.Write(ud)
	case "vendor_data.json":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}"))
	}
	w.WriteHeader(http.StatusNotFound)
}

// ServeHTTP for kubeConfigHandler provides cluster kube config
func (hdl appInstMetaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		msg := "appInstMetaHandler: request method is not Post"
		hdl.zedrouter.log.Error(msg)
		http.Error(w, msg, http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		msg := "appInstMetaHandler: Content-Type header is not application/json"
		hdl.zedrouter.log.Error(msg)
		http.Error(w, msg, http.StatusUnsupportedMediaType)
		return
	}

	kubeConfig, err := io.ReadAll(io.LimitReader(r.Body, AppInstMetadataResponseSizeLimitInBytes))
	if err != nil {
		msg := fmt.Sprintf("appInstMetaHandler: ReadAll failed: %v", err)
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	if binary.Size(kubeConfig) > hdl.maxResponseLen {
		msg := fmt.Sprintf("appInstMetaHandler: kubeconfig size exceeds limit. Expected <= %v, actual size: %v",
			hdl.maxResponseLen, binary.Size(kubeConfig))
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	remoteIP := net.ParseIP(strings.Split(r.RemoteAddr, ":")[0])
	anStatus := hdl.zedrouter.lookupAppNetworkStatusByAppIP(remoteIP)
	if anStatus == nil {
		msg := fmt.Sprintf("appInstMetaHandler: no AppNetworkStatus for %s", remoteIP.String())
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, http.StatusText(http.StatusNoContent), http.StatusNoContent)
		return
	}

	var appInstMetaData = &types.AppInstMetaData{
		AppInstUUID: anStatus.UUIDandVersion.UUID,
		Data:        kubeConfig,
		Type:        hdl.publishDataType,
	}
	hdl.zedrouter.publishAppInstMetadata(appInstMetaData)
	return
}

// ServeHTTP for locationInfoHandler provides a json return
func (hdl locationInfoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdl.zedrouter.log.Tracef("locationInfoHandler.ServeHTTP")
	locInfoObj, err := hdl.zedrouter.subLocationInfo.Get("global")
	if err != nil {
		http.Error(w, http.StatusText(http.StatusNoContent), http.StatusNoContent)
		return
	}
	locInfo := locInfoObj.(types.WwanLocationInfo)
	resp, err := json.Marshal(locInfo)
	if err != nil {
		msg := fmt.Sprintf("Failed to marshal location info: %v", err)
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(resp)
}

// ServeHTTP for wwanStatusHandler returns json output.
func (hdl wwanStatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdl.zedrouter.log.Tracef("wwanStatusHandler.ServeHTTP")
	statusObj, err := hdl.zedrouter.subWwanStatus.Get("global")
	if err != nil {
		http.Error(w, http.StatusText(http.StatusNoContent), http.StatusNoContent)
		return
	}
	status := statusObj.(types.WwanStatus)
	// SIM card and modem (logical) names are not relevant to applications.
	// They are generated by EVE and used in the EVE<->Controller API (ZInfoDevice)
	// for reference purposes. ConfigChecksum is also cleared because it is used
	// only internally by EVE microservices.
	// All these fields will be completely omitted from the json output
	// (all have omitempty json tag).
	for i := range status.Networks {
		status.Networks[i].Module.Name = ""
		for j := range status.Networks[i].SimCards {
			status.Networks[i].SimCards[j].Name = ""
		}
	}
	status.ConfigChecksum = ""
	resp, err := json.Marshal(status)
	if err != nil {
		msg := fmt.Sprintf("Failed to marshal WWAN status: %v", err)
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(resp)
}

// ServeHTTP for wwanMetricsHandler returns json output.
func (hdl wwanMetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdl.zedrouter.log.Tracef("wwanMetricsHandler.ServeHTTP")
	metricsObj, err := hdl.zedrouter.subWwanMetrics.Get("global")
	if err != nil {
		http.Error(w, http.StatusText(http.StatusNoContent), http.StatusNoContent)
		return
	}
	metrics := metricsObj.(types.WwanMetrics)
	resp, err := json.Marshal(metrics)
	if err != nil {
		msg := fmt.Sprintf("Failed to marshal WWAN metrics: %v", err)
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(resp)
}

// ServeHTTP for signerHandler returns protobuf output
func (hdl signerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdl.zedrouter.log.Tracef("signerHandler.ServeHTTP")

	if r.Method != http.MethodPost {
		msg := "signerHandler: request method is not POST"
		hdl.zedrouter.log.Error(msg)
		http.Error(w, msg, http.StatusMethodNotAllowed)
		return
	}
	// One larger to make sure we detect too large below.
	payload, err := io.ReadAll(io.LimitReader(r.Body, SignerMaxSize+1))
	if err != nil {
		msg := fmt.Sprintf("signerHandler: ReadAll failed: %v", err)
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}

	if binary.Size(payload) > SignerMaxSize {
		msg := fmt.Sprintf("signerHandler: size exceeds limit. Expected <= %v",
			SignerMaxSize)
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	remoteIP := net.ParseIP(strings.Split(r.RemoteAddr, ":")[0])
	anStatus := hdl.zedrouter.lookupAppNetworkStatusByAppIP(remoteIP)
	if anStatus == nil {
		msg := fmt.Sprintf("signerHandler: no AppNetworkStatus for %s",
			remoteIP.String())
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, msg, http.StatusForbidden)
		return
	}

	resp, err := zedcloud.AddAuthentication(hdl.zedcloudCtx,
		bytes.NewBuffer(payload), false)
	if err != nil {
		msg := fmt.Sprintf("Failed to AddAuthentication: %v", err)
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	w.Header().Add("Content-Type", "application/x-proto-binary")
	w.WriteHeader(http.StatusOK)
	w.Write(resp.Bytes())
}

// ServeHTTP for diagHandler returns text output
func (hdl diagHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdl.zedrouter.log.Tracef("diagHandler.ServeHTTP")

	if r.Method != http.MethodGet {
		msg := "diagHandler: request method is not GET"
		hdl.zedrouter.log.Error(msg)
		http.Error(w, msg, http.StatusMethodNotAllowed)
		return
	}
	// Check that request comes from a source IP for an app instance
	// to avoid returning data to others.
	remoteIP := net.ParseIP(strings.Split(r.RemoteAddr, ":")[0])
	anStatus := hdl.zedrouter.lookupAppNetworkStatusByAppIP(remoteIP)
	if anStatus == nil {
		msg := fmt.Sprintf("diagHandler: no AppNetworkStatus for %s",
			remoteIP.String())
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, msg, http.StatusForbidden)
		return
	}
	const diagStatefile = "/run/diag.out"

	if _, err := os.Stat(diagStatefile); err != nil && os.IsNotExist(err) {
		msg := "diagHandler: file not found"
		hdl.zedrouter.log.Error(msg)
		http.Error(w, msg, http.StatusNotFound)
		return
	}
	b, err := fileutils.ReadWithMaxSize(hdl.zedrouter.log, diagStatefile,
		DiagMaxSize+1)
	if err != nil {
		msg := fmt.Sprintf("diagHandler: read: %v", err)
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}

	if len(b) > DiagMaxSize {
		msg := fmt.Sprintf("diagHandler: size exceeds limit. Expected <= %v",
			DiagMaxSize)
		hdl.zedrouter.log.Errorf(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	w.Header().Add("Content-Type", "text")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

func (hdl AppInfoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdl.zedrouter.log.Tracef("wwanAppInfoHandler.ServeHTTP")
	w.Header().Add("Content-Type", "application/json")

	remoteIP := net.ParseIP(strings.Split(r.RemoteAddr, ":")[0])
	anStatus := hdl.zedrouter.lookupAppNetworkStatusByAppIP(remoteIP)
	if anStatus == nil {
		hdl.zedrouter.log.Errorf("Could not find network instance by ip %v", remoteIP)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not found"))
		return
	}

	diskStatusList := hdl.zedrouter.lookupDiskStatusList(
		anStatus.UUIDandVersion.UUID.String())

	var appInfo types.AppInfo
	for _, st := range diskStatusList {
		if st.Devtype != "AppCustom" {
			continue
		}

		blob := types.AppBlobsAvailable{
			CustomMeta: st.CustomMeta,
			DownloadURL: fmt.Sprintf("http://169.254.169.254/eve/app-custom-blobs/%s",
				st.DisplayName),
		}

		appInfo.AppBlobs = append(appInfo.AppBlobs, blob)
	}

	resp, _ := json.Marshal(appInfo)
	w.WriteHeader(http.StatusOK)
	w.Write(resp)
}

func (hdl AppCustomBlobsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	blobName := path.Base(r.URL.Path)

	remoteIP := net.ParseIP(strings.Split(r.RemoteAddr, ":")[0])
	anStatus := hdl.zedrouter.lookupAppNetworkStatusByAppIP(remoteIP)
	if anStatus == nil {
		hdl.zedrouter.log.Errorf("Could not find network instance by ip %v", remoteIP)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not found"))
		return
	}

	diskStatusList := hdl.zedrouter.lookupDiskStatusList(
		anStatus.UUIDandVersion.UUID.String())

	var blobFileLocation string
	for _, st := range diskStatusList {
		if st.Devtype != "AppCustom" {
			continue
		}

		if st.DisplayName == blobName {
			blobFileLocation = st.FileLocation
			break
		}
	}

	if blobFileLocation == "" {
		http.Error(w, r.RequestURI, http.StatusNotFound)
		return
	}

	f, err := os.Open(blobFileLocation)
	if err != nil {
		http.Error(w, r.RequestURI, http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, r.RequestURI, http.StatusNotFound)
		return
	}
	modTime := fi.ModTime()

	http.ServeContent(w, r, blobFileLocation, modTime, f)
}

// HandlePatchDescription returns Patch Envelopes available for app instance
func HandlePatchDescription(z *zedrouter) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		// WithPatchEnvelopesByIP middleware returns envelopes which are more than 0
		envelopes := r.Context().Value(patchEnvelopesContextKey).(types.PatchEnvelopeInfoList)

		b, err := patchEnvelopesJSONFOrAppInstance(envelopes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(b)
		return
	}
}

func sendError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(code)
	w.Write([]byte(fmt.Sprintf("{\"message\": \"%s\"}", msg)))
}

func appInstanceUUIDByIP(z *zedrouter, remoteAddr string) (uuid.UUID, error) {
	remoteIP := net.ParseIP(strings.Split(remoteAddr, ":")[0])
	anStatus := z.lookupAppNetworkStatusByAppIP(remoteIP)
	if anStatus == nil {
		return uuid.Nil, fmt.Errorf("No AppNetworkStatus for %s",
			remoteIP.String())
	}

	return anStatus.UUIDandVersion.UUID, nil
}

// HandlePatchDownload serves binary artifacts of specified patch envelope to app
// instance. Patch envelope id is specified in URL. All artifacts are compressed to
// a zip archive
func HandlePatchDownload(z *zedrouter) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		// WithPatchEnvelopesByIP middleware returns envelopes which are more than 0
		envelopes := r.Context().Value(patchEnvelopesContextKey).(types.PatchEnvelopeInfoList)

		patchID := chi.URLParam(r, "patch")
		if patchID == "" {
			sendError(w, http.StatusNoContent, "patch in route is missing")
			return
		}
		e := envelopes.FindPatchEnvelopeByID(patchID)
		if e != nil {
			path, err := os.MkdirTemp("", "patchEnvelopeZip")
			if err != nil {
				sendError(w, http.StatusInternalServerError,
					fmt.Sprintf("failed to create temp dir %v", err))
				return
			}
			zipFilename, err := utils.GetZipArchive(path, *e)

			if err != nil {
				sendError(w, http.StatusInternalServerError,
					fmt.Sprintf("failed to archive binary blobs %v", err))
				return
			}

			http.ServeFile(w, r, zipFilename)

			err = os.Remove(zipFilename)
			if err != nil {
				sendError(w, http.StatusInternalServerError,
					fmt.Sprintf("failed to delete archive %v", err))
				return
			}
			return
		}
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
}

// HandlePatchFileDownload serves binary artifact of specified patch envelope to app
// instance. Patch envelope id and file name is specified in URL.
func HandlePatchFileDownload(z *zedrouter) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		// WithPatchEnvelopesByIP middleware returns envelopes which are more than 0
		envelopes := r.Context().Value(patchEnvelopesContextKey).(types.PatchEnvelopeInfoList)

		patchID := chi.URLParam(r, "patch")
		if patchID == "" {
			sendError(w, http.StatusNotFound, "patch in route is missing")
			return
		}
		fileName := chi.URLParam(r, "file")
		if fileName == "" {
			sendError(w, http.StatusNotFound, "file in route is missing")
			return
		}

		e := envelopes.FindPatchEnvelopeByID(patchID)
		if e != nil {
			if idx := types.CompletedBinaryBlobIdxByName(e.BinaryBlobs, fileName); idx != -1 {
				http.ServeFile(w, r, e.BinaryBlobs[idx].URL)
				return
			} else {
				sendError(w, http.StatusNotFound, "file is not found")
				return
			}
		}

		sendError(w, http.StatusNotFound, "patch is not found")
	}
}

// WithPatchEnvelopesByIP is a middleware for Patch Envelopes which adds
// to a context patchEnvelope variable containing available patch envelopes
// for given IP address (it gets resolved to app instance UUID)
// in case there is no patch envelopes available it returns StatusNoContent
func WithPatchEnvelopesByIP(z *zedrouter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			appUUID, err := appInstanceUUIDByIP(z, r.RemoteAddr)
			if err != nil {
				w.WriteHeader(http.StatusNoContent)
				z.log.Errorf("No AppNetworkStatus for %s", r.RemoteAddr)
				return
			}

			accessablePe := z.patchEnvelopes.Get(appUUID.String())
			if len(accessablePe.Envelopes) == 0 {
				sendError(w, http.StatusOK, fmt.Sprintf("No envelopes for %s", appUUID.String()))
			}

			ctx := context.WithValue(r.Context(), patchEnvelopesContextKey, accessablePe)

			r = r.WithContext(ctx)
			next.ServeHTTP(w, r)
		})
	}
}

// HandleAppInstanceOpaqueStatus sends pubSub message to update controller with new
// status from app if it has changed
func HandleAppInstanceOpaqueStatus(z *zedrouter) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		opaqueStatus, err := io.ReadAll(io.LimitReader(r.Body, AppInstMetadataResponseSizeLimitInBytes))
		if err != nil {
			msg := fmt.Sprintf("appInstMetaHandler: ReadAll failed: %v", err)
			sendError(w, http.StatusInternalServerError, msg)
			z.log.Error(msg)
		}
		if r.Header.Get("Content-Type") != "application/octet-stream" {
			msg := "HandleAppInstanceOpaqueStatus: Content-Type header is not application/octet-stream"
			sendError(w, http.StatusUnsupportedMediaType, msg)
			z.log.Error(msg)
			return
		}

		appUUID, err := appInstanceUUIDByIP(z, r.RemoteAddr)
		if err != nil {
			msg := fmt.Sprintf("No AppNetworkStatus for %s", r.RemoteAddr)
			sendError(w, http.StatusNoContent, msg)
			z.log.Error(msg)
			return
		}

		pc, err := persistcache.New(types.PersistCachePatchEnvelopes)
		if err != nil {
			msg := fmt.Sprintf("Failed to load persistCache %v", err)
			sendError(w, http.StatusInternalServerError, msg)
			z.log.Error(msg)
			return
		}

		hasher := sha256.New()
		hasher.Write(opaqueStatus)
		newSha := hasher.Sum(nil)

		prevSHA, err := pc.Get(appUUID.String())
		if err != nil && !persistcache.IsKeyNotFound(err) {
			msg := fmt.Sprintf("Failed to get  %v", err)
			sendError(w, http.StatusInternalServerError, msg)
			z.log.Error(msg)
			return
		}

		if persistcache.IsKeyNotFound(err) || !bytes.Equal(newSha, prevSHA) {
			appInstOpaqueStatus := &types.AppInstMetaData{
				AppInstUUID: appUUID,
				Data:        opaqueStatus,
				Type:        types.AppInstMetaDataOpaqueStatus,
			}

			z.publishAppInstMetadata(appInstOpaqueStatus)
			pc.Put(appUUID.String(), newSha)
		}
	}
}
