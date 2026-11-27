package extract

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

type k8sObjKey struct {
	clusterURI string
	namespace  string
	name       string
}

type k8sNamespaceKey struct {
	clusterURI string
	namespace  string
}

type k8sWorkloadMeta struct {
	uri                string
	clusterURI         string
	namespace          string
	podLabels          map[string]string
	serviceAccountName string
	configMapNames     []string
}

type k8sServiceMeta struct {
	uri        string
	clusterURI string
	namespace  string
	name       string
	projectID  string
	selector   map[string]string
	negPorts   map[string]string // negName -> port string
	lbIPs      []string
}

type k8sIngressBackend struct {
	serviceName string
	host        string
	path        string
}

type k8sIngressMeta struct {
	uri                 string
	clusterURI          string
	namespace           string
	projectID           string
	hosts               []string
	paths               []string
	backends            []k8sIngressBackend
	forwardingRuleNames []string
}

type gkeState struct {
	serviceAccounts  map[k8sObjKey]string
	configMapEnvRefs map[k8sObjKey][]string
	configMapSecRefs map[k8sObjKey][]string
	workloads        map[string]*k8sWorkloadMeta
	services         map[string]*k8sServiceMeta
	ingresses        map[string]*k8sIngressMeta
	clusterIGURLs    map[string][]string
}

func newGKEState() *gkeState {
	return &gkeState{
		serviceAccounts:  make(map[k8sObjKey]string),
		configMapEnvRefs: make(map[k8sObjKey][]string),
		configMapSecRefs: make(map[k8sObjKey][]string),
		workloads:        make(map[string]*k8sWorkloadMeta),
		services:         make(map[string]*k8sServiceMeta),
		ingresses:        make(map[string]*k8sIngressMeta),
		clusterIGURLs:    make(map[string][]string),
	}
}

func isSystemK8sNamespace(ns string) bool {
	s := strings.ToLower(strings.TrimSpace(ns))
	return s == "kube-system" || s == "gmp-system" || strings.HasPrefix(s, "gke-managed-")
}

func isRetainedK8sAssetType(assetType string) bool {
	switch assetType {
	case "apps.k8s.io/Deployment",
		"apps.k8s.io/StatefulSet",
		"apps.k8s.io/DaemonSet",
		"batch.k8s.io/CronJob",
		"batch.k8s.io/Job",
		"k8s.io/Service",
		"networking.k8s.io/Ingress":
		return true
	}
	return false
}

func extractK8sNamespace(canonURI string, data *structpb.Struct) string {
	if ns := strings.TrimSpace(getStringField(getStructField(data, "metadata"), "namespace")); ns != "" {
		return ns
	}
	return extractSegmentAfter(canonURI, "namespaces")
}

func extractK8sName(canonURI string, data *structpb.Struct) string {
	if name := strings.TrimSpace(getStringField(getStructField(data, "metadata"), "name")); name != "" {
		return name
	}
	return shortResourceName(canonURI)
}

func (g *gkeState) indexK8sServiceAccount(clusterURI, ns, name string, data *structpb.Struct) {
	if clusterURI == "" || ns == "" || name == "" || isSystemK8sNamespace(ns) {
		return
	}
	ann := getStructField(getStructField(data, "metadata"), "annotations")
	gsa := strings.TrimSpace(getStringField(ann, "iam.gke.io/gcp-service-account"))
	if gsa != "" {
		g.serviceAccounts[k8sObjKey{clusterURI: clusterURI, namespace: ns, name: name}] = gsa
	}
}

func (g *gkeState) indexK8sConfigMap(clusterURI, ns, name string, data *structpb.Struct, projIdx map[string]string) {
	if clusterURI == "" || ns == "" || name == "" || isSystemK8sNamespace(ns) || data == nil {
		return
	}
	var envRefs, secRefs []string
	extractEnvMapRefs(getStructField(data, "data"), projIdx, &envRefs, &secRefs)
	if len(envRefs) == 0 && len(secRefs) == 0 {
		return
	}
	key := k8sObjKey{clusterURI: clusterURI, namespace: ns, name: name}
	for _, r := range envRefs {
		appendUniqueMap(g.configMapEnvRefs, key, r)
	}
	for _, r := range secRefs {
		appendUniqueMap(g.configMapSecRefs, key, r)
	}
}

func (g *gkeState) indexK8sWorkload(uri, assetType, clusterURI, ns, projID string, data *structpb.Struct, projIdx map[string]string) (envRefs, secRefs []string) {
	spec := getStructField(data, "spec")
	var podTemplate *structpb.Struct
	if assetType == "batch.k8s.io/CronJob" {
		podTemplate = getStructField(getStructField(getStructField(spec, "jobTemplate"), "spec"), "template")
	} else {
		podTemplate = getStructField(spec, "template")
	}
	podMeta := getStructField(podTemplate, "metadata")
	podSpec := getStructField(podTemplate, "spec")

	var csql, cmNames []string
	extractContainerAndInitRefs(podSpec, projID, projIdx, &envRefs, &secRefs, &csql, &cmNames)
	for _, u := range csql {
		appendUnique(&envRefs, u)
	}

	g.workloads[uri] = &k8sWorkloadMeta{
		uri:                uri,
		clusterURI:         clusterURI,
		namespace:          ns,
		podLabels:          extractStringMap(getStructField(podMeta, "labels")),
		serviceAccountName: strings.TrimSpace(getStringField(podSpec, "serviceAccountName")),
		configMapNames:     cmNames,
	}
	return envRefs, secRefs
}

func (g *gkeState) indexK8sService(uri, clusterURI, ns, name, projID string, data *structpb.Struct) {
	meta := getStructField(data, "metadata")
	spec := getStructField(data, "spec")
	status := getStructField(data, "status")

	negPorts := parseNEGStatusAnnotation(getStringField(getStructField(meta, "annotations"), "cloud.google.com/neg-status"))
	var lbIPs []string
	if lb := getStructField(status, "loadBalancer"); lb != nil {
		for _, ingVal := range getListField(lb, "ingress") {
			if ing := ingVal.GetStructValue(); ing != nil {
				if ip := strings.TrimSpace(getStringField(ing, "ip")); ip != "" {
					appendUnique(&lbIPs, ip)
				}
			}
		}
	}
	slices.Sort(lbIPs)

	g.services[uri] = &k8sServiceMeta{
		uri:        uri,
		clusterURI: clusterURI,
		namespace:  ns,
		name:       name,
		projectID:  projID,
		selector:   extractStringMap(getStructField(spec, "selector")),
		negPorts:   negPorts,
		lbIPs:      lbIPs,
	}
}

func (g *gkeState) indexK8sIngress(uri, clusterURI, ns, projID string, data *structpb.Struct) {
	meta := getStructField(data, "metadata")
	ann := getStructField(meta, "annotations")
	spec := getStructField(data, "spec")

	im := &k8sIngressMeta{
		uri:        uri,
		clusterURI: clusterURI,
		namespace:  ns,
		projectID:  projID,
	}
	for _, key := range []string{
		"ingress.kubernetes.io/forwarding-rule",
		"ingress.kubernetes.io/https-forwarding-rule",
	} {
		appendUnique(&im.forwardingRuleNames, strings.TrimSpace(getStringField(ann, key)))
	}

	for _, tlsVal := range getListField(spec, "tls") {
		for _, h := range getStringListField(tlsVal.GetStructValue(), "hosts") {
			appendUnique(&im.hosts, h)
		}
	}

	im.collectRules(spec)

	// Default backend (v1 defaultBackend or v1beta1 backend).
	for _, key := range []string{"defaultBackend", "backend"} {
		if svcName := extractIngressBackendServiceName(getStructField(spec, key)); svcName != "" {
			im.backends = append(im.backends, k8sIngressBackend{serviceName: svcName, path: "/*"})
		}
	}

	slices.Sort(im.hosts)
	slices.Sort(im.paths)
	slices.Sort(im.forwardingRuleNames)
	g.ingresses[uri] = im
}

func (im *k8sIngressMeta) collectRules(spec *structpb.Struct) {
	for _, ruleVal := range getListField(spec, "rules") {
		rule := ruleVal.GetStructValue()
		if rule == nil {
			continue
		}
		host := strings.TrimSpace(getStringField(rule, "host"))
		appendUnique(&im.hosts, host)
		im.collectRulePaths(getStructField(rule, "http"), host)
	}
}

func (im *k8sIngressMeta) collectRulePaths(httpBlock *structpb.Struct, host string) {
	for _, pVal := range getListField(httpBlock, "paths") {
		p := pVal.GetStructValue()
		if p == nil {
			continue
		}
		pathStr := strings.TrimSpace(getStringField(p, "path"))
		appendUnique(&im.paths, pathStr)
		if svcName := extractIngressBackendServiceName(getStructField(p, "backend")); svcName != "" {
			im.backends = append(im.backends, k8sIngressBackend{
				serviceName: svcName,
				host:        host,
				path:        pathStr,
			})
		}
	}
}

func extractIngressBackendServiceName(backend *structpb.Struct) string {
	if backend == nil {
		return ""
	}
	if svc := getStructField(backend, "service"); svc != nil {
		if name := strings.TrimSpace(getStringField(svc, "name")); name != "" {
			return name
		}
	}
	return strings.TrimSpace(getStringField(backend, "serviceName"))
}

func parseNEGStatusAnnotation(raw string) map[string]string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	var payload struct {
		NetworkEndpointGroups map[string]string `json:"network_endpoint_groups"`
	}
	if err := json.Unmarshal([]byte(s), &payload); err != nil || len(payload.NetworkEndpointGroups) == 0 {
		return nil
	}
	out := make(map[string]string, len(payload.NetworkEndpointGroups))
	for port, negName := range payload.NetworkEndpointGroups {
		if negName != "" {
			out[negName] = port
		}
	}
	return out
}

func (g *gkeState) rekey(projIdx map[string]string) {
	rekeyKSA := make(map[k8sObjKey]string, len(g.serviceAccounts))
	for k, v := range g.serviceAccounts {
		k.clusterURI = CanonicalizeAIP122(k.clusterURI, projIdx)
		rekeyKSA[k] = v
	}
	g.serviceAccounts = rekeyKSA

	rekeyCMEnv := make(map[k8sObjKey][]string, len(g.configMapEnvRefs))
	for k, refs := range g.configMapEnvRefs {
		k.clusterURI = CanonicalizeAIP122(k.clusterURI, projIdx)
		rekeyCMEnv[k] = canonicalizeSlice(refs, projIdx)
	}
	g.configMapEnvRefs = rekeyCMEnv

	rekeyCMSec := make(map[k8sObjKey][]string, len(g.configMapSecRefs))
	for k, refs := range g.configMapSecRefs {
		k.clusterURI = CanonicalizeAIP122(k.clusterURI, projIdx)
		rekeyCMSec[k] = canonicalizeSlice(refs, projIdx)
	}
	g.configMapSecRefs = rekeyCMSec

	rekeyWL := make(map[string]*k8sWorkloadMeta, len(g.workloads))
	for _, w := range g.workloads {
		w.uri = CanonicalizeAIP122(w.uri, projIdx)
		w.clusterURI = CanonicalizeAIP122(w.clusterURI, projIdx)
		rekeyWL[w.uri] = w
	}
	g.workloads = rekeyWL

	rekeySvc := make(map[string]*k8sServiceMeta, len(g.services))
	for _, s := range g.services {
		s.uri = CanonicalizeAIP122(s.uri, projIdx)
		s.clusterURI = CanonicalizeAIP122(s.clusterURI, projIdx)
		s.projectID = mapProject(s.projectID, projIdx)
		rekeySvc[s.uri] = s
	}
	g.services = rekeySvc

	rekeyIng := make(map[string]*k8sIngressMeta, len(g.ingresses))
	for _, im := range g.ingresses {
		im.uri = CanonicalizeAIP122(im.uri, projIdx)
		im.clusterURI = CanonicalizeAIP122(im.clusterURI, projIdx)
		im.projectID = mapProject(im.projectID, projIdx)
		rekeyIng[im.uri] = im
	}
	g.ingresses = rekeyIng

	rekeyIG := make(map[string][]string, len(g.clusterIGURLs))
	for cURI, urls := range g.clusterIGURLs {
		canonC := CanonicalizeAIP122(cURI, projIdx)
		for _, u := range urls {
			appendUniqueMap(rekeyIG, canonC, CanonicalizeAIP122WithService(u, svcCompute, projIdx))
		}
	}
	g.clusterIGURLs = rekeyIG
}

type gkeNEGTarget struct {
	svcURI string
	port   string
}

func (st *extractState) resolveGKE() {
	wlURIs := slices.Sorted(maps.Keys(st.gke.workloads))
	st.resolveGKEWorkloads(wlURIs)
	svcByObjKey, negTargetsByProj, lbIPToSvcs := st.resolveGKEServices(wlURIs)
	ingressesBySvc, ingressesByFRName := st.resolveGKEIngresses(svcByObjKey)
	st.resolveGKEForwardingRules(negTargetsByProj, lbIPToSvcs, ingressesBySvc, ingressesByFRName)
}

func (st *extractState) resolveGKEWorkloads(wlURIs []string) {
	g := st.gke
	for _, uri := range wlURIs {
		w := g.workloads[uri]
		asset := st.assetsByURI[uri]
		if asset == nil {
			continue
		}
		for _, cmName := range w.configMapNames {
			key := k8sObjKey{clusterURI: w.clusterURI, namespace: w.namespace, name: cmName}
			for _, r := range g.configMapEnvRefs[key] {
				appendUnique(&asset.envRefs, r)
			}
			for _, r := range g.configMapSecRefs[key] {
				appendUnique(&asset.secretRefs, r)
			}
		}
		st.resolveGKEWorkloadIdentity(uri, w, asset)
	}
}

func (st *extractState) resolveGKEWorkloadIdentity(uri string, w *k8sWorkloadMeta, asset *ingestedAsset) {
	ksaName := w.serviceAccountName
	if ksaName == "" {
		ksaName = "default"
	}
	gsaEmail := st.gke.serviceAccounts[k8sObjKey{clusterURI: w.clusterURI, namespace: w.namespace, name: ksaName}]
	if gsaEmail == "" {
		return
	}
	gsaURI := st.serviceAccountURI(gsaEmail, asset.projectID)
	asset.setAttr("service_account", gsaEmail)
	st.referencedURIs[gsaURI] = true

	if st.assetsByURI[gsaURI] != nil {
		st.addEdge(uri, gsaURI, EdgeWorkloadIdentity, "ksa:"+w.namespace+"/"+ksaName)
		return
	}
	saProj := extractProjectID(gsaURI, nil, st.projIdx)
	if saProj != "" && !st.inScopeProjects[saProj] {
		if stubURI := st.registerOutOfScopeProject(saProj, uri, "iam_cross_project"); stubURI != "" {
			st.addEdge(uri, stubURI, EdgeWorkloadIdentity, "ksa:"+w.namespace+"/"+ksaName+"->"+gsaEmail)
		}
		return
	}
	st.ensureInScopeServiceAccount(gsaURI, saProj, gsaEmail)
	st.addEdge(uri, gsaURI, EdgeWorkloadIdentity, "ksa:"+w.namespace+"/"+ksaName)
}

func (st *extractState) resolveGKEServices(wlURIs []string) (
	map[k8sObjKey]*k8sServiceMeta,
	map[string][]gkeNEGTarget,
	map[string][]string,
) {
	g := st.gke
	wlByNS := make(map[k8sNamespaceKey][]*k8sWorkloadMeta)
	for _, wURI := range wlURIs {
		w := g.workloads[wURI]
		k := k8sNamespaceKey{clusterURI: w.clusterURI, namespace: w.namespace}
		wlByNS[k] = append(wlByNS[k], w)
	}

	svcByObjKey := make(map[k8sObjKey]*k8sServiceMeta, len(g.services))
	negTargetsByProj := make(map[string][]gkeNEGTarget)
	lbIPToSvcs := make(map[string][]string)

	for _, uri := range slices.Sorted(maps.Keys(g.services)) {
		svc := g.services[uri]
		svcByObjKey[k8sObjKey{clusterURI: svc.clusterURI, namespace: svc.namespace, name: svc.name}] = svc

		if len(svc.lbIPs) > 0 {
			st.recordServiceLBIPs(uri, svc, lbIPToSvcs)
		}
		for negName, port := range svc.negPorts {
			k := svc.projectID + "|" + negName
			negTargetsByProj[k] = append(negTargetsByProj[k], gkeNEGTarget{svcURI: uri, port: port})
		}
		st.matchServiceSelectorWorkloads(uri, svc, wlByNS[k8sNamespaceKey{clusterURI: svc.clusterURI, namespace: svc.namespace}])
	}
	return svcByObjKey, negTargetsByProj, lbIPToSvcs
}

func (st *extractState) recordServiceLBIPs(uri string, svc *k8sServiceMeta, lbIPToSvcs map[string][]string) {
	if a := st.assetsByURI[uri]; a != nil {
		a.setAttr("load_balancer_ips", svc.lbIPs)
	}
	for _, ip := range svc.lbIPs {
		appendUniqueMap(lbIPToSvcs, svc.projectID+"|"+ip, uri)
	}
}

func (st *extractState) matchServiceSelectorWorkloads(uri string, svc *k8sServiceMeta, workloads []*k8sWorkloadMeta) {
	// GUARD: require non-empty selector so headless/ExternalName Services never match every workload!
	if len(svc.selector) == 0 || len(workloads) == 0 {
		return
	}
	detail := formatSelector(svc.selector)
	for _, w := range workloads {
		if labelsMatchSelector(svc.selector, w.podLabels) {
			st.addEdge(uri, w.uri, EdgeK8sServiceSelector, detail)
		}
	}
}

func (st *extractState) resolveGKEIngresses(svcByObjKey map[k8sObjKey]*k8sServiceMeta) (
	map[string][]*k8sIngressMeta,
	map[string][]*k8sIngressMeta,
) {
	ingressesBySvc := make(map[string][]*k8sIngressMeta)
	ingressesByFRName := make(map[string][]*k8sIngressMeta)
	for _, ingURI := range slices.Sorted(maps.Keys(st.gke.ingresses)) {
		im := st.gke.ingresses[ingURI]
		attachIngressAttrs(st.assetsByURI[ingURI], im.hosts, im.paths)
		for _, frName := range im.forwardingRuleNames {
			k := im.projectID + "|" + frName
			ingressesByFRName[k] = append(ingressesByFRName[k], im)
		}
		for _, be := range im.backends {
			svc := svcByObjKey[k8sObjKey{clusterURI: im.clusterURI, namespace: im.namespace, name: be.serviceName}]
			if svc == nil {
				continue
			}
			detail := strings.TrimSpace(be.host + be.path)
			if detail == "" {
				detail = "/*"
			}
			st.addEdge(ingURI, svc.uri, EdgeK8sIngressBackend, detail)
			ingressesBySvc[svc.uri] = append(ingressesBySvc[svc.uri], im)
		}
	}
	return ingressesBySvc, ingressesByFRName
}

func attachIngressAttrs(a *ingestedAsset, hosts, paths []string) {
	if a == nil || (len(hosts) == 0 && len(paths) == 0) {
		return
	}
	if len(hosts) > 0 {
		a.setAttr("ingress_hosts", hosts)
	}
	if len(paths) > 0 {
		a.setAttr("ingress_paths", paths)
	}
}

func (st *extractState) resolveGKEForwardingRules(
	negTargetsByProj map[string][]gkeNEGTarget,
	lbIPToSvcs map[string][]string,
	ingressesBySvc, ingressesByFRName map[string][]*k8sIngressMeta,
) {
	for _, a := range st.assets {
		if a.assetType != "compute.googleapis.com/ForwardingRule" && a.assetType != "compute.googleapis.com/GlobalForwardingRule" {
			continue
		}
		st.resolveSingleForwardingRule(a, negTargetsByProj, lbIPToSvcs, ingressesBySvc, ingressesByFRName)
	}
}

func (st *extractState) resolveSingleForwardingRule(
	a *ingestedAsset,
	negTargetsByProj map[string][]gkeNEGTarget,
	lbIPToSvcs map[string][]string,
	ingressesBySvc, ingressesByFRName map[string][]*k8sIngressMeta,
) {
	tr := st.traceForwardingRule(a)
	if tr.tls {
		a.setAttr("tls", true)
	}

	var hosts, paths []string
	for _, h := range tr.hosts {
		appendUnique(&hosts, h)
	}
	for _, p := range tr.paths {
		appendUnique(&paths, p)
	}

	linkedSvcs := st.linkForwardingRuleNEGs(a, tr.backends, negTargetsByProj)
	st.linkForwardingRuleL4IP(a, lbIPToSvcs, linkedSvcs)
	collectForwardingRuleIngresses(a, ingressesByFRName, ingressesBySvc, linkedSvcs, &hosts, &paths)

	slices.Sort(hosts)
	slices.Sort(paths)
	attachIngressAttrs(a, hosts, paths)
}

func (st *extractState) linkForwardingRuleNEGs(a *ingestedAsset, backends []tracedBackend, negTargetsByProj map[string][]gkeNEGTarget) map[string]bool {
	linkedSvcs := make(map[string]bool)
	for _, be := range backends {
		for _, grpURI := range be.backendGroups {
			st.linkForwardingRuleNEGGroup(a, grpURI, be.paths, negTargetsByProj, linkedSvcs)
		}
	}
	return linkedSvcs
}

func (st *extractState) linkForwardingRuleNEGGroup(
	a *ingestedAsset,
	grpURI string,
	paths []string,
	negTargetsByProj map[string][]gkeNEGTarget,
	linkedSvcs map[string]bool,
) {
	negProj := extractProjectID(grpURI, nil, st.projIdx)
	if negProj == "" {
		negProj = a.projectID
	}
	matches := negTargetsByProj[negProj+"|"+shortResourceName(grpURI)]
	if len(matches) == 0 {
		return
	}
	st.gkeResolvedNEGs[a.uri+"|"+grpURI] = true
	slices.SortFunc(matches, func(a, b gkeNEGTarget) int {
		return cmp.Or(cmp.Compare(a.svcURI, b.svcURI), cmp.Compare(a.port, b.port))
	})
	for _, m := range matches {
		linkedSvcs[m.svcURI] = true
		st.addEdge(a.uri, m.svcURI, EdgeK8sNEGBackend, formatNEGDetail(paths, m.port))
	}
}

func (st *extractState) linkForwardingRuleL4IP(a *ingestedAsset, lbIPToSvcs map[string][]string, linkedSvcs map[string]bool) {
	ip := getAttrString(a.attrs, "ip_address")
	if ip == "" {
		return
	}
	for _, svcURI := range lbIPToSvcs[a.projectID+"|"+ip] {
		if !linkedSvcs[svcURI] {
			linkedSvcs[svcURI] = true
			st.addEdge(a.uri, svcURI, EdgeK8sIngressBackend, "L4:"+ip)
		}
	}
}

func collectForwardingRuleIngresses(
	a *ingestedAsset,
	ingressesByFRName, ingressesBySvc map[string][]*k8sIngressMeta,
	linkedSvcs map[string]bool,
	hosts, paths *[]string,
) {
	collect := func(im *k8sIngressMeta) {
		for _, h := range im.hosts {
			appendUnique(hosts, h)
		}
		for _, p := range im.paths {
			appendUnique(paths, p)
		}
	}
	for _, im := range ingressesByFRName[a.projectID+"|"+shortResourceName(a.uri)] {
		collect(im)
	}
	for _, svcURI := range slices.Sorted(maps.Keys(linkedSvcs)) {
		for _, im := range ingressesBySvc[svcURI] {
			collect(im)
		}
	}
}

func labelsMatchSelector(selector, labels map[string]string) bool {
	if len(selector) == 0 || len(labels) == 0 {
		return false
	}
	for k, want := range selector {
		if got, ok := labels[k]; !ok || got != want {
			return false
		}
	}
	return true
}

func formatSelector(selector map[string]string) string {
	keys := slices.Sorted(maps.Keys(selector))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+selector[k])
	}
	return strings.Join(parts, ",")
}

func formatNEGDetail(paths []string, port string) string {
	var parts []string
	if len(paths) > 0 {
		parts = append(parts, strings.Join(paths, ","))
	}
	if port != "" {
		parts = append(parts, "port:"+port)
	}
	if len(parts) == 0 {
		return "neg"
	}
	return strings.Join(parts, " ")
}
