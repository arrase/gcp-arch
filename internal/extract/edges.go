package extract

import (
	"regexp"
	"sort"
	"strings"
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
		{"roles/run.", []string{"run.googleapis.com/Service"}},
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

// traceForwardingRule follows ForwardingRule -> Target*Proxy -> UrlMap -> BackendService/BackendBucket
// across suppressed LB intermediate records.
func (st *extractState) traceForwardingRule(fr *ingestedAsset) tracedForwardingRule {
	var out tracedForwardingRule
	backendPaths := make(map[string][]string)
	var orderedBackends []string

	recordBackend := func(uri string, paths []string) {
		if uri == "" {
			return
		}
		if _, exists := backendPaths[uri]; !exists {
			orderedBackends = append(orderedBackends, uri)
			backendPaths[uri] = nil
		}
		for _, p := range paths {
			if p != "" {
				list := backendPaths[uri]
				appendUnique(&list, p)
				backendPaths[uri] = list
				appendUnique(&out.paths, p)
			}
		}
	}

	visitUrlMap := func(umURI string) {
		um := st.lbAssets[umURI]
		if um == nil {
			return
		}
		for _, h := range um.urlMapHosts {
			appendUnique(&out.hosts, h)
		}
		if um.defaultService != "" {
			recordBackend(um.defaultService, nil)
		}
		for _, entry := range um.urlMapBackends {
			recordBackend(entry.backendURI, entry.paths)
		}
	}

	if fr.frBackendSvc != "" {
		recordBackend(fr.frBackendSvc, nil)
	}
	if fr.frTarget != "" {
		if lb := st.lbAssets[fr.frTarget]; lb != nil {
			if lb.isTLS {
				out.tls = true
			}
			switch lb.assetType {
			case "compute.googleapis.com/TargetHttpProxy",
				"compute.googleapis.com/TargetHttpsProxy",
				"compute.googleapis.com/TargetGrpcProxy":
				visitUrlMap(lb.urlMap)
			case "compute.googleapis.com/TargetTcpProxy",
				"compute.googleapis.com/TargetSslProxy":
				recordBackend(lb.service, nil)
			case "compute.googleapis.com/BackendService",
				"compute.googleapis.com/BackendBucket":
				recordBackend(lb.uri, nil)
			}
		}
	}

	for _, bURI := range orderedBackends {
		b := st.lbAssets[bURI]
		if b == nil {
			continue
		}
		paths := backendPaths[bURI]
		sort.Strings(paths)
		out.backends = append(out.backends, tracedBackend{
			backendURI:    bURI,
			backendType:   b.assetType,
			bucketName:    b.bucketName,
			backendGroups: b.backendGroups,
			paths:         paths,
		})
	}
	sort.Strings(out.hosts)
	sort.Strings(out.paths)
	return out
}

func (st *extractState) suppressGKENodeInfra() {
	clusterNamesByProj := make(map[string][]string)
	gkeMIGURIs := make(map[string]bool)
	for _, a := range st.assets {
		if a.assetType != "container.googleapis.com/Cluster" || a.suppressed {
			continue
		}
		cName := shortResourceName(a.uri)
		clusterNamesByProj[a.projectID] = append(clusterNamesByProj[a.projectID], cName)
		for _, ig := range st.gke.clusterIGURLs[a.uri] {
			gkeMIGURIs[normalizeMIGURI(ig, st.projIdx)] = true
		}
	}
	if len(clusterNamesByProj) == 0 {
		return
	}

	for _, a := range st.assets {
		if a.suppressed {
			continue
		}
		clusters := clusterNamesByProj[a.projectID]
		if len(clusters) == 0 {
			continue
		}
		switch a.assetType {
		case "compute.googleapis.com/InstanceGroupManager":
			if gkeMIGURIs[normalizeMIGURI(a.uri, st.projIdx)] || matchesAnyGKEClusterPrefix(shortResourceName(a.uri), clusters) {
				a.suppressed = true
				st.suppressed[a.assetType]++
				gkeMIGURIs[normalizeMIGURI(a.uri, st.projIdx)] = true
			}
		case "compute.googleapis.com/Instance":
			if isGKEWorkerVM(a, clusters, gkeMIGURIs, st.projIdx) {
				a.suppressed = true
				st.suppressed[a.assetType]++
			}
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
	canon := CanonicalizeAIP122WithService(raw, "compute.googleapis.com", projIdx)
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
		case "compute.googleapis.com/Subnetwork":
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
	var subURI string
	if !strings.Contains(sub, "/") {
		subProj := getAttrString(a.attrs, "subnet_project")
		if subProj == "" {
			subProj = a.projectID
		}
		subURI = "//compute.googleapis.com/projects/" + subProj + "/regions/" + a.location + "/subnetworks/" + sub
	} else {
		subURI = CanonicalizeAIP122WithService(sub, "compute.googleapis.com", st.projIdx)
	}
	if vpc := st.subnetToVPC[subURI]; vpc != "" {
		st.connectorToVPC[a.uri] = vpc
	}
}

func (st *extractState) indexVPCPeerings() {
	for _, a := range st.assets {
		if a.suppressed || a.assetType != "compute.googleapis.com/Network" {
			continue
		}
		for _, peer := range a.vpcPeerings {
			st.indexSingleVPCPeering(a, peer)
		}
	}
}

func (st *extractState) indexSingleVPCPeering(a *ingestedAsset, peer vpcPeeringInfo) {
	state := strings.ToUpper(strings.TrimSpace(peer.state))
	if (state != "" && state != "ACTIVE") || peer.network == "" {
		return
	}
	peerURI := normalizeComputeNetworkURI(peer.network, a.projectID, st.projIdx)
	peerProj := extractProjectID(peerURI, nil, st.projIdx)
	isPSA := IsPSATenantProject(peerProj) || strings.Contains(strings.ToLower(peer.name), "servicenetworking")
	if isPSA {
		st.recordPSATenant(a.uri, peerProj)
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
		if stubURI := st.registerOutOfScopeProject(peerProj, a.uri, "config_ref_cross_project"); stubURI != "" {
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
		if isPSAManagedService(a.assetType) {
			st.emitPSAPeeringEdge(a, vpc)
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
		if stubURI := st.registerOutOfScopeProject(vpcProj, a.uri, "config_ref_cross_project"); stubURI != "" {
			st.addEdge(a.uri, stubURI, EdgeConfigRef, "shared_vpc")
		}
	}
}

func (st *extractState) emitPSAPeeringEdge(a *ingestedAsset, vpc string) {
	detail := "servicenetworking-googleapis-com"
	if tenant := st.psaTenants[vpc]; tenant != "" {
		detail = "servicenetworking-googleapis-com (" + tenant + ")"
		if a.attrs == nil {
			a.attrs = make(map[string]any)
		}
		a.attrs["psa_tenant_project"] = tenant
	}
	if st.assetsByURI[vpc] != nil {
		st.addEdge(vpc, a.uri, EdgePSAPeering, detail)
	}
}

func (st *extractState) attachAlloyDBInstanceIPs() {
	for _, uri := range sortedMapKeys(st.absorbed) {
		ab := st.absorbed[uri]
		if ab.assetType != "alloydb.googleapis.com/Instance" || ab.privateIP == "" || ab.parentURI == "" {
			continue
		}
		cl := st.assetsByURI[ab.parentURI]
		if cl == nil {
			continue
		}
		appendUnique(&cl.privateIPs, ab.privateIP)
		if cl.attrs == nil {
			cl.attrs = make(map[string]any)
		}
		if getAttrString(cl.attrs, "private_ip") == "" {
			cl.attrs["private_ip"] = ab.privateIP
		}
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
			k := netIPKey{netURI: netURI, ip: ip}
			list := st.privateIPTargets[k]
			appendUnique(&list, a.uri)
			st.privateIPTargets[k] = list
		}
	}
}

func isPSAManagedService(assetType string) bool {
	switch assetType {
	case "sqladmin.googleapis.com/Instance",
		"alloydb.googleapis.com/Cluster",
		"redis.googleapis.com/Instance",
		"redis.googleapis.com/Cluster",
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
		out = append(out, sortedMapKeys(peers)...)
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
	sort.Strings(matches)
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
	if a.parentURI != "" {
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
	}
	return ""
}

func normalizeComputeNetworkURI(raw, projID string, projIdx map[string]string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "/") && projID != "" {
		return "//compute.googleapis.com/projects/" + mapProject(projID, projIdx) + "/global/networks/" + s
	}
	return CanonicalizeAIP122WithService(s, "compute.googleapis.com", projIdx)
}

func (st *extractState) indexCloudRunHosts() {
	registerURL := func(rawURL, uri string) {
		su := SanitizeURL(rawURL)
		if su == "" {
			return
		}
		low := strings.ToLower(su)
		if _, exists := st.runHostToURI[low]; !exists {
			st.runHostToURI[low] = uri
		}
		trimmed := strings.TrimPrefix(strings.TrimPrefix(low, "https://"), "http://")
		host, _, _ := strings.Cut(trimmed, "/")
		if host != "" {
			if _, exists := st.runHostToURI[host]; !exists {
				st.runHostToURI[host] = uri
			}
		}
	}

	for _, a := range st.assets {
		if a.suppressed {
			continue
		}
		if a.assetType != "run.googleapis.com/Service" && a.assetType != "cloudfunctions.googleapis.com/CloudFunction" {
			continue
		}
		for _, u := range a.runURLs {
			registerURL(u, a.uri)
		}
		name := strings.ToLower(shortResourceName(a.uri))
		if name != "" {
			list := st.runNameToURIs[name]
			appendUnique(&list, a.uri)
			st.runNameToURIs[name] = list
		}
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
	trimmed := strings.TrimPrefix(strings.TrimPrefix(su, "https://"), "http://")
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
		st.emitTargetEdge(frURI, bktURI, EdgeConfigRef, detail, "config_ref_cross_project")
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
		uri := "//run.googleapis.com/projects/" + neg.projectID + "/locations/" + loc + "/services/" + neg.negCloudRunSvc
		st.emitTargetEdge(frURI, uri, EdgeConfigRef, detail, "config_ref_cross_project")
	case neg.negCloudFn != "":
		uri := "//cloudfunctions.googleapis.com/projects/" + neg.projectID + "/locations/" + loc + "/functions/" + neg.negCloudFn
		st.emitTargetEdge(frURI, uri, EdgeConfigRef, detail, "config_ref_cross_project")
	case neg.negAppEngineSvc != "":
		uri := "//appengine.googleapis.com/apps/" + neg.projectID + "/services/" + neg.negAppEngineSvc
		st.emitTargetEdge(frURI, uri, EdgeConfigRef, detail, "config_ref_cross_project")
	case neg.negPSCTarget != "":
		st.emitTargetEdge(frURI, neg.negPSCTarget, EdgeConfigRef, detail, "config_ref_cross_project")
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
				st.emitTargetEdge(targetURI, a.uri, EdgeConfigRef, ref.detail, "config_ref_cross_project")
			} else {
				st.emitTargetEdge(a.uri, targetURI, EdgeConfigRef, ref.detail, "config_ref_cross_project")
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
		if uri := cloudSQLConnNameToURI(ref.targetURI, st.projIdx); uri != "" {
			return uri
		}
		if !strings.Contains(ref.targetURI, "/") {
			return "//sqladmin.googleapis.com/projects/" + src.projectID + "/instances/" + ref.targetURI
		}
		return CanonicalizeAIP122WithService(ref.targetURI, "sqladmin.googleapis.com", st.projIdx)
	case "datastore_id":
		if strings.Contains(ref.targetURI, "/") {
			return CanonicalizeAIP122WithService(ref.targetURI, "discoveryengine.googleapis.com", st.projIdx)
		}
		for _, cand := range st.assets {
			if !cand.suppressed && cand.assetType == "discoveryengine.googleapis.com/DataStore" &&
				cand.projectID == src.projectID && shortResourceName(cand.uri) == ref.targetURI {
				return cand.uri
			}
		}
		return ""
	}

	canon := CanonicalizeAIP122WithService(ref.targetURI, ref.defaultSvc, st.projIdx)
	if !strings.HasPrefix(canon, "//") {
		return ""
	}
	// Resolve VpnTunnel -> VpnGateway transparently when Router links a VpnTunnel.
	if ab := st.absorbed[canon]; ab != nil {
		if ab.assetType == "compute.googleapis.com/VpnTunnel" && ab.vpnGateway != "" {
			return ab.vpnGateway
		}
		if ab.assetType == "datastream.googleapis.com/ConnectionProfile" {
			return "" // Handled in resolveAbsorbedBridges for Datastream Stream.
		}
	}
	return canon
}

func (st *extractState) resolveAbsorbedBridges() {
	st.resolveAbsorbedResourceBridges()
	st.resolveDatastreamStreamBridges()
	st.resolveBigQueryFoldedExternalURIs()
}

func (st *extractState) resolveAbsorbedResourceBridges() {
	for _, uri := range sortedMapKeys(st.absorbed) {
		ab := st.absorbed[uri]
		switch ab.assetType {
		case "compute.googleapis.com/VpnTunnel":
			if ab.router != "" && ab.vpnGateway != "" {
				st.emitTargetEdge(ab.router, ab.vpnGateway, EdgeConfigRef, "vpn_tunnel", "config_ref_cross_project")
			}
		case "networkconnectivity.googleapis.com/Spoke":
			st.resolveNCCSpokeBridge(ab)
		case "aiplatform.googleapis.com/FeatureView":
			if ab.parentURI != "" && ab.bqSourceURI != "" {
				dsURI := normalizeBQDatasetURI(ab.bqSourceURI, ab.projectID, st.projIdx)
				st.emitTargetEdge(dsURI, ab.parentURI, EdgeConfigRef, "feature_sync", "config_ref_cross_project")
			}
		case "analyticshub.googleapis.com/Listing":
			if ab.parentURI != "" && ab.bqSourceURI != "" {
				dsURI := normalizeBQDatasetURI(ab.bqSourceURI, ab.projectID, st.projIdx)
				st.emitTargetEdge(dsURI, ab.parentURI, EdgeConfigRef, "analytics_hub_listing", "config_ref_cross_project")
			}
		case "dataplex.googleapis.com/Asset":
			if ab.parentURI != "" && ab.resourceSpecName != "" {
				tgt := CanonicalizeAIP122(ab.resourceSpecName, st.projIdx)
				st.emitTargetEdge(ab.parentURI, tgt, EdgeConfigRef, "dataplex_asset", "config_ref_cross_project")
			}
		}
	}
}

func (st *extractState) resolveNCCSpokeBridge(ab *absorbedRecord) {
	if ab.hub == "" || st.assetsByURI[ab.hub] == nil {
		return
	}
	if ab.linkedVPC != "" {
		st.emitTargetEdge(ab.hub, ab.linkedVPC, EdgeConfigRef, "ncc_spoke", "config_ref_cross_project")
	}
	for _, v := range ab.linkedVPNs {
		if tun := st.absorbed[v]; tun != nil && tun.vpnGateway != "" {
			st.emitTargetEdge(ab.hub, tun.vpnGateway, EdgeConfigRef, "ncc_spoke", "config_ref_cross_project")
		}
	}
	for _, ic := range ab.linkedInterconnects {
		st.emitTargetEdge(ab.hub, ic, EdgeConfigRef, "ncc_spoke", "config_ref_cross_project")
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
	cpURI := CanonicalizeAIP122WithService(ref.targetURI, "datastream.googleapis.com", st.projIdx)
	cp := st.absorbed[cpURI]
	if cp == nil || cp.assetType != "datastream.googleapis.com/ConnectionProfile" {
		return
	}
	if ref.direction != "in" {
		if cp.gcsBucket != "" {
			bktURI := normalizeBucketURI(cp.gcsBucket, st.projIdx)
			st.emitTargetEdge(a.uri, bktURI, EdgeConfigRef, "datastream.gcs", "config_ref_cross_project")
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
	for _, dsURI := range sortedMapKeys(st.foldedExternalURIs) {
		if st.assetsByURI[dsURI] == nil {
			continue
		}
		for _, ext := range st.foldedExternalURIs[dsURI] {
			if strings.HasPrefix(ext, "https://") || strings.HasPrefix(ext, "http://") {
				if tgt := st.resolveHTTPTarget(ext); tgt != "" {
					st.addEdge(dsURI, tgt, EdgeConfigRef, "bq_remote_fn")
				}
				continue
			}
			canon := CanonicalizeAIP122(ext, st.projIdx)
			if strings.HasPrefix(canon, "//storage.googleapis.com/") {
				st.emitTargetEdge(canon, dsURI, EdgeConfigRef, "biglake", "config_ref_cross_project")
			} else if strings.HasPrefix(canon, "//") {
				st.emitTargetEdge(dsURI, canon, EdgeConfigRef, "bq_external", "config_ref_cross_project")
			}
		}
	}
}

func (st *extractState) resolveEnvAndSecretRefs() {
	for _, a := range st.assets {
		if a.suppressed {
			continue
		}
		srcNet := st.assetNetworkURI(a)
		for _, ref := range a.envRefs {
			st.resolveSingleConfigRef(a.uri, a.projectID, srcNet, ref, EdgeConfigRef, "config_ref_cross_project")
		}
		for _, sec := range a.secretRefs {
			secURI := stripSecretVersion(CanonicalizeAIP122WithService(sec, "secretmanager.googleapis.com", st.projIdx))
			st.emitTargetEdge(a.uri, secURI, EdgeConfigRef, "secret_ref", "config_ref_cross_project")
		}
	}
}

func (st *extractState) resolveCloudBuildRefList(sourceURI, srcProj string, refs, secRefs []string) {
	// Capture any explicit --project=<id> target in this build so --cluster / run-deploy know target project.
	var targetProjects []string
	for _, r := range refs {
		if strings.HasPrefix(r, "--project=") {
			proj := strings.TrimPrefix(r, "--project=")
			appendUnique(&targetProjects, proj)
		}
	}

	for _, ref := range refs {
		switch {
		case strings.HasPrefix(ref, "--project="):
			proj := strings.TrimPrefix(ref, "--project=")
			if proj != "" && !st.inScopeProjects[proj] {
				if stubURI := st.registerOutOfScopeProject(proj, sourceURI, "cloudbuild_deploy_flag"); stubURI != "" {
					st.addEdge(sourceURI, stubURI, EdgeCloudBuildStepRef, ref)
				}
			}
		case strings.HasPrefix(ref, "gcr.io/"):
			proj := strings.TrimPrefix(ref, "gcr.io/")
			if proj != "" && !st.inScopeProjects[proj] {
				if stubURI := st.registerOutOfScopeProject(proj, sourceURI, "cloudbuild_deploy_flag"); stubURI != "" {
					st.addEdge(sourceURI, stubURI, EdgeCloudBuildStepRef, ref)
				}
			}
		case strings.HasPrefix(ref, "--cluster="):
			clusterName := strings.TrimPrefix(ref, "--cluster=")
			if tgt := st.findClusterByName(clusterName, srcProj, targetProjects); tgt != "" {
				st.addEdge(sourceURI, tgt, EdgeCloudBuildStepRef, ref)
			}
		case strings.HasPrefix(ref, "run-deploy:"):
			svcName := strings.TrimPrefix(ref, "run-deploy:")
			if tgt := st.findCloudRunByName(svcName, srcProj, targetProjects); tgt != "" {
				st.addEdge(sourceURI, tgt, EdgeCloudBuildStepRef, ref)
			}
		default:
			st.resolveSingleConfigRef(sourceURI, srcProj, "", ref, EdgeCloudBuildStepRef, "cloudbuild_deploy_flag")
		}
	}

	for _, sec := range secRefs {
		secURI := stripSecretVersion(CanonicalizeAIP122WithService(sec, "secretmanager.googleapis.com", st.projIdx))
		st.emitTargetEdge(sourceURI, secURI, EdgeCloudBuildStepRef, "secret_ref", "cloudbuild_deploy_flag")
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
	case strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://"):
		if tgt := st.resolveHTTPTarget(s); tgt != "" {
			st.addEdge(srcURI, tgt, edgeKind, s)
		}
	case strings.HasPrefix(s, "//") || strings.HasPrefix(s, "projects/"):
		st.resolveAIP122ConfigRef(srcURI, s, edgeKind, oosKind)
	case strings.HasPrefix(s, "--cluster="), strings.HasPrefix(s, "run-deploy:"), strings.HasPrefix(s, "--project="):
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
	case strings.HasPrefix(s, "--cluster="):
		if tgt := st.findClusterByName(strings.TrimPrefix(s, "--cluster="), srcProj, nil); tgt != "" {
			st.addEdge(srcURI, tgt, edgeKind, s)
		}
	case strings.HasPrefix(s, "run-deploy:"):
		if tgt := st.findCloudRunByName(strings.TrimPrefix(s, "run-deploy:"), srcProj, nil); tgt != "" {
			st.addEdge(srcURI, tgt, edgeKind, s)
		}
	case strings.HasPrefix(s, "--project="):
		proj := strings.TrimPrefix(s, "--project=")
		if proj != "" && !st.inScopeProjects[proj] {
			if stubURI := st.registerOutOfScopeProject(proj, srcURI, oosKind); stubURI != "" {
				st.addEdge(srcURI, stubURI, edgeKind, s)
			}
		}
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
		dsURI := "//bigquery.googleapis.com/projects/" + proj + "/datasets/" + ds
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
	if fromURI == "" || toURI == "" || fromURI == toURI {
		return
	}
	fromInScope := st.isInScopeEndpoint(fromURI)
	toInScope := st.isInScopeEndpoint(toURI)
	if !fromInScope && !toInScope {
		return
	}

	resolveEndpoint := func(uri string, inScope bool, peerURI string) (string, bool) {
		if inScope {
			return uri, true
		}
		if _, ext := st.externalResources[uri]; ext {
			return uri, true
		}
		proj := extractProjectID(uri, nil, st.projIdx)
		if proj != "" && !st.inScopeProjects[proj] {
			if stubURI := st.registerOutOfScopeProject(proj, peerURI, oosKind); stubURI != "" {
				return stubURI, true
			}
		}
		return "", false
	}

	resolvedFrom, okFrom := resolveEndpoint(fromURI, fromInScope, toURI)
	if !okFrom {
		return
	}
	resolvedTo, okTo := resolveEndpoint(toURI, toInScope, fromURI)
	if !okTo || resolvedFrom == resolvedTo {
		return
	}
	if fromInScope {
		st.referencedURIs[resolvedFrom] = true
	}
	if toInScope {
		st.referencedURIs[resolvedTo] = true
	}
	st.addEdge(resolvedFrom, resolvedTo, kind, detail)
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
		isAncestor := b.assetType == "cloudresourcemanager.googleapis.com/Organization" ||
			b.assetType == "cloudresourcemanager.googleapis.com/Folder" ||
			b.assetType == "cloudresourcemanager.googleapis.com/Project"
		for _, member := range b.members {
			st.resolveSingleIAMBindingMember(b, member, isAncestor, byProjType)
		}
	}
}

func (st *extractState) resolveSingleIAMBindingMember(b *iamBindingRecord, member string, isAncestor bool, byProjType map[projAssetTypeKey][]*ingestedAsset) {
	email, ok := parseServiceAccountMember(member)
	if !ok {
		return
	}
	if isAncestor && broadAncestorRoles[b.role] {
		if sa := st.saByEmail[email]; sa != nil {
			appendUnique(&sa.capabilities, b.role)
		}
		return
	}
	if b.role == "roles/iam.workloadIdentityUser" {
		return
	}

	targets := st.resolveIAMTargets(b, isAncestor, email, byProjType)
	if len(targets) == 0 {
		return
	}
	st.emitIAMBindingEdges(b, email, targets)
}

func (st *extractState) resolveIAMTargets(b *iamBindingRecord, isAncestor bool, email string, byProjType map[projAssetTypeKey][]*ingestedAsset) []*ingestedAsset {
	if !isAncestor {
		if target := st.assetsByURI[b.resourceURI]; target != nil && !target.suppressed {
			return []*ingestedAsset{target}
		}
		return nil
	}
	if b.assetType != "cloudresourcemanager.googleapis.com/Project" {
		return nil
	}
	var targets []*ingestedAsset
	if b.conditionExpr != "" {
		for _, tgtURI := range extractConditionPinnedURIs(b.conditionExpr, st.projIdx) {
			if target := st.assetsByURI[tgtURI]; target != nil && !target.suppressed {
				targets = append(targets, target)
			}
		}
	}
	if len(targets) > 0 || isDefaultServiceAccount(email) {
		return targets
	}
	targetTypes := matchServiceRoleTargetTypes(b.role)
	if len(targetTypes) == 0 {
		return nil
	}
	var candidates []*ingestedAsset
	for _, tt := range targetTypes {
		candidates = append(candidates, byProjType[projAssetTypeKey{project: b.projectID, assetType: tt}]...)
	}
	if len(candidates) == 1 {
		return candidates
	}
	return nil
}

func (st *extractState) emitIAMBindingEdges(b *iamBindingRecord, email string, targets []*ingestedAsset) {
	saURI := st.serviceAccountURI(email, b.projectID)
	saProj := extractProjectID(saURI, nil, st.projIdx)
	if st.assetsByURI[saURI] == nil && isDefaultServiceAccount(email) {
		return
	}
	if saProj != "" && !st.inScopeProjects[saProj] {
		for _, target := range targets {
			srcURI := st.registerOutOfScopeProject(saProj, target.uri, "iam_cross_project")
			if srcURI != "" && srcURI != target.uri {
				st.addEdge(srcURI, target.uri, EdgeIAMResourceBinding, b.role)
			}
		}
		return
	}
	if st.assetsByURI[saURI] == nil {
		st.ensureInScopeServiceAccount(saURI, saProj, email)
	}
	for _, target := range targets {
		if saURI != target.uri {
			st.referencedURIs[saURI] = true
			st.addEdge(saURI, target.uri, EdgeIAMResourceBinding, b.role)
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
	sort.Strings(out)
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
			AssetType:   "cloudresourcemanager.googleapis.com/Project",
			ProjectID:   proj,
			DisplayName: proj,
			IsExternal:  true,
		}
	}
	if sourceKind == "" {
		sourceKind = "config_ref_cross_project"
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
		case strings.HasPrefix(local, "service-") && isNumeric(strings.TrimPrefix(local, "service-")):
			saProj = mapProject(strings.TrimPrefix(local, "service-"), st.projIdx)
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
		assetType:   "iam.googleapis.com/ServiceAccount",
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

func (st *extractState) findClusterByName(name, srcProj string, targetProjs []string) string {
	allowedProjs := make(map[string]bool)
	for _, p := range targetProjs {
		allowedProjs[p] = true
	}
	if len(allowedProjs) == 0 && srcProj != "" {
		allowedProjs[srcProj] = true
	}
	var matches []string
	for _, a := range st.assets {
		if a.suppressed || a.assetType != "container.googleapis.com/Cluster" {
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

func (st *extractState) findCloudRunByName(name, srcProj string, targetProjs []string) string {
	allowedProjs := make(map[string]bool)
	for _, p := range targetProjs {
		allowedProjs[p] = true
	}
	if len(allowedProjs) == 0 && srcProj != "" {
		allowedProjs[srcProj] = true
	}
	var matches []string
	for _, a := range st.assets {
		if a.suppressed || a.assetType != "run.googleapis.com/Service" {
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
		return CanonicalizeAIP122WithService(s, "storage.googleapis.com", projIdx)
	}
	return "//storage.googleapis.com/" + s
}

func normalizeBQDatasetURI(raw, defaultProj string, projIdx map[string]string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "projects/") || strings.HasPrefix(s, "http") {
		canon := CanonicalizeAIP122WithService(s, "bigquery.googleapis.com", projIdx)
		if idx := strings.Index(canon, "/tables/"); idx > 0 {
			return canon[:idx]
		}
		return canon
	}
	if proj, ds, ok := parseProjectDatasetRef(s, projIdx); ok {
		return "//bigquery.googleapis.com/projects/" + proj + "/datasets/" + ds
	}
	if !strings.Contains(s, "/") && defaultProj != "" {
		return "//bigquery.googleapis.com/projects/" + mapProject(defaultProj, projIdx) + "/datasets/" + s
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
