package synth

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

type containerBuilder struct {
	resByURI             map[string]extract.FactResource
	containers           []*manifest.Container
	byID                 map[string]*manifest.Container
	containerByBackedURI map[string]string
	projectByID          map[string]extract.ProjectNode
	vpcURIByProj         map[string]string
	subnetByURI          map[string]extract.FactResource
	clusterByURI         map[string]extract.FactResource
	connectorByURI       map[string]extract.FactResource
	subExposureByURI     map[string]string
}

func newContainerBuilder(facts *extract.FactsFile, resByURI map[string]extract.FactResource) *containerBuilder {
	cb := &containerBuilder{
		resByURI:             resByURI,
		byID:                 make(map[string]*manifest.Container),
		containerByBackedURI: make(map[string]string),
		projectByID:          make(map[string]extract.ProjectNode),
		vpcURIByProj:         make(map[string]string),
		subnetByURI:          make(map[string]extract.FactResource),
		clusterByURI:         make(map[string]extract.FactResource),
		connectorByURI:       make(map[string]extract.FactResource),
		subExposureByURI:     make(map[string]string),
	}

	serviceProjectsByHost := cb.indexHierarchy(facts.Hierarchy)
	vpcs, subnets, clusters := cb.indexStructuralAssets(resByURI)

	cb.addHierarchyContainers(facts.Hierarchy, serviceProjectsByHost)
	for _, r := range vpcs {
		cb.ensureVPC(r, serviceProjectsByHost[r.ProjectID])
	}
	for _, r := range subnets {
		cb.ensureSubnet(r)
	}
	for _, r := range clusters {
		cb.ensureCluster(r)
	}

	return cb
}

func (cb *containerBuilder) indexHierarchy(h extract.Hierarchy) map[string][]string {
	serviceProjectsByHost := make(map[string][]string)
	for _, p := range h.Projects {
		cb.projectByID[p.ProjectID] = p
		if p.SharedVPCHostProject != "" {
			serviceProjectsByHost[p.SharedVPCHostProject] = append(serviceProjectsByHost[p.SharedVPCHostProject], p.ProjectID)
		}
	}
	for host := range serviceProjectsByHost {
		slices.Sort(serviceProjectsByHost[host])
	}
	return serviceProjectsByHost
}

func (cb *containerBuilder) indexStructuralAssets(resByURI map[string]extract.FactResource) (vpcs, subnets, clusters []extract.FactResource) {
	for _, r := range resByURI {
		if r.IsExternal {
			continue
		}
		switch r.AssetType {
		case "compute.googleapis.com/Network":
			vpcs = append(vpcs, r)
			if cb.vpcURIByProj[r.ProjectID] == "" || r.URI < cb.vpcURIByProj[r.ProjectID] {
				cb.vpcURIByProj[r.ProjectID] = r.URI
			}
		case "compute.googleapis.com/Subnetwork":
			subnets = append(subnets, r)
			cb.subnetByURI[r.URI] = r
		case "container.googleapis.com/Cluster":
			clusters = append(clusters, r)
			cb.clusterByURI[r.URI] = r
		case "vpcaccess.googleapis.com/Connector":
			cb.connectorByURI[r.URI] = r
		}
	}
	byURI := func(a, b extract.FactResource) int { return cmp.Compare(a.URI, b.URI) }
	slices.SortFunc(vpcs, byURI)
	slices.SortFunc(subnets, byURI)
	slices.SortFunc(clusters, byURI)
	return vpcs, subnets, clusters
}

func (cb *containerBuilder) addHierarchyContainers(h extract.Hierarchy, serviceProjectsByHost map[string][]string) {
	for _, o := range h.Organizations {
		cb.addContainer(manifest.Container{
			ID:        "org_" + sanitizeID(o.OrgID),
			Kind:      manifest.ContainerOrg,
			Title:     "Org: " + cmp.Or(o.DisplayName, o.OrgID),
			Subtitle:  "organizations/" + o.OrgID,
			OrderHint: 10,
		})
	}
	for _, f := range h.Folders {
		cb.addContainer(manifest.Container{
			ID:                "folder_" + sanitizeID(f.FolderID),
			Kind:              manifest.ContainerFolder,
			Title:             "Folder: " + cmp.Or(f.DisplayName, f.FolderID),
			Subtitle:          "folders/" + f.FolderID,
			ParentContainerID: cb.resolveHierarchyParent(f.Parent),
			OrderHint:         20,
		})
	}
	for _, p := range h.Projects {
		if p.IsPSATenant {
			continue
		}
		title := p.ProjectID
		if p.DisplayName != "" && p.DisplayName != p.ProjectID {
			title = fmt.Sprintf("%s (%s)", p.DisplayName, p.ProjectID)
		}
		var badges []string
		if len(serviceProjectsByHost[p.ProjectID]) > 0 {
			badges = append(badges, "Shared VPC Host")
		} else if p.SharedVPCHostProject != "" {
			badges = append(badges, "Shared VPC Service Project")
		}
		cb.addContainer(manifest.Container{
			ID:                "proj_" + sanitizeID(p.ProjectID),
			Kind:              manifest.ContainerProject,
			Title:             title,
			ParentContainerID: cb.resolveHierarchyParent(p.Parent),
			Badges:            badges,
			OrderHint:         30,
		})
	}
}

func (cb *containerBuilder) addContainer(c manifest.Container) *manifest.Container {
	if existing, ok := cb.byID[c.ID]; ok {
		return existing
	}
	ptr := &c
	cb.containers = append(cb.containers, ptr)
	cb.byID[c.ID] = ptr
	for _, br := range c.BackedBy {
		if br.URI != "" {
			cb.containerByBackedURI[br.URI] = c.ID
		}
	}
	return ptr
}

func (cb *containerBuilder) resolveHierarchyParent(raw string) string {
	if strings.HasPrefix(raw, "organizations/") {
		id := "org_" + sanitizeID(strings.TrimPrefix(raw, "organizations/"))
		if _, ok := cb.byID[id]; ok {
			return id
		}
	}
	if strings.HasPrefix(raw, "folders/") {
		id := "folder_" + sanitizeID(strings.TrimPrefix(raw, "folders/"))
		if _, ok := cb.byID[id]; ok {
			return id
		}
	}
	return ""
}

func (cb *containerBuilder) serviceProjectID(projID string) string {
	if p, ok := cb.projectByID[projID]; ok && p.SharedVPCHostProject != "" {
		return projID
	}
	return ""
}

func (cb *containerBuilder) ensureProject(projectID string) string {
	pid := "proj_" + sanitizeID(projectID)
	if _, ok := cb.byID[pid]; ok {
		return pid
	}
	cb.addContainer(manifest.Container{
		ID:        pid,
		Kind:      manifest.ContainerProject,
		Title:     "Project: " + projectID,
		OrderHint: 30,
	})
	return pid
}

func (cb *containerBuilder) ensureVPC(r extract.FactResource, svcProjects []string) string {
	cid := "vpc_" + sanitizeID(r.ProjectID) + "_" + sanitizeID(r.DisplayName)
	if existing, ok := cb.byID[cid]; ok {
		addUniqueBacked(&existing.BackedBy, r)
		cb.containerByBackedURI[r.URI] = cid
		return cid
	}
	projCID := cb.ensureProject(r.ProjectID)
	var badges []string
	if len(svcProjects) > 0 {
		badges = append(badges, fmt.Sprintf("Shared VPC (%d svc projects)", len(svcProjects)))
	}
	cb.addContainer(manifest.Container{
		ID:                cid,
		Kind:              manifest.ContainerVPC,
		Title:             "VPC: " + r.DisplayName,
		Subtitle:          "Host: " + r.ProjectID,
		ParentContainerID: projCID,
		Badges:            badges,
		OrderHint:         40,
		BackedBy: []manifest.BackedResource{{
			URI:       r.URI,
			AssetType: r.AssetType,
			ProjectID: r.ProjectID,
		}},
	})
	return cid
}

func (cb *containerBuilder) ensureRegion(vpcCID, region string) string {
	if region == "" {
		region = "global"
	}
	cid := fmt.Sprintf("reg_%s_%s", vpcCID, sanitizeID(region))
	if _, ok := cb.byID[cid]; ok {
		return cid
	}
	cb.addContainer(manifest.Container{
		ID:                cid,
		Kind:              manifest.ContainerRegion,
		Title:             "Region: " + region,
		ParentContainerID: vpcCID,
		OrderHint:         50,
	})
	return cid
}

func (cb *containerBuilder) ensureSubnet(r extract.FactResource) string {
	region := regionFromLocation(r.Location)
	cid := fmt.Sprintf("sub_%s_%s_%s", sanitizeID(r.ProjectID), sanitizeID(region), sanitizeID(r.DisplayName))
	if existing, ok := cb.byID[cid]; ok {
		addUniqueBacked(&existing.BackedBy, r)
		cb.containerByBackedURI[r.URI] = cid
		return cid
	}
	vpcURI := cmp.Or(r.ParentURI, attrStr(r.Attributes, "vpc"))
	var parentCID string
	if vpcRes, ok := cb.resByURI[vpcURI]; ok {
		vpcCID := cb.ensureVPC(vpcRes, nil)
		parentCID = cb.ensureRegion(vpcCID, region)
	} else {
		parentCID = cb.ensureProject(r.ProjectID)
	}

	exp := "PRIVATE"
	purpose := attrStr(r.Attributes, "purpose")
	attrExp := attrStr(r.Attributes, "exposure")
	lowerName := strings.ToLower(r.DisplayName)
	order := 62
	switch {
	case purpose == "REGIONAL_MANAGED_PROXY" || attrExp == "public_ingress" || strings.Contains(lowerName, "public") || strings.Contains(lowerName, "dmz"):
		exp = "PUBLIC"
		order = 60
	case purpose == "PRIVATE_SERVICE_CONNECT" || strings.Contains(lowerName, "data"):
		exp = "PRIVATE"
		order = 65
	}
	cb.subExposureByURI[r.URI] = exp

	cb.addContainer(manifest.Container{
		ID:                cid,
		Kind:              manifest.ContainerSubnet,
		Exposure:          exp,
		Title:             "Subnet: " + r.DisplayName,
		Subtitle:          attrStr(r.Attributes, "cidr"),
		ParentContainerID: parentCID,
		Badges:            []string{exp},
		OrderHint:         order,
		BackedBy: []manifest.BackedResource{{
			URI:       r.URI,
			AssetType: r.AssetType,
			ProjectID: r.ProjectID,
		}},
	})
	return cid
}

func (cb *containerBuilder) ensureCluster(r extract.FactResource) string {
	loc := cmp.Or(strings.TrimSpace(r.Location), "global")
	cid := fmt.Sprintf("gke_%s_%s_%s", sanitizeID(r.ProjectID), sanitizeID(loc), sanitizeID(r.DisplayName))
	if existing, ok := cb.byID[cid]; ok {
		addUniqueBacked(&existing.BackedBy, r)
		cb.containerByBackedURI[r.URI] = cid
		return cid
	}
	subnetURI := cmp.Or(r.ParentURI, attrStr(r.Attributes, "subnet"))
	var parentCID string
	if subRes, ok := cb.resByURI[subnetURI]; ok {
		parentCID = cb.ensureSubnet(subRes)
	} else {
		parentCID = cb.ensureProject(r.ProjectID)
	}

	var badges []string
	if priv, ok := r.Attributes["private_nodes"].(bool); ok && priv {
		badges = append(badges, "Private Nodes")
	}
	cb.addContainer(manifest.Container{
		ID:                cid,
		Kind:              manifest.ContainerGKECluster,
		Exposure:          "PRIVATE",
		Title:             "GKE Cluster: " + r.DisplayName,
		Subtitle:          r.Location,
		ParentContainerID: parentCID,
		Badges:            badges,
		OrderHint:         70,
		BackedBy: []manifest.BackedResource{{
			URI:       r.URI,
			AssetType: r.AssetType,
			ProjectID: r.ProjectID,
		}},
	})
	return cid
}

func (cb *containerBuilder) ensureZone(parentCID string, kind manifest.ContainerKind, exp, qualifier, title, subtitle string, order int) string {
	cid := fmt.Sprintf("zone_%s_%s_%s", strings.ToLower(string(kind)), strings.ToLower(exp), sanitizeID(parentCID))
	if qualifier != "" {
		cid += "_" + sanitizeID(qualifier)
	}
	if _, ok := cb.byID[cid]; ok {
		return cid
	}
	cb.addContainer(manifest.Container{
		ID:                cid,
		Kind:              kind,
		Exposure:          exp,
		Title:             title,
		Subtitle:          subtitle,
		ParentContainerID: parentCID,
		OrderHint:         order,
	})
	return cid
}

func (cb *containerBuilder) ensureExternalZone(isCaller bool) string {
	if isCaller {
		cid := "ext_sources"
		cb.addContainer(manifest.Container{
			ID:        cid,
			Kind:      manifest.ContainerExternalZone,
			Exposure:  "EXTERNAL",
			Title:     "External Callers & Sources",
			OrderHint: -100,
		})
		return cid
	}
	cid := "ext_targets"
	cb.addContainer(manifest.Container{
		ID:        cid,
		Kind:      manifest.ContainerExternalZone,
		Exposure:  "EXTERNAL",
		Title:     "External Targets & Peered Networks",
		OrderHint: 900,
	})
	return cid
}

func (cb *containerBuilder) placePrimaryResource(r extract.FactResource, reg *icons.Registry) (string, string) {
	if r.IsExternal {
		spec := classifyAssetSpec(r, reg)
		return cb.ensureExternalZone(spec.componentKind == manifest.KindExternalCaller), "EXTERNAL"
	}
	if clusRes, ok := cb.clusterByURI[r.ParentURI]; ok {
		return cb.ensureCluster(clusRes), "PRIVATE"
	}
	if cid, exp, ok := cb.placeHybridGatewayOrHub(r); ok {
		return cid, exp
	}
	if cid, exp, ok := cb.placePSADatastore(r); ok {
		return cid, exp
	}
	if cid, exp, ok := cb.placeSubnetOrConnectorResource(r); ok {
		return cid, exp
	}
	if cid, exp, ok := cb.placeLoadBalancerResource(r); ok {
		return cid, exp
	}
	if cid, exp, ok := cb.placeVPCScopedResource(r); ok {
		return cid, exp
	}
	projCID := cb.ensureProject(r.ProjectID)
	return cb.ensureZone(projCID, manifest.ContainerExposureZone, "PRIVATE", "", "Managed Services & Messaging", "", 80), "PRIVATE"
}

func (cb *containerBuilder) placeHybridGatewayOrHub(r extract.FactResource) (string, string, bool) {
	if r.AssetType == "compute.googleapis.com/VpnGateway" ||
		r.AssetType == "compute.googleapis.com/VpnTunnel" ||
		r.AssetType == "compute.googleapis.com/InterconnectAttachment" {
		vpcURI := cmp.Or(attrStr(r.Attributes, "vpc"), r.ParentURI)
		if vpcRes, ok := cb.resByURI[vpcURI]; ok && vpcRes.AssetType == "compute.googleapis.com/Network" {
			vpcCID := cb.ensureVPC(vpcRes, nil)
			regCID := cb.ensureRegion(vpcCID, regionFromLocation(r.Location))
			return cb.ensureZone(regCID, manifest.ContainerHybridZone, "PRIVATE", "", "Hybrid Connectivity (VPN / Interconnect)", "", 58), "PRIVATE", true
		}
	}
	if r.AssetType == "networkconnectivity.googleapis.com/Hub" {
		projCID := cb.ensureProject(r.ProjectID)
		return cb.ensureZone(projCID, manifest.ContainerSharedHub, "PRIVATE", "", "Network Connectivity Hub", "", 42), "PRIVATE", true
	}
	return "", "", false
}

func (cb *containerBuilder) placePSADatastore(r extract.FactResource) (string, string, bool) {
	psaTenant := attrStr(r.Attributes, "psa_tenant_project")
	if psaTenant == "" {
		return "", "", false
	}
	vpcURI := cmp.Or(attrStr(r.Attributes, "vpc"), r.ParentURI)
	parentCID := cb.ensureVPCRegionOrProject(vpcURI, r.ProjectID, regionFromLocation(r.Location))
	return cb.ensureZone(parentCID, manifest.ContainerPSAZone, "PRIVATE", psaTenant, "PSA Private Service Access", "Tenant: "+psaTenant, 75), "PRIVATE", true
}

func (cb *containerBuilder) placeSubnetOrConnectorResource(r extract.FactResource) (string, string, bool) {
	subnetURI := cb.resolveResourceSubnetURI(r)
	subRes, ok := cb.subnetByURI[subnetURI]
	if !ok {
		return "", "", false
	}
	subCID := cb.ensureSubnet(subRes)
	exp := classifyResourceExposure(r, cb.subExposureByURI[subnetURI])
	zoneTitle, order := "Private Workloads", 64
	switch exp {
	case "PUBLIC":
		zoneTitle, order = "Public Exposure", 61
	case "INTERNAL":
		zoneTitle, order = "Internal Exposure", 63
	}
	return cb.ensureZone(subCID, manifest.ContainerExposureZone, exp, "", zoneTitle, "", order), exp, true
}

func (cb *containerBuilder) resolveResourceSubnetURI(r extract.FactResource) string {
	if _, ok := cb.subnetByURI[r.ParentURI]; ok {
		return r.ParentURI
	}
	if s := attrStr(r.Attributes, "subnet"); cb.subnetByURI[s].URI != "" {
		return s
	}
	if conn, ok := cb.connectorByURI[r.ParentURI]; ok {
		if s := attrStr(conn.Attributes, "subnet"); cb.subnetByURI[s].URI != "" {
			return s
		}
		if cb.subnetByURI[conn.ParentURI].URI != "" {
			return conn.ParentURI
		}
	}
	return ""
}

func (cb *containerBuilder) placeLoadBalancerResource(r extract.FactResource) (string, string, bool) {
	lbScheme := attrStr(r.Attributes, "load_balancing_scheme")
	if lbScheme == "" {
		return "", "", false
	}
	exp, zoneTitle := "INTERNAL", "Internal Load Balancing"
	if strings.HasPrefix(lbScheme, "EXTERNAL") {
		exp, zoneTitle = "PUBLIC", "Public Edge Ingress"
	}
	if vpcURI := cb.findProjectVPC(r.ProjectID); vpcURI != "" {
		if vpcRes, okV := cb.resByURI[vpcURI]; okV {
			vpcCID := cb.ensureVPC(vpcRes, nil)
			return cb.ensureZone(vpcCID, manifest.ContainerExposureZone, exp, lbScheme, zoneTitle, lbScheme, 45), exp, true
		}
	}
	projCID := cb.ensureProject(r.ProjectID)
	return cb.ensureZone(projCID, manifest.ContainerExposureZone, exp, lbScheme, zoneTitle, lbScheme, 45), exp, true
}

func (cb *containerBuilder) placeVPCScopedResource(r extract.FactResource) (string, string, bool) {
	vpcURI := cmp.Or(attrStr(r.Attributes, "vpc"), r.ParentURI)
	if vpcRes, ok := cb.resByURI[vpcURI]; ok && vpcRes.AssetType == "compute.googleapis.com/Network" {
		parentCID := cb.ensureVPCRegionOrProject(vpcURI, vpcRes.ProjectID, regionFromLocation(r.Location))
		return cb.ensureZone(parentCID, manifest.ContainerExposureZone, "PRIVATE", "", "Private VPC Services", "", 72), "PRIVATE", true
	}
	return "", "", false
}

// classifyResourceExposure applies exact enum matching (never strings.Contains(ing, "ALL")).
func classifyResourceExposure(r extract.FactResource, subnetDefault string) string {
	if lbScheme := attrStr(r.Attributes, "load_balancing_scheme"); lbScheme != "" {
		if strings.HasPrefix(lbScheme, "EXTERNAL") {
			return "PUBLIC"
		}
		if strings.HasPrefix(lbScheme, "INTERNAL") {
			return "INTERNAL"
		}
	}
	switch attrStr(r.Attributes, "ingress") {
	case "INGRESS_TRAFFIC_ALL", "ALLOW_ALL":
		return "PUBLIC"
	case "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER", "INGRESS_TRAFFIC_INTERNAL_ONLY", "ALLOW_INTERNAL_ONLY":
		return "PRIVATE"
	}
	if subnetDefault != "" {
		return subnetDefault
	}
	return "PRIVATE"
}

func (cb *containerBuilder) findProjectVPC(projectID string) string {
	if v := cb.vpcURIByProj[projectID]; v != "" {
		return v
	}
	if p, ok := cb.projectByID[projectID]; ok && p.SharedVPCHostProject != "" {
		return cb.vpcURIByProj[p.SharedVPCHostProject]
	}
	return ""
}

func (cb *containerBuilder) ensureVPCRegionOrProject(vpcURI, projectID, region string) string {
	if vpcRes, ok := cb.resByURI[vpcURI]; ok && vpcRes.AssetType == "compute.googleapis.com/Network" {
		vpcCID := cb.ensureVPC(vpcRes, nil)
		return cb.ensureRegion(vpcCID, region)
	}
	return cb.ensureProject(projectID)
}

func (cb *containerBuilder) resolveTransversalOwnerContainer(r extract.FactResource) string {
	if cid := cb.containerByBackedURI[r.ParentURI]; cid != "" {
		return cid
	}
	if cid := cb.containerByBackedURI[attrStr(r.Attributes, "subnet")]; cid != "" {
		return cid
	}
	if cid := cb.containerByBackedURI[attrStr(r.Attributes, "vpc")]; cid != "" {
		return cid
	}
	if pid := "proj_" + sanitizeID(r.ProjectID); cb.byID[pid] != nil {
		return pid
	}
	return ""
}

func (cb *containerBuilder) finalizeContainers(nodes []skeletonNode) []manifest.Container {
	keep := make(map[string]bool)
	markWithAncestors := func(cid string) {
		cur := cid
		for cur != "" && !keep[cur] {
			keep[cur] = true
			if ct, ok := cb.byID[cur]; ok {
				cur = ct.ParentContainerID
			} else {
				break
			}
		}
	}
	for _, n := range nodes {
		markWithAncestors(n.ContainerID)
	}
	// Never drop a structural resource-backed container (Network, Subnetwork, Cluster)
	// or a container anchoring folded edges.
	for _, ct := range cb.containers {
		if len(ct.BackedBy) > 0 || len(ct.FoldedEdgeIDs) > 0 {
			markWithAncestors(ct.ID)
		}
	}

	var out []manifest.Container
	for _, ct := range cb.containers {
		if keep[ct.ID] {
			out = append(out, *ct)
		}
	}
	slices.SortFunc(out, func(a, b manifest.Container) int {
		return cmp.Or(cmp.Compare(a.OrderHint, b.OrderHint), cmp.Compare(a.ID, b.ID))
	})
	return out
}

func addUniqueBacked(dst *[]manifest.BackedResource, r extract.FactResource) {
	for _, br := range *dst {
		if br.URI == r.URI {
			return
		}
	}
	*dst = append(*dst, manifest.BackedResource{
		URI:       r.URI,
		AssetType: r.AssetType,
		ProjectID: r.ProjectID,
	})
}
