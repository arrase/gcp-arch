package extract

import (
	"maps"
	"regexp"
	"slices"
	"strings"
)

const (
	oosConfigRefCrossProject = "config_ref_cross_project"
	oosCloudBuildDeployFlag  = "cloudbuild_deploy_flag"
	detailNCCSpoke           = "ncc_spoke"
	assetTypeGKECluster      = "container.googleapis.com/Cluster"
)

type netIPKey struct {
	netURI string
	ip     string
}

type tracedBackend struct {
	backendURI    string
	backendType   string
	bucketName    string
	backendGroups []string
	paths         []string
}

type tracedForwardingRule struct {
	tls      bool
	hosts    []string
	paths    []string
	backends []tracedBackend
}

var (
	runAppHostRe        = regexp.MustCompile(`^([a-z0-9-]+)-[a-z0-9]{6,}-[a-z]{2}\.a\.run\.app$`)
	iamCondResNameRe    = regexp.MustCompile(`resource\.name\s*(?:==|\.startsWith\()\s*["']([^"']+)["']`)
	publicBuilderMirror = map[string]bool{
		"cloud-builders":           true,
		"cloud-builders-community": true,
		"google-containers":        true,
		"distroless":               true,
		"gke-release":              true,
		"gcp-runtimes":             true,
		"cloud-marketplace":        true,
		"cos-cloud":                true,
		"debian-cloud":             true,
		"ubuntu-os-cloud":          true,
		"google.com":               true,
		"external":                 true,
		"_":                        true,
	}

	broadAncestorRoles = map[string]bool{
		"roles/owner":   true,
		"roles/editor":  true,
		"roles/viewer":  true,
		"roles/browser": true,
	}

	serviceRoleTargetTypes = []struct {
		prefix     string
		assetTypes []string
	}{
		{"roles/cloudsql.", []string{"sqladmin.googleapis.com/Instance"}},
		{"roles/alloydb.", []string{"alloydb.googleapis.com/Cluster"}},
		{"roles/spanner.", []string{"spanner.googleapis.com/Instance"}},
		{"roles/datastore.", []string{"firestore.googleapis.com/Database"}},
		{"roles/firestore.", []string{"firestore.googleapis.com/Database"}},
		{"roles/bigtable.", []string{"bigtableadmin.googleapis.com/Instance"}},
		{"roles/redis.", []string{"redis.googleapis.com/Instance", "redis.googleapis.com/Cluster"}},
		{"roles/memcache.", []string{"memcache.googleapis.com/Instance"}},
		{"roles/file.", []string{"file.googleapis.com/Instance"}},
		{"roles/storage.", []string{"storage.googleapis.com/Bucket"}},
		{"roles/bigquery.", []string{"bigquery.googleapis.com/Dataset"}},
		{"roles/pubsub.", []string{"pubsub.googleapis.com/Topic"}},
		{"roles/cloudtasks.", []string{"cloudtasks.googleapis.com/Queue"}},
		{"roles/workflows.", []string{"workflows.googleapis.com/Workflow"}},
		{"roles/secretmanager.", []string{"secretmanager.googleapis.com/Secret"}},
		{"roles/artifactregistry.", []string{"artifactregistry.googleapis.com/Repository"}},
		{"roles/cloudkms.", []string{"cloudkms.googleapis.com/CryptoKey"}},
		{"roles/run.", []string{assetTypeRunService}},
		{"roles/cloudfunctions.", []string{"cloudfunctions.googleapis.com/CloudFunction"}},
		{"roles/discoveryengine.", []string{"discoveryengine.googleapis.com/Engine", "discoveryengine.googleapis.com/DataStore"}},
		{"roles/dialogflow.", []string{"dialogflow.googleapis.com/Agent"}},
		{"roles/documentai.", []string{"documentai.googleapis.com/Processor"}},
	}
)

func (st *extractState) resolveStructuralAndIAM() {
	st.resolveLoadBalancerNonGKEBackends()
	st.resolveStructuralRefs()
	st.resolveAbsorbedBridges()
	st.resolveEnvAndSecretRefs()
	st.resolveIAMBindings()
}

type forwardingRuleTracer struct {
	st              *extractState
	out             tracedForwardingRule
	backendPaths    map[string][]string
	orderedBackends []string
}

// traceForwardingRule follows ForwardingRule -> Target*Proxy -> UrlMap -> BackendService/BackendBucket
// across suppressed LB intermediate records.
func (st *extractState) traceForwardingRule(fr *ingestedAsset) tracedForwardingRule {
	t := forwardingRuleTracer{
		st:           st,
		backendPaths: make(map[string][]string),
	}
	t.recordBackend(fr.frBackendSvc, nil)
	t.visitTarget(fr.frTarget)
	return t.finalize()
}

func (t *forwardingRuleTracer) recordBackend(uri string, paths []string) {
	if uri == "" {
		return
	}
	if _, exists := t.backendPaths[uri]; !exists {
		t.orderedBackends = append(t.orderedBackends, uri)
		t.backendPaths[uri] = nil
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		appendUniqueMap(t.backendPaths, uri, p)
		appendUnique(&t.out.paths, p)
	}
}

func (t *forwardingRuleTracer) visitTarget(targetURI string) {
	if targetURI == "" {
		return
	}
	lb := t.st.lbAssets[targetURI]
	if lb == nil {
		return
	}
	if lb.isTLS {
		t.out.tls = true
	}
	switch lb.assetType {
	case "compute.googleapis.com/TargetHttpProxy",
		"compute.googleapis.com/TargetHttpsProxy",
		"compute.googleapis.com/TargetGrpcProxy":
		t.visitURLMap(lb.urlMap)
	case "compute.googleapis.com/TargetTcpProxy",
		"compute.googleapis.com/TargetSslProxy":
		t.recordBackend(lb.service, nil)
	case "compute.googleapis.com/BackendService",
		"compute.googleapis.com/BackendBucket":
		t.recordBackend(lb.uri, nil)
	}
}

func (t *forwardingRuleTracer) visitURLMap(umURI string) {
	um := t.st.lbAssets[umURI]
	if um == nil {
		return
	}
	for _, h := range um.urlMapHosts {
		appendUnique(&t.out.hosts, h)
	}
	t.recordBackend(um.defaultService, nil)
	for _, entry := range um.urlMapBackends {
		t.recordBackend(entry.backendURI, entry.paths)
	}
}

func (t *forwardingRuleTracer) finalize() tracedForwardingRule {
	for _, bURI := range t.orderedBackends {
		b := t.st.lbAssets[bURI]
		if b == nil {
			continue
		}
		paths := t.backendPaths[bURI]
		slices.Sort(paths)
		t.out.backends = append(t.out.backends, tracedBackend{
			backendURI:    bURI,
			backendType:   b.assetType,
			bucketName:    b.bucketName,
			backendGroups: b.backendGroups,
			paths:         paths,
		})
	}
	slices.Sort(t.out.hosts)
	slices.Sort(t.out.paths)
	return t.out
}

func (st *extractState) suppressGKENodeInfra() {
	clusterNamesByProj, gkeMIGURIs := st.collectGKEClusterNodeInfo()
	if len(clusterNamesByProj) == 0 {
		return
	}
	for _, a := range st.assets {
		if a.suppressed {
			continue
		}
		if clusters := clusterNamesByProj[a.projectID]; len(clusters) > 0 {
			st.suppressIfGKENodeAsset(a, clusters, gkeMIGURIs)
		}
	}
}

func (st *extractState) collectGKEClusterNodeInfo() (map[string][]string, map[string]bool) {
	clusterNamesByProj := make(map[string][]string)
	gkeMIGURIs := make(map[string]bool)
	for _, a := range st.assets {
		if a.assetType != assetTypeGKECluster || a.suppressed {
			continue
		}
		clusterNamesByProj[a.projectID] = append(clusterNamesByProj[a.projectID], shortResourceName(a.uri))
		for _, ig := range st.gke.clusterIGURLs[a.uri] {
			gkeMIGURIs[normalizeMIGURI(ig, st.projIdx)] = true
		}
	}
	return clusterNamesByProj, gkeMIGURIs
}

func (st *extractState) suppressIfGKENodeAsset(a *ingestedAsset, clusters []string, gkeMIGURIs map[string]bool) {
	switch a.assetType {
	case "compute.googleapis.com/InstanceGroupManager":
		migURI := normalizeMIGURI(a.uri, st.projIdx)
		if gkeMIGURIs[migURI] || matchesAnyGKEClusterPrefix(shortResourceName(a.uri), clusters) {
			a.suppressed = true
			st.suppressed[a.assetType]++
			gkeMIGURIs[migURI] = true
		}
	case "compute.googleapis.com/Instance":
		if isGKEWorkerVM(a, clusters, gkeMIGURIs, st.projIdx) {
			a.suppressed = true
			st.suppressed[a.assetType]++
		}
	}
}

func matchesAnyGKEClusterPrefix(name string, clusters []string) bool {
	for _, c := range clusters {
		if c != "" && (strings.HasPrefix(name, "gke-"+c+"-") || strings.HasPrefix(name, "gk3-"+c+"-")) {
			return true
		}
	}
	return false
}

func isGKEWorkerVM(vm *ingestedAsset, clusters []string, gkeMIGURIs map[string]bool, projIdx map[string]string) bool {
	if creator := vm.computeMetadata["created-by"]; creator != "" {
		if gkeMIGURIs[normalizeMIGURI(creator, projIdx)] {
			return true
		}
	}
	cn := vm.computeMetadata["cluster-name"]
	for _, c := range clusters {
		if c == "" {
			continue
		}
		if cn == c || vm.labels["goog-gke-cluster-name"] == c || vm.labels["goog-k8s-cluster-name"] == c {
			return true
		}
	}
	return matchesAnyGKEClusterPrefix(shortResourceName(vm.uri), clusters)
}

func normalizeMIGURI(raw string, projIdx map[string]string) string {
	canon := CanonicalizeAIP122WithService(raw, svcCompute, projIdx)
	return strings.Replace(canon, "/instanceGroups/", "/instanceGroupManagers/", 1)
}

func (st *extractState) indexVPCAndPrivateIPs() {
	st.indexSubnetsAndConnectors()
	st.indexVPCPeerings()
	st.emitPSAPeeringAndSharedVPC()
	st.attachAlloyDBInstanceIPs()
	st.indexPrivateIPTargets()
}

func (st *extractState) indexSubnetsAndConnectors() {
	for _, a := range st.assets {
		if a.suppressed {
			continue
		}
		switch a.assetType {
		case assetTypeSubnetwork:
			if vpc := getAttrString(a.attrs, "vpc"); vpc != "" {
				st.subnetToVPC[a.uri] = vpc
			}
		case "vpcaccess.googleapis.com/Connector":
			st.indexVPCConnector(a)
		}
	}
}

func (st *extractState) indexVPCConnector(a *ingestedAsset) {
	if vpc := getAttrString(a.attrs, "vpc"); vpc != "" {
		st.connectorToVPC[a.uri] = vpc
		return
	}
	sub := getAttrString(a.attrs, "subnet")
	if sub == "" {
		return
	}
	subProj := getAttrString(a.attrs, "subnet_project")
	if subProj == "" {
		subProj = a.projectID
	}
	if subURI := resolveComputeSubnetURI(sub, subProj, a.location, st.projIdx); subURI != "" {
		if vpc := st.subnetToVPC[subURI]; vpc != "" {
			st.connectorToVPC[a.uri] = vpc
		}
	}
}

// indexVPCPeerings runs a PSA pass before the regular pass so every Private Services Access tenant
// (servicenetworking peering or tenant-ID convention) is recorded before any peer project can be
// registered as an out-of-scope stub.
func (st *extractState) indexVPCPeerings() {
	for _, psaPass := range []bool{true, false} {
		for _, a := range st.assets {
			if a.suppressed || a.assetType != "compute.googleapis.com/Network" {
				continue
			}
			for _, peer := range a.vpcPeerings {
				st.indexSingleVPCPeering(a, peer, psaPass)
			}
		}
	}
}

func (st *extractState) indexSingleVPCPeering(a *ingestedAsset, peer vpcPeeringInfo, psaPass bool) {
	state := strings.ToUpper(strings.TrimSpace(peer.state))
	if (state != "" && state != "ACTIVE") || peer.network == "" {
		return
	}
	peerURI := normalizeComputeNetworkURI(peer.network, a.projectID, st.projIdx)
	peerProj := extractProjectID(peerURI, nil, st.projIdx)
	isPSA := IsPSATenantProject(peerProj) || strings.Contains(strings.ToLower(peer.name), "servicenetworking")
	if psaPass {
		if isPSA {
			st.recordPSATenant(a.uri, peerProj)
		}
		return
	}
	if isPSA || st.isPSATenant(peerProj) {
		return
	}

	st.addVPCPeer(a.uri, peerURI)
	if st.assetsByURI[peerURI] != nil {
		src, dst := a.uri, peerURI
		if src > dst {
			src, dst = dst, src
		}
		st.addEdge(src, dst, EdgeVPCPeering, peer.name)
	} else if peerProj != "" && !st.inScopeProjects[peerProj] {
		if stubURI := st.registerOutOfScopeProject(peerProj, a.uri, oosConfigRefCrossProject); stubURI != "" {
			st.addEdge(a.uri, stubURI, EdgeVPCPeering, peer.name)
		}
	}
}

func (st *extractState) recordPSATenant(vpcURI, peerProj string) {
	if peerProj == "" {
		return
	}
	st.psaTenants[vpcURI] = peerProj
	st.psaProjectSet[peerProj] = true
	p := st.projects[peerProj]
	p.ProjectID = peerProj
	if p.DisplayName == "" {
		p.DisplayName = peerProj
	}
	p.IsPSATenant = true
	st.projects[peerProj] = p
}

func (st *extractState) emitPSAPeeringAndSharedVPC() {
	for _, a := range st.assets {
		if a.suppressed {
			continue
		}
		vpc := st.assetNetworkURI(a)
		if vpc == "" {
			continue
		}
		st.recordSharedVPCHost(a, vpc)
		switch {
		case isPSAManagedService(a.assetType):
			st.emitPSAPeeringEdge(a, vpc)
		case a.assetType == "redis.googleapis.com/Cluster" && st.assetsByURI[vpc] != nil:
			st.addEdge(vpc, a.uri, EdgeConfigRef, "psc_network")
		}
	}
}

func (st *extractState) recordSharedVPCHost(a *ingestedAsset, vpc string) {
	vpcProj := extractProjectID(vpc, nil, st.projIdx)
	if vpcProj == "" || a.projectID == "" || vpcProj == a.projectID || st.isPSATenant(vpcProj) {
		return
	}
	if p, ok := st.projects[a.projectID]; ok && p.SharedVPCHostProject == "" {
		p.SharedVPCHostProject = vpcProj
		st.projects[a.projectID] = p
	}
	if !st.inScopeProjects[vpcProj] {
		if stubURI := st.registerOutOfScopeProject(vpcProj, a.uri, oosConfigRefCrossProject); stubURI != "" {
			st.addEdge(a.uri, stubURI, EdgeConfigRef, "shared_vpc")
		}
	}
}

func (st *extractState) emitPSAPeeringEdge(a *ingestedAsset, vpc string) {
	detail := "servicenetworking-googleapis-com"
	if tenant := st.psaTenants[vpc]; tenant != "" {
		detail = "servicenetworking-googleapis-com (" + tenant + ")"
		a.setAttr("psa_tenant_project", tenant)
	}
	if st.assetsByURI[vpc] != nil {
		st.addEdge(vpc, a.uri, EdgePSAPeering, detail)
	}
}

func (st *extractState) attachAlloyDBInstanceIPs() {
	for _, uri := range slices.Sorted(maps.Keys(st.absorbed)) {
		ab := st.absorbed[uri]
		if ab.assetType != "alloydb.googleapis.com/Instance" || ab.privateIP == "" || ab.parentURI == "" {
			continue
		}
		cl := st.assetsByURI[ab.parentURI]
		if cl == nil {
			continue
		}
		appendUnique(&cl.privateIPs, ab.privateIP)
		cl.setAttrIfEmpty("private_ip", ab.privateIP)
	}
}

func (st *extractState) indexPrivateIPTargets() {
	for _, a := range st.assets {
		if a.suppressed || len(a.privateIPs) == 0 {
			continue
		}
		netURI := st.assetNetworkURI(a)
		if netURI == "" {
			continue
		}
		for _, ip := range a.privateIPs {
			appendUniqueMap(st.privateIPTargets, netIPKey{netURI: netURI, ip: ip}, a.uri)
		}
	}
}

func isPSAManagedService(assetType string) bool {
	switch assetType {
	case "sqladmin.googleapis.com/Instance",
		"alloydb.googleapis.com/Cluster",
		"redis.googleapis.com/Instance",
		"memcache.googleapis.com/Instance",
		"file.googleapis.com/Instance":
		return true
	}
	return false
}

func (st *extractState) addVPCPeer(a, b string) {
	if a == "" || b == "" || a == b {
		return
	}
	if st.vpcPeers[a] == nil {
		st.vpcPeers[a] = make(map[string]bool)
	}
	st.vpcPeers[a][b] = true
	if st.vpcPeers[b] == nil {
		st.vpcPeers[b] = make(map[string]bool)
	}
	st.vpcPeers[b][a] = true
}

func (st *extractState) reachableNetworks(netURI string) []string {
	if netURI == "" {
		return nil
	}
	out := []string{netURI}
	if peers := st.vpcPeers[netURI]; len(peers) > 0 {
		out = append(out, slices.Sorted(maps.Keys(peers))...)
	}
	return out
}

func (st *extractState) matchPrivateIP(srcNet, ip string) []string {
	if srcNet == "" || ip == "" {
		return nil
	}
	var matches []string
	for _, netURI := range st.reachableNetworks(srcNet) {
		for _, targetURI := range st.privateIPTargets[netIPKey{netURI: netURI, ip: ip}] {
			appendUnique(&matches, targetURI)
		}
	}
	slices.Sort(matches)
	return matches
}

func (st *extractState) assetNetworkURI(a *ingestedAsset) string {
	if a == nil {
		return ""
	}
	if vpc := getAttrString(a.attrs, "vpc"); vpc != "" {
		return normalizeComputeNetworkURI(vpc, a.projectID, st.projIdx)
	}
	if sub := getAttrString(a.attrs, "subnet"); sub != "" {
		if vpc := st.subnetToVPC[sub]; vpc != "" {
			return vpc
		}
	}
	if conn := getAttrString(a.attrs, "vpc_connector"); conn != "" {
		if vpc := st.connectorToVPC[conn]; vpc != "" {
			return vpc
		}
	}
	return st.parentNetworkURI(a)
}

func (st *extractState) parentNetworkURI(a *ingestedAsset) string {
	if a.parentURI == "" {
		return ""
	}
	if strings.Contains(a.parentURI, "/networks/") {
		return a.parentURI
	}
	if vpc := st.subnetToVPC[a.parentURI]; vpc != "" {
		return vpc
	}
	// GKE workloads inherit VPC from their parent container.googleapis.com/Cluster.
	if parent := st.assetsByURI[a.parentURI]; parent != nil && parent != a {
		return st.assetNetworkURI(parent)
	}
	return ""
}

func normalizeComputeNetworkURI(raw, projID string, projIdx map[string]string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "/") && projID != "" {
		return prefixComputeProjects + mapProject(projID, projIdx) + "/global/networks/" + s
	}
	return CanonicalizeAIP122WithService(s, svcCompute, projIdx)
}

func (st *extractState) indexCloudRunHosts() {
	for _, a := range st.assets {
		if !isServerlessHTTPAsset(a) {
			continue
		}
		for _, u := range a.runURLs {
			st.registerCloudRunURL(u, a.uri)
		}
		if name := strings.ToLower(shortResourceName(a.uri)); name != "" {
			appendUniqueMap(st.runNameToURIs, name, a.uri)
		}
	}
}

func isServerlessHTTPAsset(a *ingestedAsset) bool {
	return !a.suppressed && (a.assetType == assetTypeRunService || a.assetType == "cloudfunctions.googleapis.com/CloudFunction")
}

func (st *extractState) registerCloudRunURL(rawURL, uri string) {
	su := SanitizeURL(rawURL)
	if su == "" {
		return
	}
	low := strings.ToLower(su)
	if _, exists := st.runHostToURI[low]; !exists {
		st.runHostToURI[low] = uri
	}
	trimmed := stripHTTPURLScheme(low)
	host, _, _ := strings.Cut(trimmed, "/")
	if host == "" {
		return
	}
	if _, exists := st.runHostToURI[host]; !exists {
		st.runHostToURI[host] = uri
	}
}

func (st *extractState) resolveHTTPTarget(rawURL string) string {
	su := strings.ToLower(SanitizeURL(rawURL))
	if su == "" {
		return ""
	}
	if uri, ok := st.runHostToURI[su]; ok {
		return uri
	}
	trimmed := stripHTTPURLScheme(su)
	host, path, _ := strings.Cut(trimmed, "/")
	if uri, ok := st.runHostToURI[host]; ok && !strings.HasSuffix(host, ".cloudfunctions.net") {
		return uri
	}
	if m := runAppHostRe.FindStringSubmatch(host); len(m) == 2 {
		if matches := st.runNameToURIs[m[1]]; len(matches) == 1 {
			return matches[0]
		}
	}
	if strings.HasSuffix(host, ".cloudfunctions.net") && path != "" {
		fnName, _, _ := strings.Cut(path, "/")
		if matches := st.runNameToURIs[strings.ToLower(fnName)]; len(matches) == 1 {
			return matches[0]
		}
	}
	if strings.Contains(rawURL, ".googleapis.com/") {
		canon := CanonicalizeAIP122(SanitizeURL(rawURL), st.projIdx)
		if st.assetsByURI[canon] != nil {
			return canon
		}
	}
	return ""
}

func (st *extractState) resolveLoadBalancerNonGKEBackends() {
	for _, a := range st.assets {
		if a.suppressed || (a.assetType != "compute.googleapis.com/ForwardingRule" && a.assetType != "compute.googleapis.com/GlobalForwardingRule") {
			continue
		}
		tr := st.traceForwardingRule(a)
		for _, be := range tr.backends {
			st.resolveSingleNonGKEBackend(a.uri, be)
		}
	}
}

func (st *extractState) resolveSingleNonGKEBackend(frURI string, be tracedBackend) {
	detail := strings.Join(be.paths, ",")
	if be.backendType == "compute.googleapis.com/BackendBucket" && be.bucketName != "" {
		bktURI := normalizeBucketURI(be.bucketName, st.projIdx)
		st.emitTargetEdge(frURI, bktURI, EdgeConfigRef, detail, oosConfigRefCrossProject)
		return
	}
	for _, grpURI := range be.backendGroups {
		if st.gkeResolvedNEGs[frURI+"|"+grpURI] {
			continue
		}
		st.resolveNonGKEBackendGroup(frURI, grpURI, detail)
	}
}

func (st *extractState) resolveNonGKEBackendGroup(frURI, grpURI, detail string) {
	if neg := st.lbAssets[grpURI]; neg != nil && neg.assetType == "compute.googleapis.com/NetworkEndpointGroup" {
		st.emitServerlessOrPSCNEGEdge(frURI, grpURI, neg, detail)
		return
	}
	migURI := normalizeMIGURI(grpURI, st.projIdx)
	if st.assetsByURI[migURI] != nil && !st.assetsByURI[migURI].suppressed {
		st.addEdge(frURI, migURI, EdgeConfigRef, detail)
	} else if st.assetsByURI[grpURI] != nil && !st.assetsByURI[grpURI].suppressed {
		st.addEdge(frURI, grpURI, EdgeConfigRef, detail)
	}
}

func (st *extractState) emitServerlessOrPSCNEGEdge(frURI, grpURI string, neg *lbIntermediate, detail string) {
	loc := neg.location
	if loc == "" {
		loc = extractLocationFromURI(grpURI)
	}
	switch {
	case neg.negCloudRunSvc != "":
		uri := prefixRunProjects + neg.projectID + segLocations + loc + segServices + neg.negCloudRunSvc
		st.emitTargetEdge(frURI, uri, EdgeConfigRef, detail, oosConfigRefCrossProject)
	case neg.negCloudFn != "":
		uri := "//cloudfunctions.googleapis.com/projects/" + neg.projectID + segLocations + loc + "/functions/" + neg.negCloudFn
		st.emitTargetEdge(frURI, uri, EdgeConfigRef, detail, oosConfigRefCrossProject)
	case neg.negAppEngineSvc != "":
		uri := "//appengine.googleapis.com/apps/" + neg.projectID + segServices + neg.negAppEngineSvc
		st.emitTargetEdge(frURI, uri, EdgeConfigRef, detail, oosConfigRefCrossProject)
	case neg.negPSCTarget != "":
		st.emitTargetEdge(frURI, neg.negPSCTarget, EdgeConfigRef, detail, oosConfigRefCrossProject)
	}
}

func (st *extractState) resolveStructuralRefs() {
	for _, a := range st.assets {
		if a.suppressed {
			continue
		}
		for _, ref := range a.outboundRefs {
			targetURI := st.resolveStructuralTarget(a, ref)
			if targetURI == "" {
				continue
			}
			if ref.direction == "in" {
				st.emitTargetEdge(targetURI, a.uri, EdgeConfigRef, ref.detail, oosConfigRefCrossProject)
			} else {
				st.emitTargetEdge(a.uri, targetURI, EdgeConfigRef, ref.detail, oosConfigRefCrossProject)
			}
		}
	}
}

func (st *extractState) resolveStructuralTarget(src *ingestedAsset, ref structuralRef) string {
	switch ref.kind {
	case "http":
		return st.resolveHTTPTarget(ref.targetURI)
	case "bucket":
		return normalizeBucketURI(ref.targetURI, st.projIdx)
	case "bq_dataset":
		return normalizeBQDatasetURI(ref.targetURI, src.projectID, st.projIdx)
	case "cloudsql":
		return st.resolveCloudSQLRef(ref.targetURI, src.projectID)
	case "datastore_id":
		return st.resolveDataStoreIDRef(ref.targetURI, src.projectID)
	}

	canon := CanonicalizeAIP122WithService(ref.targetURI, ref.defaultSvc, st.projIdx)
	if !strings.HasPrefix(canon, "//") {
		return ""
	}
	return st.resolveAbsorbedTargetURI(canon)
}

func (st *extractState) resolveCloudSQLRef(targetURI, srcProj string) string {
	if uri := cloudSQLConnNameToURI(targetURI, st.projIdx); uri != "" {
		return uri
	}
	if !strings.Contains(targetURI, "/") {
		return prefixSQLProjects + srcProj + segInstances + targetURI
	}
	return CanonicalizeAIP122WithService(targetURI, "sqladmin.googleapis.com", st.projIdx)
}

func (st *extractState) resolveDataStoreIDRef(targetURI, srcProj string) string {
	if strings.Contains(targetURI, "/") {
		return CanonicalizeAIP122WithService(targetURI, "discoveryengine.googleapis.com", st.projIdx)
	}
	for _, cand := range st.assets {
		if !cand.suppressed && cand.assetType == "discoveryengine.googleapis.com/DataStore" &&
			cand.projectID == srcProj && shortResourceName(cand.uri) == targetURI {
			return cand.uri
		}
	}
	return ""
}

func (st *extractState) resolveAbsorbedTargetURI(canon string) string {
	// Resolve VpnTunnel -> VpnGateway transparently when Router links a VpnTunnel.
	ab := st.absorbed[canon]
	if ab == nil {
		return canon
	}
	if ab.assetType == "compute.googleapis.com/VpnTunnel" && ab.vpnGateway != "" {
		return ab.vpnGateway
	}
	if ab.assetType == "datastream.googleapis.com/ConnectionProfile" {
		return "" // Handled in resolveAbsorbedBridges for Datastream Stream.
	}
	return canon
}

func (st *extractState) resolveAbsorbedBridges() {
	st.resolveAbsorbedResourceBridges()
	st.resolveDatastreamStreamBridges()
	st.resolveBigQueryFoldedExternalURIs()
}

func (st *extractState) resolveAbsorbedResourceBridges() {
	for _, uri := range slices.Sorted(maps.Keys(st.absorbed)) {
		st.resolveSingleAbsorbedBridge(st.absorbed[uri])
	}
}

func (st *extractState) resolveSingleAbsorbedBridge(ab *absorbedRecord) {
	switch ab.assetType {
	case "compute.googleapis.com/VpnTunnel":
		if ab.router != "" && ab.vpnGateway != "" {
			st.emitTargetEdge(ab.router, ab.vpnGateway, EdgeConfigRef, "vpn_tunnel", oosConfigRefCrossProject)
		}
	case "networkconnectivity.googleapis.com/Spoke":
		st.resolveNCCSpokeBridge(ab)
	case "aiplatform.googleapis.com/FeatureView":
		st.emitAbsorbedBQSourceEdge(ab, "feature_sync")
	case "analyticshub.googleapis.com/Listing":
		st.emitAbsorbedBQSourceEdge(ab, "analytics_hub_listing")
	case "dataplex.googleapis.com/Asset":
		if ab.parentURI != "" && ab.resourceSpecName != "" {
			tgt := CanonicalizeAIP122(ab.resourceSpecName, st.projIdx)
			st.emitTargetEdge(ab.parentURI, tgt, EdgeConfigRef, "dataplex_asset", oosConfigRefCrossProject)
		}
	}
}

func (st *extractState) emitAbsorbedBQSourceEdge(ab *absorbedRecord, detail string) {
	if ab.parentURI == "" || ab.bqSourceURI == "" {
		return
	}
	dsURI := normalizeBQDatasetURI(ab.bqSourceURI, ab.projectID, st.projIdx)
	st.emitTargetEdge(dsURI, ab.parentURI, EdgeConfigRef, detail, oosConfigRefCrossProject)
}

func (st *extractState) resolveNCCSpokeBridge(ab *absorbedRecord) {
	if ab.hub == "" || st.assetsByURI[ab.hub] == nil {
		return
	}
	if ab.linkedVPC != "" {
		st.emitTargetEdge(ab.hub, ab.linkedVPC, EdgeConfigRef, detailNCCSpoke, oosConfigRefCrossProject)
	}
	for _, v := range ab.linkedVPNs {
		if tun := st.absorbed[v]; tun != nil && tun.vpnGateway != "" {
			st.emitTargetEdge(ab.hub, tun.vpnGateway, EdgeConfigRef, detailNCCSpoke, oosConfigRefCrossProject)
		}
	}
	for _, ic := range ab.linkedInterconnects {
		st.emitTargetEdge(ab.hub, ic, EdgeConfigRef, detailNCCSpoke, oosConfigRefCrossProject)
	}
}

func (st *extractState) resolveDatastreamStreamBridges() {
	for _, a := range st.assets {
		if a.suppressed || a.assetType != "datastream.googleapis.com/Stream" {
			continue
		}
		for _, ref := range a.outboundRefs {
			st.resolveDatastreamStreamRef(a, ref)
		}
	}
}

func (st *extractState) resolveDatastreamStreamRef(a *ingestedAsset, ref structuralRef) {
	cpURI := CanonicalizeAIP122WithService(ref.targetURI, svcDatastream, st.projIdx)
	cp := st.absorbed[cpURI]
	if cp == nil || cp.assetType != "datastream.googleapis.com/ConnectionProfile" {
		return
	}
	if ref.direction != "in" {
		if cp.gcsBucket != "" {
			bktURI := normalizeBucketURI(cp.gcsBucket, st.projIdx)
			st.emitTargetEdge(a.uri, bktURI, EdgeConfigRef, "datastream.gcs", oosConfigRefCrossProject)
		}
		return
	}
	cpNet := st.assetNetworkURI(a)
	if pc := st.absorbed[cp.privateConnection]; pc != nil && pc.vpc != "" {
		cpNet = pc.vpc
	}
	if cpNet == "" {
		return
	}
	for _, host := range cp.hostnames {
		if ip := parsePrivateIPv4(host); ip != "" {
			if matches := st.matchPrivateIP(cpNet, ip); len(matches) == 1 {
				st.addEdge(matches[0], a.uri, EdgeConfigRef, "datastream.cdc:"+ip)
			}
		}
	}
}

func (st *extractState) resolveBigQueryFoldedExternalURIs() {
	for _, dsURI := range slices.Sorted(maps.Keys(st.foldedExternalURIs)) {
		if st.assetsByURI[dsURI] == nil {
			continue
		}
		for _, ext := range st.foldedExternalURIs[dsURI] {
			st.resolveSingleBigQueryExternalURI(dsURI, ext)
		}
	}
}

func (st *extractState) resolveSingleBigQueryExternalURI(dsURI, ext string) {
	if hasHTTPURLScheme(ext) {
		if tgt := st.resolveHTTPTarget(ext); tgt != "" {
			st.addEdge(dsURI, tgt, EdgeConfigRef, "bq_remote_fn")
		}
		return
	}
	canon := CanonicalizeAIP122(ext, st.projIdx)
	if strings.HasPrefix(canon, prefixStorageURI) {
		st.emitTargetEdge(canon, dsURI, EdgeConfigRef, "biglake", oosConfigRefCrossProject)
		return
	}
	if strings.HasPrefix(canon, "//") {
		st.emitTargetEdge(dsURI, canon, EdgeConfigRef, "bq_external", oosConfigRefCrossProject)
	}
}

func (st *extractState) resolveEnvAndSecretRefs() {
	for _, a := range st.assets {
		if a.suppressed {
			continue
		}
		srcNet := st.assetNetworkURI(a)
		for _, ref := range a.envRefs {
			st.resolveSingleConfigRef(a.uri, a.projectID, srcNet, ref, EdgeConfigRef, oosConfigRefCrossProject)
		}
		for _, sec := range a.secretRefs {
			secURI := stripSecretVersion(CanonicalizeAIP122WithService(sec, "secretmanager.googleapis.com", st.projIdx))
			st.emitTargetEdge(a.uri, secURI, EdgeConfigRef, "secret_ref", oosConfigRefCrossProject)
		}
	}
}

func (st *extractState) resolveCloudBuildRefList(sourceURI, srcProj string, refs, secRefs []string) {
	// Capture any explicit --project=<id> target in this build so --cluster / run-deploy know target project.
	targetProjects := extractCloudBuildTargetProjects(refs)
	for _, ref := range refs {
		st.resolveSingleCloudBuildRef(sourceURI, srcProj, ref, targetProjects)
	}
	for _, sec := range secRefs {
		secURI := stripSecretVersion(CanonicalizeAIP122WithService(sec, "secretmanager.googleapis.com", st.projIdx))
		st.emitTargetEdge(sourceURI, secURI, EdgeCloudBuildStepRef, "secret_ref", oosCloudBuildDeployFlag)
	}
}

func extractCloudBuildTargetProjects(refs []string) []string {
	var targetProjects []string
	for _, r := range refs {
		if proj, ok := strings.CutPrefix(r, flagProjectPrefix); ok {
			appendUnique(&targetProjects, proj)
		}
	}
	return targetProjects
}

func (st *extractState) resolveSingleCloudBuildRef(sourceURI, srcProj, ref string, targetProjects []string) {
	switch {
	case strings.HasPrefix(ref, flagProjectPrefix):
		st.emitOutOfScopeProjectEdge(sourceURI, strings.TrimPrefix(ref, flagProjectPrefix), ref, EdgeCloudBuildStepRef, oosCloudBuildDeployFlag)
	case strings.HasPrefix(ref, prefixGCR):
		st.emitOutOfScopeProjectEdge(sourceURI, strings.TrimPrefix(ref, prefixGCR), ref, EdgeCloudBuildStepRef, oosCloudBuildDeployFlag)
	case strings.HasPrefix(ref, flagClusterPrefix):
		st.resolveCloudBuildClusterRef(sourceURI, srcProj, strings.TrimPrefix(ref, flagClusterPrefix))
	case strings.HasPrefix(ref, prefixRunDeploy):
		if tgt := st.findUniqueAssetByName(assetTypeRunService, strings.TrimPrefix(ref, prefixRunDeploy), srcProj, targetProjects); tgt != "" {
			st.addEdge(sourceURI, tgt, EdgeCloudBuildStepRef, ref)
		}
	default:
		st.resolveSingleConfigRef(sourceURI, srcProj, "", ref, EdgeCloudBuildStepRef, oosCloudBuildDeployFlag)
	}
}

// resolveCloudBuildClusterRef links a bare --cluster name to the single same-named GKE cluster within
// the step's --project scope (encoded by scopeStepClusterRefs as "<p1,p2>/<name>"), else the build's
// project, else every scanned project; ambiguous or unknown names emit no edge.
func (st *extractState) resolveCloudBuildClusterRef(sourceURI, srcProj, val string) {
	name := val
	var stepProjects []string
	if scope, n, scoped := strings.Cut(val, "/"); scoped {
		name, stepProjects = n, strings.Split(scope, ",")
	}
	if tgt := st.findUniqueAssetByName(assetTypeGKECluster, name, srcProj, stepProjects); tgt != "" {
		st.addEdge(sourceURI, tgt, EdgeCloudBuildStepRef, flagClusterPrefix+name)
	}
}

func (st *extractState) emitOutOfScopeProjectEdge(srcURI, proj, detail string, edgeKind FactEdgeKind, oosKind string) {
	if proj == "" || st.inScopeProjects[proj] {
		return
	}
	if stubURI := st.registerOutOfScopeProject(proj, srcURI, oosKind); stubURI != "" {
		st.addEdge(srcURI, stubURI, edgeKind, detail)
	}
}

func (st *extractState) resolveSingleConfigRef(srcURI, srcProj, srcNet, rawRef string, edgeKind FactEdgeKind, oosKind string) {
	s := strings.TrimSpace(rawRef)
	if s == "" {
		return
	}
	switch {
	case strings.HasPrefix(s, "gs://"):
		st.emitTargetEdge(srcURI, normalizeBucketURI(s, st.projIdx), edgeKind, s, oosKind)
	case hasHTTPURLScheme(s):
		if tgt := st.resolveHTTPTarget(s); tgt != "" {
			st.addEdge(srcURI, tgt, edgeKind, s)
		}
	case strings.HasPrefix(s, "//") || strings.HasPrefix(s, prefixProjects):
		st.resolveAIP122ConfigRef(srcURI, s, edgeKind, oosKind)
	case strings.HasPrefix(s, flagClusterPrefix), strings.HasPrefix(s, prefixRunDeploy), strings.HasPrefix(s, flagProjectPrefix):
		st.resolveCommandFlagConfigRef(srcURI, srcProj, s, edgeKind, oosKind)
	default:
		st.resolveIPOrDatasetConfigRef(srcURI, srcNet, s, edgeKind, oosKind)
	}
}

func (st *extractState) resolveAIP122ConfigRef(srcURI, s string, edgeKind FactEdgeKind, oosKind string) {
	canon := CanonicalizeAIP122(s, st.projIdx)
	if !strings.HasPrefix(canon, "//") {
		return
	}
	if strings.Contains(canon, "/subscriptions/") {
		st.emitTargetEdge(canon, srcURI, edgeKind, "pubsub_subscription", oosKind)
		return
	}
	st.emitTargetEdge(srcURI, canon, edgeKind, shortRefDetail(s, canon), oosKind)
}

func (st *extractState) resolveCommandFlagConfigRef(srcURI, srcProj, s string, edgeKind FactEdgeKind, oosKind string) {
	switch {
	case strings.HasPrefix(s, flagClusterPrefix):
		if tgt := st.findUniqueAssetByName(assetTypeGKECluster, strings.TrimPrefix(s, flagClusterPrefix), srcProj, nil); tgt != "" {
			st.addEdge(srcURI, tgt, edgeKind, s)
		}
	case strings.HasPrefix(s, prefixRunDeploy):
		if tgt := st.findUniqueAssetByName(assetTypeRunService, strings.TrimPrefix(s, prefixRunDeploy), srcProj, nil); tgt != "" {
			st.addEdge(srcURI, tgt, edgeKind, s)
		}
	case strings.HasPrefix(s, flagProjectPrefix):
		st.emitOutOfScopeProjectEdge(srcURI, strings.TrimPrefix(s, flagProjectPrefix), s, edgeKind, oosKind)
	}
}

func (st *extractState) resolveIPOrDatasetConfigRef(srcURI, srcNet, s string, edgeKind FactEdgeKind, oosKind string) {
	if ip := parsePrivateIPv4(s); ip != "" {
		if srcNet != "" {
			if matches := st.matchPrivateIP(srcNet, ip); len(matches) == 1 {
				st.addEdge(srcURI, matches[0], edgeKind, ip)
			}
		}
		return
	}
	if proj, ds, ok := parseProjectDatasetRef(s, st.projIdx); ok && st.isKnownProjectID(proj) {
		dsURI := prefixBQProjects + proj + segDatasets + ds
		st.emitTargetEdge(srcURI, dsURI, edgeKind, proj+"."+ds, oosKind)
	}
}

func (st *extractState) isInScopeEndpoint(uri string) bool {
	if a := st.assetsByURI[uri]; a != nil && !a.suppressed {
		return true
	}
	if strings.HasPrefix(uri, "//cloudresourcemanager.googleapis.com/projects/") {
		return st.inScopeProjects[shortResourceName(uri)]
	}
	return false
}

func (st *extractState) emitTargetEdge(fromURI, toURI string, kind FactEdgeKind, detail, oosKind string) {
	if from, to, ok := st.resolveEdgeEndpoints(fromURI, toURI, oosKind); ok {
		st.addEdge(from, to, kind, detail)
	}
}

// resolveEdgeEndpoints keeps in-scope endpoints, maps out-of-scope ones to synthetic project stubs,
// and rejects edges with no in-scope endpoint.
func (st *extractState) resolveEdgeEndpoints(fromURI, toURI, oosKind string) (string, string, bool) {
	if fromURI == "" || toURI == "" || fromURI == toURI {
		return "", "", false
	}
	fromInScope := st.isInScopeEndpoint(fromURI)
	toInScope := st.isInScopeEndpoint(toURI)
	if !fromInScope && !toInScope {
		return "", "", false
	}

	resolvedFrom, okFrom := st.resolveTargetEndpoint(fromURI, fromInScope, toURI, oosKind)
	if !okFrom {
		return "", "", false
	}
	resolvedTo, okTo := st.resolveTargetEndpoint(toURI, toInScope, fromURI, oosKind)
	if !okTo || resolvedFrom == resolvedTo {
		return "", "", false
	}
	if fromInScope {
		st.referencedURIs[resolvedFrom] = true
	}
	if toInScope {
		st.referencedURIs[resolvedTo] = true
	}
	return resolvedFrom, resolvedTo, true
}

func (st *extractState) resolveTargetEndpoint(uri string, inScope bool, peerURI, oosKind string) (string, bool) {
	if inScope {
		return uri, true
	}
	if _, ext := st.externalResources[uri]; ext {
		return uri, true
	}
	proj := extractProjectID(uri, nil, st.projIdx)
	if proj == "" || st.inScopeProjects[proj] {
		return "", false
	}
	if stubURI := st.registerOutOfScopeProject(proj, peerURI, oosKind); stubURI != "" {
		return stubURI, true
	}
	return "", false
}

type projAssetTypeKey struct {
	project   string
	assetType string
}

func (st *extractState) resolveIAMBindings() {
	byProjType := make(map[projAssetTypeKey][]*ingestedAsset)
	for _, a := range st.assets {
		if a.suppressed {
			continue
		}
		k := projAssetTypeKey{project: a.projectID, assetType: a.assetType}
		byProjType[k] = append(byProjType[k], a)
	}

	for _, b := range st.iamBindings {
		isAncestor := b.assetType == assetTypeOrganization ||
			b.assetType == assetTypeFolder ||
			b.assetType == assetTypeProject
		for _, email := range b.saEmails {
			st.resolveSingleIAMBindingMember(b, email, isAncestor, byProjType)
		}
	}
}

func (st *extractState) resolveSingleIAMBindingMember(b *iamBindingRecord, email string, isAncestor bool, byProjType map[projAssetTypeKey][]*ingestedAsset) {
	if isAncestor && broadAncestorRoles[b.role] {
		if sa := st.saByEmail[email]; sa != nil {
			appendUnique(&sa.capabilities, b.role)
		}
		return
	}

	targets, heuristic := st.resolveIAMTargets(b, isAncestor, email, byProjType)
	if len(targets) == 0 {
		return
	}
	st.emitIAMBindingEdges(b, email, targets, heuristic)
}

// resolveIAMTargets returns the binding's target resources; heuristic is true when a project-level
// role was attributed only because the project holds a single instance of the role's target service.
func (st *extractState) resolveIAMTargets(b *iamBindingRecord, isAncestor bool, email string, byProjType map[projAssetTypeKey][]*ingestedAsset) ([]*ingestedAsset, bool) {
	if !isAncestor {
		if target := st.assetsByURI[b.resourceURI]; target != nil && !target.suppressed {
			return []*ingestedAsset{target}, false
		}
		return nil, false
	}
	if b.assetType != assetTypeProject {
		return nil, false
	}
	if targets := st.resolveConditionPinnedTargets(b.conditionExpr); len(targets) > 0 {
		return targets, false
	}
	if isDefaultServiceAccount(email) {
		return nil, false
	}
	return resolveUnambiguousRoleTarget(b.projectID, b.role, byProjType), true
}

func (st *extractState) resolveConditionPinnedTargets(expr string) []*ingestedAsset {
	if expr == "" {
		return nil
	}
	var targets []*ingestedAsset
	for _, tgtURI := range extractConditionPinnedURIs(expr, st.projIdx) {
		if target := st.assetsByURI[tgtURI]; target != nil && !target.suppressed {
			targets = append(targets, target)
		}
	}
	return targets
}

func resolveUnambiguousRoleTarget(projectID, role string, byProjType map[projAssetTypeKey][]*ingestedAsset) []*ingestedAsset {
	targetTypes := matchServiceRoleTargetTypes(role)
	if len(targetTypes) == 0 {
		return nil
	}
	var candidates []*ingestedAsset
	for _, tt := range targetTypes {
		candidates = append(candidates, byProjType[projAssetTypeKey{project: projectID, assetType: tt}]...)
	}
	if len(candidates) == 1 {
		return candidates
	}
	return nil
}

func (st *extractState) emitIAMBindingEdges(b *iamBindingRecord, email string, targets []*ingestedAsset, heuristic bool) {
	saURI := st.serviceAccountURI(email, b.projectID)
	saProj := extractProjectID(saURI, nil, st.projIdx)
	if st.assetsByURI[saURI] == nil && isDefaultServiceAccount(email) {
		return
	}
	if saProj != "" && !st.inScopeProjects[saProj] {
		for _, target := range targets {
			srcURI := st.registerOutOfScopeProject(saProj, target.uri, "iam_cross_project")
			st.putEdge(FactEdge{FromURI: srcURI, ToURI: target.uri, Kind: EdgeIAMResourceBinding, Detail: b.role, Heuristic: heuristic})
		}
		return
	}
	if st.assetsByURI[saURI] == nil {
		st.ensureInScopeServiceAccount(saURI, saProj, email)
	}
	for _, target := range targets {
		if saURI != target.uri {
			st.referencedURIs[saURI] = true
			st.putEdge(FactEdge{FromURI: saURI, ToURI: target.uri, Kind: EdgeIAMResourceBinding, Detail: b.role, Heuristic: heuristic})
		}
	}
}

func extractConditionPinnedURIs(expr string, projIdx map[string]string) []string {
	var out []string
	for _, m := range iamCondResNameRe.FindAllStringSubmatch(expr, -1) {
		canon := CanonicalizeAIP122(m[1], projIdx)
		if strings.HasPrefix(canon, "//") {
			appendUnique(&out, canon)
		}
	}
	eR, sR := ExtractSanitizedRefs(expr, projIdx)
	for _, r := range append(eR, sR...) {
		if strings.HasPrefix(r, "//") {
			appendUnique(&out, stripSecretVersion(r))
		}
	}
	slices.Sort(out)
	return out
}

func matchServiceRoleTargetTypes(role string) []string {
	low := strings.ToLower(strings.TrimSpace(role))
	for _, entry := range serviceRoleTargetTypes {
		if strings.HasPrefix(low, entry.prefix) {
			return entry.assetTypes
		}
	}
	return nil
}

func parseServiceAccountMember(member string) (string, bool) {
	m := strings.TrimSpace(member)
	if !strings.HasPrefix(m, "serviceAccount:") {
		return "", false
	}
	email := strings.ToLower(strings.TrimPrefix(m, "serviceAccount:"))
	// Exclude GKE WIF pool principals like proj.svc.id.goog[ns/ksa].
	if !strings.Contains(email, "@") || strings.Contains(email, "[") {
		return "", false
	}
	return email, true
}

func (st *extractState) isPSATenant(projectID string) bool {
	return IsPSATenantProject(projectID) || st.psaProjectSet[projectID]
}

func (st *extractState) registerOutOfScopeProject(targetProj, referencedByURI, sourceKind string) string {
	proj := mapProject(strings.TrimSpace(targetProj), st.projIdx)
	if proj == "" || st.inScopeProjects[proj] || st.isPSATenant(proj) || publicBuilderMirror[strings.ToLower(proj)] {
		return ""
	}
	syntheticURI := "//cloudresourcemanager.googleapis.com/projects/" + proj
	if _, exists := st.externalResources[syntheticURI]; !exists {
		st.externalResources[syntheticURI] = FactResource{
			URI:         syntheticURI,
			AssetType:   assetTypeProject,
			ProjectID:   proj,
			DisplayName: proj,
			IsExternal:  true,
		}
	}
	if sourceKind == "" {
		sourceKind = oosConfigRefCrossProject
	}
	k := oosKey{targetProjectID: proj, sourceKind: sourceKind}
	hint, ok := st.outOfScope[k]
	if !ok {
		hint = &OutOfScopeHint{
			TargetProjectID: proj,
			SyntheticURI:    syntheticURI,
			SourceKind:      sourceKind,
		}
		st.outOfScope[k] = hint
	}
	if referencedByURI != "" {
		appendUnique(&hint.ReferencedByURIs, referencedByURI)
	}
	return syntheticURI
}

func (st *extractState) serviceAccountURI(email, fallbackProj string) string {
	low := strings.ToLower(strings.TrimSpace(email))
	if low == "default" {
		if num := st.projectNumberFor(fallbackProj); num != "" {
			low = num + "-compute@developer.gserviceaccount.com"
		}
	}
	if sa := st.saByEmail[low]; sa != nil {
		return sa.uri
	}
	saProj := ""
	if local, domain, ok := strings.Cut(low, "@"); ok {
		switch {
		case strings.HasPrefix(local, prefixServiceDash) && isNumeric(strings.TrimPrefix(local, prefixServiceDash)):
			saProj = mapProject(strings.TrimPrefix(local, prefixServiceDash), st.projIdx)
		case domain == "cloudservices.gserviceaccount.com" || domain == "cloudbuild.gserviceaccount.com":
			if isNumeric(local) {
				saProj = mapProject(local, st.projIdx)
			}
		case strings.HasSuffix(domain, ".iam.gserviceaccount.com"):
			saProj = mapProject(strings.TrimSuffix(domain, ".iam.gserviceaccount.com"), st.projIdx)
		case domain == "appspot.gserviceaccount.com":
			saProj = mapProject(local, st.projIdx)
		case domain == "developer.gserviceaccount.com":
			num := strings.TrimSuffix(local, "-compute")
			saProj = mapProject(num, st.projIdx)
		}
	}
	if saProj == "" {
		saProj = mapProject(fallbackProj, st.projIdx)
	}
	return "//iam.googleapis.com/projects/" + saProj + "/serviceAccounts/" + low
}

func (st *extractState) ensureInScopeServiceAccount(uri, projID, email string) {
	if st.assetsByURI[uri] != nil || projID == "" || !st.inScopeProjects[projID] {
		return
	}
	low := strings.ToLower(strings.TrimSpace(email))
	rec := &ingestedAsset{
		uri:         uri,
		assetType:   assetTypeServiceAccount,
		projectID:   projID,
		displayName: low,
		attrs:       map[string]any{"email": low},
		saEmail:     low,
	}
	st.assets = append(st.assets, rec)
	st.assetsByURI[uri] = rec
	st.saByEmail[low] = rec
}

func (st *extractState) markServiceAccountRef(srcURI, srcProj, email string) {
	low := strings.ToLower(strings.TrimSpace(email))
	if low == "" {
		return
	}
	saURI := st.serviceAccountURI(low, srcProj)
	st.referencedURIs[saURI] = true
	if sa := st.assetsByURI[saURI]; sa != nil && !sa.suppressed && srcURI != saURI {
		st.addEdge(srcURI, saURI, EdgeConfigRef, "service_account")
	}
}

func buildAllowedProjects(srcProj string, targetProjs []string) map[string]bool {
	allowed := make(map[string]bool, len(targetProjs))
	for _, p := range targetProjs {
		allowed[p] = true
	}
	if len(allowed) == 0 && srcProj != "" {
		allowed[srcProj] = true
	}
	return allowed
}

func (st *extractState) findUniqueAssetByName(assetType, name, srcProj string, targetProjs []string) string {
	allowedProjs := buildAllowedProjects(srcProj, targetProjs)
	var matches []string
	for _, a := range st.assets {
		if a.suppressed || a.assetType != assetType {
			continue
		}
		if len(allowedProjs) > 0 && !allowedProjs[a.projectID] {
			continue
		}
		if shortResourceName(a.uri) == name {
			matches = append(matches, a.uri)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

func (st *extractState) isKnownProjectID(proj string) bool {
	if proj == "" {
		return false
	}
	if st.inScopeProjects[proj] {
		return true
	}
	for _, mapped := range st.projIdx {
		if mapped == proj {
			return true
		}
	}
	return false
}

func normalizeBucketURI(raw string, projIdx map[string]string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "gs://") || strings.HasPrefix(s, "//") || strings.HasPrefix(s, "http") || strings.Contains(s, "/") {
		return CanonicalizeAIP122WithService(s, svcStorage, projIdx)
	}
	return prefixStorageURI + s
}

func normalizeBQDatasetURI(raw, defaultProj string, projIdx map[string]string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "//") || strings.HasPrefix(s, prefixProjects) || strings.HasPrefix(s, "http") {
		canon := CanonicalizeAIP122WithService(s, svcBigQuery, projIdx)
		if idx := strings.Index(canon, "/tables/"); idx > 0 {
			return canon[:idx]
		}
		return canon
	}
	if proj, ds, ok := parseProjectDatasetRef(s, projIdx); ok {
		return prefixBQProjects + proj + segDatasets + ds
	}
	if !strings.Contains(s, "/") && defaultProj != "" {
		return prefixBQProjects + mapProject(defaultProj, projIdx) + segDatasets + s
	}
	return ""
}

func parseProjectDatasetRef(s string, projIdx map[string]string) (proj, ds string, ok bool) {
	cleaned := strings.TrimPrefix(strings.TrimSpace(s), "bq://")
	if strings.Contains(cleaned, "/") || strings.Contains(cleaned, " ") {
		return "", "", false
	}
	var sep string
	switch {
	case strings.Contains(cleaned, ":"):
		sep = ":"
	case strings.Contains(cleaned, "."):
		sep = "."
	default:
		return "", "", false
	}
	p, rest, found := strings.Cut(cleaned, sep)
	if !found || p == "" || rest == "" {
		return "", "", false
	}
	d, _, _ := strings.Cut(rest, ".")
	if d == "" {
		return "", "", false
	}
	return mapProject(p, projIdx), d, true
}

func shortRefDetail(raw, canon string) string {
	if strings.HasPrefix(canon, "//artifactregistry.googleapis.com/") {
		loc := extractSegmentAfter(canon, "locations")
		proj := extractSegmentAfter(canon, "projects")
		repo := extractSegmentAfter(canon, "repositories")
		if loc != "" && proj != "" && repo != "" {
			return loc + "-docker.pkg.dev/" + proj + "/" + repo
		}
	}
	return raw
}
