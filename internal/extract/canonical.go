package extract

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	regionalHostPrefixRe = regexp.MustCompile(`^(?:[a-z]+-[a-z]+\d+|global|us|eu)-([a-z0-9-]+\.googleapis\.com)$`)
	apiVersionSegRe      = regexp.MustCompile(`^(?:alpha|beta|v\d+(?:(?:alpha|beta|p\d+beta|p\d+alpha)\d*)?)$`)
	gcpZoneRe            = regexp.MustCompile(`^[a-z]+-[a-z]+\d+-[a-z][a-z0-9]*$`)
)

// BuildProjectIndex maps discovered project numbers to canonical project IDs and overlays
// explicit user-supplied --project-alias overrides.
func BuildProjectIndex(projects []ProjectNode, aliases map[string]string) map[string]string {
	idx := make(map[string]string, len(projects)+len(aliases))
	for _, p := range projects {
		if p.ProjectNumber != "" && p.ProjectID != "" {
			idx[p.ProjectNumber] = p.ProjectID
		}
	}
	for num, id := range aliases {
		if num != "" && id != "" {
			idx[num] = id
		}
	}
	return idx
}

// CanonicalizeAIP122 normalizes GCP resource selfLinks, full URLs, gs:// URIs, //service/... URIs,
// and relative projects/... resource names into canonical AIP-122 format (//<service>/...)
// while replacing known project numbers (or aliases) with their canonical project IDs.
func CanonicalizeAIP122(raw string, projIdx map[string]string) string {
	return CanonicalizeAIP122WithService(raw, "", projIdx)
}

// CanonicalizeAIP122WithService behaves like CanonicalizeAIP122 but uses defaultService when
// raw is a relative resource name whose owning service cannot be inferred unambiguously.
func CanonicalizeAIP122WithService(raw, defaultService string, projIdx map[string]string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}

	if strings.HasPrefix(s, "gs://") {
		return canonicalizeGSURI(s)
	}

	s = strings.TrimRight(s, "/")
	if s == "" {
		return ""
	}

	if strings.HasPrefix(s, prefixHTTPS) || strings.HasPrefix(s, prefixHTTP) {
		return canonicalizeHTTPURL(s, projIdx)
	}

	if strings.HasPrefix(s, "//") {
		return canonicalizeDoubleSlashURI(s, projIdx)
	}

	if first, rest, ok := strings.Cut(s, "/"); ok && strings.HasSuffix(strings.ToLower(first), googleapisSuffix) {
		return canonicalizeHostAndPath(first, rest, projIdx)
	}

	return canonicalizeRelativeResource(s, defaultService, projIdx)
}

func canonicalizeGSURI(s string) string {
	bucket, _, _ := strings.Cut(strings.TrimPrefix(s, "gs://"), "/")
	if bucket == "" {
		return ""
	}
	return prefixStorageURI + bucket
}

func canonicalizeDoubleSlashURI(s string, projIdx map[string]string) string {
	host, path, ok := strings.Cut(strings.TrimPrefix(s, "//"), "/")
	if !ok {
		return "//" + normalizeServiceHost(host)
	}
	return canonicalizeHostAndPath(host, path, projIdx)
}

func canonicalizeHostAndPath(host, path string, projIdx map[string]string) string {
	svc := normalizeServiceHost(host)
	path = stripLeadingAPIVersion(path, svc)
	if svc == svcStorage {
		if bucket, ok := extractProjectsBucket(path); ok {
			return prefixStorageURI + bucket
		}
	}
	return "//" + svc + "/" + normalizeServicePath(svc, mapProjectSegments(path, projIdx))
}

func canonicalizeRelativeResource(s, defaultService string, projIdx map[string]string) string {
	mapped := mapProjectSegments(strings.TrimLeft(s, "/"), projIdx)
	if bucket, ok := extractProjectsBucket(mapped); ok && (defaultService == "" || normalizeServiceHost(defaultService) == svcStorage) {
		return prefixStorageURI + bucket
	}
	svc := defaultService
	if svc == "" {
		svc = inferServiceFromRelative(mapped)
	}
	if svc == "" {
		return mapped
	}
	normSvc := normalizeServiceHost(svc)
	return "//" + normSvc + "/" + normalizeServicePath(normSvc, mapped)
}

func canonicalizeHTTPURL(raw string, projIdx map[string]string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	path := strings.Trim(u.Path, "/")

	if host == "www.googleapis.com" {
		if canon := canonicalizeWWWGoogleAPIsPath(path, projIdx); canon != "" {
			return canon
		}
	}

	if strings.HasSuffix(host, googleapisSuffix) {
		return canonicalizeGoogleAPIsHTTPPath(host, path, projIdx)
	}

	// Non-googleapis URL: strip userinfo/query/fragment cleanly.
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

func extractStorageAPIBucket(segs []string) (string, bool) {
	if len(segs) >= 4 && segs[0] == "storage" && apiVersionSegRe.MatchString(segs[1]) && segs[2] == "b" {
		return segs[3], true
	}
	return "", false
}

func canonicalizeWWWGoogleAPIsPath(path string, projIdx map[string]string) string {
	segs := strings.Split(path, "/")
	if bucket, ok := extractStorageAPIBucket(segs); ok {
		return prefixStorageURI + bucket
	}
	if len(segs) >= 3 && apiVersionSegRe.MatchString(segs[1]) {
		svc := shortAPIToHost(segs[0])
		rem := normalizeServicePath(svc, mapProjectSegments(strings.Join(segs[2:], "/"), projIdx))
		return "//" + svc + "/" + rem
	}
	return ""
}

func canonicalizeGoogleAPIsHTTPPath(host, path string, projIdx map[string]string) string {
	svc := normalizeServiceHost(host)
	if svc == svcStorage {
		if canon := canonicalizeStorageHTTPPath(path, svc); canon != "" {
			return canon
		}
	}
	trimmed := normalizeServicePath(svc, mapProjectSegments(stripLeadingAPIVersion(path, svc), projIdx))
	return "//" + svc + "/" + trimmed
}

func canonicalizeStorageHTTPPath(path, svc string) string {
	segs := strings.Split(path, "/")
	if bucket, ok := extractStorageAPIBucket(segs); ok {
		return prefixStorageURI + bucket
	}
	if bucket, ok := extractProjectsBucket(stripLeadingAPIVersion(path, svc)); ok {
		return prefixStorageURI + bucket
	}
	if len(segs) >= 1 && segs[0] != "" {
		return prefixStorageURI + segs[0]
	}
	return ""
}

func normalizeServicePath(svc, path string) string {
	segs := strings.Split(path, "/")
	if len(segs) < 6 || segs[0] != "projects" {
		return path
	}
	switch svc {
	case svcCompute:
		if segs[2] == "locations" {
			loc := segs[3]
			if loc == "global" {
				out := make([]string, 0, len(segs)-1)
				out = append(out, "projects", segs[1], "global")
				out = append(out, segs[4:]...)
				return strings.Join(out, "/")
			}
			if gcpZoneRe.MatchString(loc) {
				segs[2] = "zones"
				return strings.Join(segs, "/")
			}
			segs[2] = "regions"
			return strings.Join(segs, "/")
		}
	case "container.googleapis.com":
		if segs[2] == "zones" && segs[4] == "clusters" {
			segs[2] = "locations"
			return strings.Join(segs, "/")
		}
	}
	return path
}

func extractProjectsBucket(path string) (string, bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) >= 4 && segs[0] == "projects" && segs[1] != "" && segs[2] == "buckets" && segs[3] != "" {
		return segs[3], true
	}
	return "", false
}

func normalizeServiceHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if m := regionalHostPrefixRe.FindStringSubmatch(h); len(m) == 2 {
		return m[1]
	}
	return h
}

func shortAPIToHost(short string) string {
	switch strings.ToLower(short) {
	case "sql":
		return "sqladmin.googleapis.com"
	default:
		return strings.ToLower(short) + googleapisSuffix
	}
}

func stripLeadingAPIVersion(path, serviceHost string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) == 0 {
		return ""
	}
	shortSvc := strings.TrimSuffix(serviceHost, googleapisSuffix)
	if len(segs) >= 3 && apiVersionSegRe.MatchString(segs[1]) &&
		(segs[0] == shortSvc || (segs[0] == "sql" && shortSvc == "sqladmin") || (segs[0] == "dns" && shortSvc == "dns")) {
		return strings.Join(segs[2:], "/")
	}
	if len(segs) >= 2 && apiVersionSegRe.MatchString(segs[0]) {
		return strings.Join(segs[1:], "/")
	}
	return strings.Join(segs, "/")
}

func mapProjectSegments(path string, projIdx map[string]string) string {
	if len(projIdx) == 0 || path == "" {
		return path
	}
	segs := strings.Split(path, "/")
	for i := 0; i < len(segs)-1; i++ {
		if segs[i] == "projects" || segs[i] == "apps" {
			if mapped, ok := projIdx[segs[i+1]]; ok && mapped != "" {
				segs[i+1] = mapped
			}
		}
	}
	return strings.Join(segs, "/")
}

var relativeProjectCollectionService = map[string]string{
	"topics":          svcPubSub,
	"subscriptions":   svcPubSub,
	"datasets":        svcBigQuery,
	"secrets":         "secretmanager.googleapis.com",
	"serviceAccounts": "iam.googleapis.com",
	"managedZones":    "dns.googleapis.com",
	"databases":       "firestore.googleapis.com",
}

func inferServiceFromRelative(path string) string {
	segs := strings.Split(path, "/")
	if len(segs) == 2 && (segs[0] == "projects" || segs[0] == "folders" || segs[0] == "organizations") {
		return "cloudresourcemanager.googleapis.com"
	}
	if len(segs) >= 2 && segs[0] == "apps" {
		return "appengine.googleapis.com"
	}
	if len(segs) < 4 || segs[0] != "projects" {
		return ""
	}
	if svc, ok := relativeProjectCollectionService[segs[2]]; ok {
		return svc
	}
	return inferScopedProjectService(segs)
}

func inferScopedProjectService(segs []string) string {
	switch segs[2] {
	case "global":
		if len(segs) >= 5 && isComputeScopeCollection(segs[3]) {
			return svcCompute
		}
	case "regions":
		if len(segs) >= 6 && isComputeScopeCollection(segs[4]) {
			return svcCompute
		}
	case "zones":
		return inferZonalService(segs)
	case "instances":
		return inferInstanceSubresourceService(segs)
	case "locations":
		return inferLocationsSubpathService(segs)
	}
	return ""
}

func inferZonalService(segs []string) string {
	if len(segs) < 6 {
		return ""
	}
	if segs[4] == "clusters" {
		return "container.googleapis.com"
	}
	if isComputeScopeCollection(segs[4]) {
		return svcCompute
	}
	return ""
}

func inferInstanceSubresourceService(segs []string) string {
	if len(segs) < 6 {
		return ""
	}
	switch segs[4] {
	case "databases":
		return "spanner.googleapis.com"
	case "tables", "clusters", "appProfiles":
		return "bigtableadmin.googleapis.com"
	}
	return ""
}

func inferLocationsSubpathService(segs []string) string {
	if len(segs) < 6 {
		return ""
	}
	if svc := inferLocationService(segs[4:]); svc != "" {
		return svc
	}
	// "instances" under /locations/ is shared by Redis, Memcache, Filestore, etc.;
	// only unambiguous Compute Engine collections infer compute.googleapis.com here.
	if segs[4] != "instances" && isComputeScopeCollection(segs[4]) {
		return svcCompute
	}
	return ""
}

func isComputeScopeCollection(coll string) bool {
	switch coll {
	case "networks", "subnetworks", "instances", "instanceGroups", "instanceGroupManagers", "instanceTemplates",
		"autoscalers", "healthChecks", "disks", "routes", "firewalls", "addresses",
		"forwardingRules", "targetHttpProxies", "targetHttpsProxies", "targetGrpcProxies",
		"targetTcpProxies", "targetSslProxies", "sslCertificates", "urlMaps", "backendServices", "backendBuckets",
		"securityPolicies", "networkEndpointGroups", "routers", "vpnGateways", "targetVpnGateways",
		"externalVpnGateways", "vpnTunnels", "interconnectAttachments", "serviceAttachments":
		return true
	}
	return false
}

func inferLocationService(rest []string) string {
	switch rest[0] {
	case "keyRings":
		return svcCloudKMS
	case "connectors":
		return svcVPCAccess
	case "triggers":
		return "eventarc.googleapis.com"
	case "queues":
		return "cloudtasks.googleapis.com"
	case "workflows":
		return "workflows.googleapis.com"
	case "functions":
		return "cloudfunctions.googleapis.com"
	case "environments":
		return "composer.googleapis.com"
	case "streams", "connectionProfiles", "privateConnections":
		return svcDatastream
	case "hubs", "spokes":
		return "networkconnectivity.googleapis.com"
	case "storagePools", "volumes":
		return "netapp.googleapis.com"
	case "ragCorpora", "endpoints", "models", "indexes", "indexEndpoints",
		"reasoningEngines", "featureOnlineStores", "featureGroups",
		"pipelineJobs", "trainingPipelines", "customJobs", "notebookRuntimes",
		"tensorboards", "metadataStores", "publisherModels":
		return "aiplatform.googleapis.com"
	case "dataStores", "collections":
		return "discoveryengine.googleapis.com"
	case "agents":
		return "dialogflow.googleapis.com"
	case "processors":
		return "documentai.googleapis.com"
	case "certificates", "certificateMaps":
		return "certificatemanager.googleapis.com"
	case "apis", "gateways":
		return "apigateway.googleapis.com"
	case "dataExchanges":
		return "analyticshub.googleapis.com"
	case "transferConfigs":
		return "bigquerydatatransfer.googleapis.com"
	case "lakes":
		return "dataplex.googleapis.com"
	case "connections":
		return "bigqueryconnection.googleapis.com"
	case "revisions", "services":
		return svcRun
	case "clusters":
		if len(rest) >= 4 && rest[2] == "instances" {
			return "alloydb.googleapis.com"
		}
	}
	return ""
}
