package extract

import "strings"

const (
	// Service hosts.
	svcCompute       = "compute.googleapis.com"
	svcStorage       = "storage.googleapis.com"
	svcBigQuery      = "bigquery.googleapis.com"
	svcPubSub        = "pubsub.googleapis.com"
	svcRun           = "run.googleapis.com"
	svcVPCAccess     = "vpcaccess.googleapis.com"
	svcDatastream    = "datastream.googleapis.com"
	svcCloudKMS      = "cloudkms.googleapis.com"
	googleapisSuffix = ".googleapis.com"

	// Asset types.
	assetTypeOrganization   = "cloudresourcemanager.googleapis.com/Organization"
	assetTypeFolder         = "cloudresourcemanager.googleapis.com/Folder"
	assetTypeProject        = "cloudresourcemanager.googleapis.com/Project"
	assetTypeServiceAccount = "iam.googleapis.com/ServiceAccount"
	assetTypeSubnetwork     = "compute.googleapis.com/Subnetwork"
	assetTypeRunService     = "run.googleapis.com/Service"

	// URI prefixes, path segments, schemes, and CLI prefixes.
	prefixProjects         = "projects/"
	prefixFolders          = "folders/"
	prefixOrganizations    = "organizations/"
	prefixHTTPS            = "https://"
	prefixStorageURI       = "//storage.googleapis.com/"
	prefixRunProjects      = "//run.googleapis.com/projects/"
	prefixSQLProjects      = "//sqladmin.googleapis.com/projects/"
	prefixComputeProjects  = "//compute.googleapis.com/projects/"
	prefixBQProjects       = "//bigquery.googleapis.com/projects/"
	segLocations           = "/locations/"
	segServices            = "/services/"
	segInstances           = "/instances/"
	segClusters            = "/clusters/"
	segDatasets            = "/datasets/"
	flagProjectPrefix      = "--project="
	flagClusterPrefix      = "--cluster="
	prefixRunDeploy        = "run-deploy:"
	prefixGCR              = "gcr.io/"
	prefixServiceDash      = "service-"
	prefixServiceAccount   = "serviceaccount:"
	hostGitHub             = "github.com"
	detailEventarcDest     = "eventarc.destination"
	labelResourceProjectID = "resource.labels.project_id"

	// Preflight roles and probe sources.
	roleFolderViewer   = "roles/resourcemanager.folderViewer"
	sourceCAIFolders   = "cai.folders"
	roleAIPlatformUser = "roles/aiplatform.user"
	sourceVertexIAM    = "vertex.iam"
)

func hasHTTPURLScheme(s string) bool {
	scheme, _, ok := strings.Cut(s, "://")
	return ok && (scheme == "https" || scheme == "http")
}

func stripHTTPURLScheme(s string) string {
	scheme, rest, ok := strings.Cut(s, "://")
	if ok && (scheme == "https" || scheme == "http") {
		return rest
	}
	return s
}
