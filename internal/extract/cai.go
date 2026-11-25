package extract

import (
	"container/heap"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/asset/apiv1/assetpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	caiPageSize = 1000
	// DefaultCloudBuildRuns is the default number of recent successful Cloud Build runs folded per trigger.
	DefaultCloudBuildRuns        = 3
	maxTriggerlessCloudBuildRuns = 5
)

// ExtractOptions configures a multi-project Stage 1 extraction run.
type ExtractOptions struct {
	Scopes         []string
	CloudBuildRuns int // <= 0 defaults to DefaultCloudBuildRuns (3)
}

// iamPolicyAssetTypes restricts CAI IAM_POLICY listing to organizational hierarchy and
// architectural resource types relevant for workload identity and resource-level edge attribution.
var iamPolicyAssetTypes = []string{
	"cloudresourcemanager.googleapis.com/Organization",
	"cloudresourcemanager.googleapis.com/Folder",
	assetTypeProject,
	assetTypeServiceAccount,
	assetTypeSubnetwork,
	"storage.googleapis.com/Bucket",
	"bigquery.googleapis.com/Dataset",
	"pubsub.googleapis.com/Topic",
	"pubsub.googleapis.com/Subscription",
	"secretmanager.googleapis.com/Secret",
	assetTypeRunService,
	"cloudfunctions.googleapis.com/CloudFunction",
	"aiplatform.googleapis.com/Endpoint",
	"aiplatform.googleapis.com/ReasoningEngine",
	"spanner.googleapis.com/Instance",
	"bigtableadmin.googleapis.com/Instance",
	"cloudtasks.googleapis.com/Queue",
	"workflows.googleapis.com/Workflow",
	"cloudkms.googleapis.com/CryptoKey",
	"artifactregistry.googleapis.com/Repository",
}

type structuralRef struct {
	targetURI  string
	defaultSvc string
	direction  string // "out" (asset -> target) or "in" (target -> asset)
	detail     string
	kind       string // "", "http", "bucket", "bq_dataset", "cloudsql", "datastore_id"
}

type vpcPeeringInfo struct {
	name    string
	network string
	state   string
}

type runTrafficEntry struct {
	revisionName string
	percent      int
	latest       bool
}

type ingestedAsset struct {
	uri             string
	assetType       string
	projectID       string
	location        string
	displayName     string
	parentURI       string
	attrs           map[string]any
	capabilities    []string
	envRefs         []string
	secretRefs      []string
	labels          map[string]string
	computeMetadata map[string]string
	outboundRefs    []structuralRef
	vpcPeerings     []vpcPeeringInfo
	privateIPs      []string
	runURLs         []string
	runTraffic      []runTrafficEntry
	runLatestReady  string
	aeSplit         map[string]float64
	frTarget        string
	frBackendSvc    string
	cbTriggerID     string
	cbRepoHost      string
	cbRepoOwner     string
	cbRepoName      string
	cbTriggerFilter string
	cbInlineRefs    []string
	cbInlineSecs    []string
	saUniqueID      string
	saEmail         string
	suppressed      bool
}

type urlMapBackendEntry struct {
	backendURI string
	paths      []string
}

type lbIntermediate struct {
	uri             string
	assetType       string
	projectID       string
	location        string
	urlMap          string
	service         string
	isTLS           bool
	defaultService  string
	urlMapHosts     []string
	urlMapBackends  []urlMapBackendEntry
	backendGroups   []string
	bucketName      string
	negCloudRunSvc  string
	negCloudFn      string
	negAppEngineSvc string
	negPSCTarget    string
}

type absorbedRecord struct {
	uri                 string
	assetType           string
	projectID           string
	location            string
	router              string
	vpnGateway          string
	hub                 string
	linkedVPC           string
	linkedVPNs          []string
	linkedInterconnects []string
	gcsBucket           string
	privateConnection   string
	hostnames           []string
	vpc                 string
	parentURI           string
	privateIP           string
	bqSourceURI         string
	resourceSpecName    string
}

type revisionSummary struct {
	uri               string
	parentURI         string
	serviceAccount    string
	vpcConnector      string
	networkURI        string
	subnetURI         string
	envRefs           []string
	secretRefs        []string
	cloudSQLInstances []string
}

type iamBindingRecord struct {
	resourceURI   string
	assetType     string
	projectID     string
	role          string
	members       []string
	conditionExpr string
}

type cbHeapKey struct {
	projectID string
	triggerID string
}

type cbBuildItem struct {
	uri            string
	createTime     time.Time
	refs           []string
	secretRefs     []string
	serviceAccount string
}

type cbBuildMinHeap []*cbBuildItem

func (h cbBuildMinHeap) Len() int { return len(h) }
func (h cbBuildMinHeap) Less(i, j int) bool {
	if !h[i].createTime.Equal(h[j].createTime) {
		return h[i].createTime.Before(h[j].createTime)
	}
	return h[i].uri < h[j].uri
}
func (h cbBuildMinHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *cbBuildMinHeap) Push(x any)   { *h = append(*h, x.(*cbBuildItem)) }
func (h *cbBuildMinHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return item
}

type oosKey struct {
	targetProjectID string
	sourceKind      string
}

type extractState struct {
	scopes          []string
	readTime        time.Time
	cloudBuildRuns  int
	projIdx         map[string]string
	inScopeProjects map[string]bool

	orgs            map[string]OrgNode
	folders         map[string]FolderNode
	projects        map[string]ProjectNode
	enabledServices map[string]map[string]bool

	suppressed         map[string]int
	foldedCounts       map[string]map[string]int
	foldedExternalURIs map[string][]string

	assets       []*ingestedAsset
	assetsByURI  map[string]*ingestedAsset
	lbAssets     map[string]*lbIntermediate
	absorbed     map[string]*absorbedRecord
	revisions    map[string]*revisionSummary
	iamBindings  []*iamBindingRecord
	saByUniqueID map[string]string
	saByEmail    map[string]*ingestedAsset

	gke             *gkeState
	cbHeaps         map[cbHeapKey]*cbBuildMinHeap
	cbTriggers      map[cbHeapKey]string
	gkeResolvedNEGs map[string]bool

	vpcPeers         map[string]map[string]bool
	psaTenants       map[string]string // consumer VPC URI -> PSA tenant project ID
	psaProjectSet    map[string]bool
	subnetToVPC      map[string]string
	connectorToVPC   map[string]string
	privateIPTargets map[netIPKey][]string
	runHostToURI     map[string]string
	runNameToURIs    map[string][]string

	edges             []FactEdge
	edgeSeen          map[string]bool
	externalResources map[string]FactResource
	outOfScope        map[oosKey]*OutOfScopeHint
	referencedURIs    map[string]bool

	ledgerMu sync.Mutex
	ledger   []CoverageEntry
	edgeMu   sync.Mutex
}

func newExtractState(scopes []string, aliases map[string]string, cbRuns int) *extractState {
	if cbRuns <= 0 {
		cbRuns = DefaultCloudBuildRuns
	}
	projIdx := make(map[string]string, len(aliases))
	for k, v := range aliases {
		if k != "" && v != "" {
			projIdx[k] = v
		}
	}
	return &extractState{
		scopes:             uniqueSortedStrings(scopes),
		cloudBuildRuns:     cbRuns,
		projIdx:            projIdx,
		inScopeProjects:    make(map[string]bool),
		orgs:               make(map[string]OrgNode),
		folders:            make(map[string]FolderNode),
		projects:           make(map[string]ProjectNode),
		enabledServices:    make(map[string]map[string]bool),
		suppressed:         make(map[string]int),
		foldedCounts:       make(map[string]map[string]int),
		foldedExternalURIs: make(map[string][]string),
		assetsByURI:        make(map[string]*ingestedAsset),
		lbAssets:           make(map[string]*lbIntermediate),
		absorbed:           make(map[string]*absorbedRecord),
		revisions:          make(map[string]*revisionSummary),
		saByUniqueID:       make(map[string]string),
		saByEmail:          make(map[string]*ingestedAsset),
		gke:                newGKEState(),
		cbHeaps:            make(map[cbHeapKey]*cbBuildMinHeap),
		cbTriggers:         make(map[cbHeapKey]string),
		gkeResolvedNEGs:    make(map[string]bool),
		vpcPeers:           make(map[string]map[string]bool),
		psaTenants:         make(map[string]string),
		psaProjectSet:      make(map[string]bool),
		subnetToVPC:        make(map[string]string),
		connectorToVPC:     make(map[string]string),
		privateIPTargets:   make(map[netIPKey][]string),
		runHostToURI:       make(map[string]string),
		runNameToURIs:      make(map[string][]string),
		edgeSeen:           make(map[string]bool),
		externalResources:  make(map[string]FactResource),
		outOfScope:         make(map[oosKey]*OutOfScopeHint),
		referencedURIs:     make(map[string]bool),
	}
}

// Extract orchestrates Stage 1 multi-project read-only extraction across CAI (RESOURCE & IAM_POLICY),
// GKE Ingress/Service/Workload/Workload-Identity joins, in-stream Cloud Build Build folding,
// structural/private-IP/PSA/IAM edge resolution, and concurrent BigQuery/Dataflow/Monitoring enrichers,
// producing a deterministic, credential-scrubbed v3 FactsFile.
func Extract(ctx context.Context, clients *Clients, opts ExtractOptions) (*FactsFile, error) {
	if clients == nil || clients.Asset == nil {
		return nil, fmt.Errorf("extract requires initialized Clients with Asset service")
	}
	if len(opts.Scopes) == 0 {
		return nil, fmt.Errorf("at least one CAI scope (projects/..., folders/..., or organizations/...) is required")
	}

	st := newExtractState(opts.Scopes, clients.ProjectAliases, opts.CloudBuildRuns)

	// 1. ReadTime-pinned CAI RESOURCE & IAM_POLICY streaming.
	if err := st.collectCAI(ctx, clients); err != nil {
		return nil, err
	}

	// 2. Re-canonicalize with complete projIdx, reconcile active revisions, fold counts.
	st.reconcilePostCAI()

	// 3. Suppress GKE node infra and index VPC/PSA peering, private IPs, and Cloud Run hosts.
	st.suppressGKENodeInfra()
	st.indexVPCAndPrivateIPs()
	st.indexCloudRunHosts()

	// 4. Resolve GKE (ConfigMap refs, Workload Identity, Service->Workload, LB->NEG->Service, Ingress).
	st.resolveGKE()

	// 5. Resolve Cloud Build triggers, synthetic GitHub/GitLab repos, and min-heap succeeded builds.
	st.resolveCloudBuild()

	// 6. Resolve remaining LB/structural pointers, env/ConfigMap refs, and IAM bindings.
	st.resolveStructuralAndIAM()

	// 7. Run bounded concurrent enrichers (BigQuery SQL lineage, active Dataflow jobs, Cloud Monitoring).
	if err := st.runEnrichers(ctx, clients); err != nil {
		return nil, err
	}

	// 8. Prune unused default sandbox assets & assemble deterministic v3 FactsFile.
	st.pruneDefaultSandboxAssets()
	return st.buildFactsFile(), nil
}

func (st *extractState) addCoverage(entry CoverageEntry) {
	st.ledgerMu.Lock()
	defer st.ledgerMu.Unlock()
	st.ledger = append(st.ledger, entry)
}

func (st *extractState) addEdge(fromURI, toURI string, kind FactEdgeKind, detail string) {
	if fromURI == "" || toURI == "" || fromURI == toURI {
		return
	}
	id := EdgeID(kind, fromURI, toURI, detail)
	st.edgeMu.Lock()
	defer st.edgeMu.Unlock()
	if st.edgeSeen[id] {
		return
	}
	st.edgeSeen[id] = true
	st.edges = append(st.edges, FactEdge{
		ID:      id,
		FromURI: fromURI,
		ToURI:   toURI,
		Kind:    kind,
		Detail:  detail,
	})
}

func (st *extractState) collectCAI(ctx context.Context, clients *Clients) error {
	pinnedReadTime, err := st.collectCAIResources(ctx, clients)
	if err != nil {
		return err
	}
	st.ensureExplicitProjectScopes()
	return st.collectCAIIAMPolicies(ctx, clients, pinnedReadTime)
}

func (st *extractState) collectCAIResources(ctx context.Context, clients *Clients) (*timestamppb.Timestamp, error) {
	var pinnedReadTime *timestamppb.Timestamp
	for _, scope := range st.scopes {
		if err := st.collectCAIResourceScope(ctx, clients, scope, &pinnedReadTime); err != nil {
			return nil, err
		}
	}
	return pinnedReadTime, nil
}

func (st *extractState) collectCAIResourceScope(ctx context.Context, clients *Clients, scope string, pinnedReadTime **timestamppb.Timestamp) error {
	pageToken := ""
	page := 0
	scanned := 0
	for {
		resp, err := listCAIPage(ctx, clients, &assetpb.ListAssetsRequest{
			Parent:      scope,
			ContentType: assetpb.ContentType_RESOURCE,
			PageSize:    caiPageSize,
			PageToken:   pageToken,
			ReadTime:    *pinnedReadTime,
		})
		if err != nil {
			handled, fatalErr := st.handleCAIScopeError(err, clients.QuotaProject, scope, "cai_resource", "RESOURCE")
			if handled {
				return nil
			}
			return fatalErr
		}

		page++
		if *pinnedReadTime == nil && resp.GetReadTime() != nil {
			*pinnedReadTime = resp.GetReadTime()
			st.readTime = (*pinnedReadTime).AsTime().UTC()
		}
		for _, a := range resp.GetAssets() {
			st.ingestAsset(a)
		}
		scanned += len(resp.GetAssets())
		clients.ReportProgress(false, "CAI RESOURCE %s: page %d (%d scanned, %d retained)", scope, page, scanned, len(st.assets))

		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}
	clients.ReportProgress(true, "CAI RESOURCE %s: complete (%d pages, %d scanned, %d retained)", scope, page, scanned, len(st.assets))
	st.addCoverage(CoverageEntry{Scope: scope, Source: "cai_resource", State: CoverageOK})
	return nil
}

func (st *extractState) ensureExplicitProjectScopes() {
	for _, scope := range st.scopes {
		if !strings.HasPrefix(scope, prefixProjects) {
			continue
		}
		p := mapProject(strings.TrimPrefix(scope, prefixProjects), st.projIdx)
		if _, ok := st.projects[p]; !ok {
			st.projects[p] = ProjectNode{ProjectID: p, DisplayName: p}
		}
		st.inScopeProjects[p] = true
	}
}

func (st *extractState) collectCAIIAMPolicies(ctx context.Context, clients *Clients, pinnedReadTime *timestamppb.Timestamp) error {
	for _, scope := range st.scopes {
		if err := st.collectCAIIAMPolicyScope(ctx, clients, scope, pinnedReadTime); err != nil {
			return err
		}
	}
	return nil
}

func (st *extractState) collectCAIIAMPolicyScope(ctx context.Context, clients *Clients, scope string, pinnedReadTime *timestamppb.Timestamp) error {
	pageToken := ""
	page := 0
	for {
		resp, err := listCAIPage(ctx, clients, &assetpb.ListAssetsRequest{
			Parent:      scope,
			ContentType: assetpb.ContentType_IAM_POLICY,
			AssetTypes:  iamPolicyAssetTypes,
			PageSize:    caiPageSize,
			PageToken:   pageToken,
			ReadTime:    pinnedReadTime,
		})
		if err != nil {
			handled, fatalErr := st.handleCAIScopeError(err, clients.QuotaProject, scope, "cai_iam_policy", "IAM_POLICY")
			if handled {
				return nil
			}
			return fatalErr
		}

		page++
		for _, a := range resp.GetAssets() {
			st.ingestIAMPolicyAsset(a)
		}
		clients.ReportProgress(false, "CAI IAM_POLICY %s: page %d (%d bindings)", scope, page, len(st.iamBindings))

		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}
	st.addCoverage(CoverageEntry{Scope: scope, Source: "cai_iam_policy", State: CoverageOK})
	return nil
}

func listCAIPage(ctx context.Context, clients *Clients, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
	var resp *assetpb.ListAssetsResponse
	err := retryOnRateLimit(ctx, clients.retryBackoff, func() error {
		if err := waitLimiter(ctx, clients.CAILimiter); err != nil {
			return err
		}
		var callErr error
		resp, callErr = clients.Asset.ListAssets(ctx, req)
		return callErr
	})
	return resp, err
}

func (st *extractState) handleCAIScopeError(err error, quotaProject, scope, source, contentLabel string) (degraded bool, fatalErr error) {
	ce := ClassifyGCPError(err, quotaProject, "", "")
	if ce.IsFatalQuota() {
		return false, &QuotaProjectError{Classified: ce}
	}
	if ce.IsDegraded() {
		st.addCoverage(CoverageEntry{
			Scope:  scope,
			Source: source,
			State:  ce.CoverageState(),
			Detail: ce.Message,
		})
		return true, nil
	}
	return false, fmt.Errorf("CAI ListAssets(%s, %s): %w", contentLabel, scope, err)
}

func (st *extractState) ingestIAMPolicyAsset(a *assetpb.Asset) {
	pol := a.GetIamPolicy()
	if pol == nil {
		return
	}
	resURI := CanonicalizeAIP122(a.GetName(), st.projIdx)
	projID := extractProjectID(resURI, a.GetAncestors(), st.projIdx)
	isSubnetwork := a.GetAssetType() == assetTypeSubnetwork
	for _, b := range pol.GetBindings() {
		if isSubnetwork && !isSubnetAttachmentRole(b.GetRole()) {
			continue
		}
		condExpr := ""
		if cond := b.GetCondition(); cond != nil {
			condExpr = cond.GetExpression()
		}
		members := append([]string(nil), b.GetMembers()...)
		sort.Strings(members)
		st.iamBindings = append(st.iamBindings, &iamBindingRecord{
			resourceURI:   resURI,
			assetType:     a.GetAssetType(),
			projectID:     projID,
			role:          b.GetRole(),
			members:       members,
			conditionExpr: condExpr,
		})
	}
}

func isSubnetAttachmentRole(role string) bool {
	return role == "roles/compute.networkUser" ||
		strings.Contains(role, "networkUser") ||
		strings.Contains(role, "compute.subnetworks.use")
}

func (st *extractState) ingestAsset(asset *assetpb.Asset) {
	if asset == nil {
		return
	}
	assetType := asset.GetAssetType()
	rawName := asset.GetName()
	res := asset.GetResource()
	var data *structpb.Struct
	if res != nil {
		data = res.GetData()
	}

	if st.ingestHierarchyAsset(assetType, rawName, res, data, asset.GetAncestors()) {
		return
	}

	canonURI := CanonicalizeAIP122(rawName, st.projIdx)
	projID := extractProjectID(canonURI, asset.GetAncestors(), st.projIdx)
	location := ""
	if res != nil {
		location = res.GetLocation()
	}
	if location == "" {
		location = extractLocationFromURI(canonURI)
	}

	if st.ingestFoldedOrPlumbingAsset(canonURI, assetType, projID, location, res, data) {
		return
	}

	// Retained K8s workloads/services/ingresses: drop system namespaces into SuppressedPlumbingCounts.
	if isRetainedK8sAssetType(assetType) && isSystemK8sNamespace(extractK8sNamespace(canonURI, data)) {
		st.suppressed[assetType]++
		return
	}

	rec := &ingestedAsset{
		uri:         canonURI,
		assetType:   assetType,
		projectID:   projID,
		location:    location,
		displayName: extractDisplayName(canonURI, assetType, data),
		attrs:       ProjectAttributes(assetType, data, projID, st.projIdx),
		labels:      extractAssetLabels(data),
	}
	st.expandBareSubnetAttr(rec)

	if isRetainedK8sAssetType(assetType) {
		st.indexRetainedK8sAsset(rec, res, data)
	} else {
		st.extractRetainedMetadata(rec, data)
	}

	st.assets = append(st.assets, rec)
	st.assetsByURI[rec.uri] = rec
}

func (st *extractState) ingestHierarchyAsset(assetType, rawName string, res *assetpb.Resource, data *structpb.Struct, ancestors []string) bool {
	switch assetType {
	case "cloudresourcemanager.googleapis.com/Organization":
		st.ingestOrganization(rawName, data)
	case "cloudresourcemanager.googleapis.com/Folder":
		st.ingestFolder(rawName, res, data, ancestors)
	case assetTypeProject:
		st.ingestProject(rawName, data, ancestors)
	case "serviceusage.googleapis.com/Service":
		st.ingestEnabledService(rawName, data, ancestors)
	default:
		return false
	}
	return true
}

func (st *extractState) ingestFoldedOrPlumbingAsset(canonURI, assetType, projID, location string, res *assetpb.Resource, data *structpb.Struct) bool {
	switch assetType {
	case "cloudbuild.googleapis.com/Build":
		st.suppressed[assetType]++
		st.ingestCloudBuildRun(canonURI, projID, data)
		return true
	case "bigquery.googleapis.com/Table":
		st.suppressed[assetType]++
		st.foldBigQueryChild(canonURI, "table_count", data)
		return true
	case "bigquery.googleapis.com/Routine":
		st.suppressed[assetType]++
		st.foldBigQueryChild(canonURI, "routine_count", data)
		return true
	case "bigquery.googleapis.com/Model":
		st.suppressed[assetType]++
		st.foldBigQueryChild(canonURI, "model_count", data)
		return true
	case "artifactregistry.googleapis.com/DockerImage":
		st.suppressed[assetType]++
		st.addFoldCount(parentArtifactRepoURI(canonURI), "image_count")
		return true
	case "artifactregistry.googleapis.com/Package":
		st.suppressed[assetType]++
		st.addFoldCount(parentArtifactRepoURI(canonURI), "package_count")
		return true
	case "artifactregistry.googleapis.com/Version":
		st.suppressed[assetType]++
		st.addFoldCount(parentArtifactRepoURI(canonURI), "version_count")
		return true
	case "spanner.googleapis.com/Database":
		st.suppressed[assetType]++
		st.addFoldCount(parentBeforeSegment(canonURI, "/databases/"), "database_count")
		return true
	case "bigtableadmin.googleapis.com/Cluster":
		st.suppressed[assetType]++
		st.addFoldCount(parentBeforeSegment(canonURI, segClusters), "cluster_count")
		return true
	case "bigtableadmin.googleapis.com/Table":
		st.suppressed[assetType]++
		st.addFoldCount(parentBeforeSegment(canonURI, "/tables/"), "table_count")
		return true
	case "container.googleapis.com/NodePool":
		st.suppressed[assetType]++
		st.foldNodePool(canonURI, res.GetParent(), data)
		return true
	case "k8s.io/Namespace":
		st.suppressed[assetType]++
		st.addFoldCount(extractGKEClusterURI(canonURI, res.GetParent(), st.projIdx), "namespace_count")
		return true
	case "k8s.io/ServiceAccount":
		st.suppressed[assetType]++
		clusterURI := extractGKEClusterURI(canonURI, res.GetParent(), st.projIdx)
		st.gke.indexK8sServiceAccount(clusterURI, extractK8sNamespace(canonURI, data), extractK8sName(canonURI, data), data)
		return true
	case "k8s.io/ConfigMap":
		st.suppressed[assetType]++
		clusterURI := extractGKEClusterURI(canonURI, res.GetParent(), st.projIdx)
		st.gke.indexK8sConfigMap(clusterURI, extractK8sNamespace(canonURI, data), extractK8sName(canonURI, data), data, st.projIdx)
		return true
	case "run.googleapis.com/Revision":
		st.suppressed[assetType]++
		st.foldCloudRunRevision(canonURI, projID, data)
		return true
	case "appengine.googleapis.com/Version":
		st.suppressed[assetType]++
		st.foldAppEngineVersion(canonURI, data)
		return true
	case "compute.googleapis.com/TargetHttpProxy",
		"compute.googleapis.com/TargetHttpsProxy",
		"compute.googleapis.com/TargetGrpcProxy",
		"compute.googleapis.com/TargetTcpProxy",
		"compute.googleapis.com/TargetSslProxy",
		"compute.googleapis.com/UrlMap",
		"compute.googleapis.com/BackendService",
		"compute.googleapis.com/BackendBucket",
		"compute.googleapis.com/NetworkEndpointGroup":
		st.suppressed[assetType]++
		st.ingestLBIntermediate(canonURI, assetType, projID, location, data)
		return true
	case "compute.googleapis.com/VpnTunnel",
		"networkconnectivity.googleapis.com/Spoke",
		"datastream.googleapis.com/ConnectionProfile",
		"datastream.googleapis.com/PrivateConnection",
		"alloydb.googleapis.com/Instance",
		"aiplatform.googleapis.com/FeatureView",
		"analyticshub.googleapis.com/Listing",
		"dataplex.googleapis.com/Asset":
		st.suppressed[assetType]++
		st.ingestAbsorbedRecord(canonURI, assetType, projID, location, data)
		return true
	}
	if IsPlumbingAssetType(assetType) {
		st.suppressed[assetType]++
		return true
	}
	return false
}

func (st *extractState) foldNodePool(canonURI, resParent string, data *structpb.Struct) {
	clusterURI := extractGKEClusterURI(canonURI, resParent, st.projIdx)
	st.addFoldCount(clusterURI, "node_pool_count")
	if clusterURI == "" {
		return
	}
	for _, ig := range getStringListField(data, "instanceGroupUrls") {
		list := st.gke.clusterIGURLs[clusterURI]
		appendUnique(&list, CanonicalizeAIP122WithService(ig, svcCompute, st.projIdx))
		st.gke.clusterIGURLs[clusterURI] = list
	}
}

func (st *extractState) expandBareSubnetAttr(rec *ingestedAsset) {
	sub := getAttrString(rec.attrs, "subnet")
	if sub == "" || strings.Contains(sub, "/") {
		return
	}
	subProj := getAttrString(rec.attrs, "subnet_project")
	if subProj == "" {
		subProj = rec.projectID
	}
	if reg := regionFromLocation(rec.location); reg != "" && subProj != "" {
		rec.attrs["subnet"] = prefixComputeProjects + mapProject(subProj, st.projIdx) + "/regions/" + reg + "/subnetworks/" + sub
	}
}

func (st *extractState) indexRetainedK8sAsset(rec *ingestedAsset, res *assetpb.Resource, data *structpb.Struct) {
	clusterURI := extractGKEClusterURI(rec.uri, res.GetParent(), st.projIdx)
	ns := extractK8sNamespace(rec.uri, data)
	name := extractK8sName(rec.uri, data)
	rec.parentURI = clusterURI
	if rec.attrs == nil {
		rec.attrs = make(map[string]any)
	}
	if ns != "" {
		rec.attrs["namespace"] = ns
	}
	switch rec.assetType {
	case "apps.k8s.io/Deployment", "apps.k8s.io/StatefulSet", "apps.k8s.io/DaemonSet",
		"batch.k8s.io/CronJob", "batch.k8s.io/Job":
		rec.envRefs, rec.secretRefs = st.gke.indexK8sWorkload(rec.uri, rec.assetType, clusterURI, ns, rec.projectID, data, st.projIdx)
	case "k8s.io/Service":
		st.gke.indexK8sService(rec.uri, clusterURI, ns, name, rec.projectID, data)
	case "networking.k8s.io/Ingress":
		st.gke.indexK8sIngress(rec.uri, clusterURI, ns, rec.projectID, data)
	}
}

func parentBeforeSegment(uri, marker string) string {
	if idx := strings.Index(uri, marker); idx > 0 {
		return uri[:idx]
	}
	return ""
}

func (st *extractState) ingestCloudBuildRun(canonURI, projID string, data *structpb.Struct) {
	if data == nil || !strings.EqualFold(strings.TrimSpace(getStringField(data, "status")), "SUCCESS") {
		return
	}
	triggerID := strings.TrimSpace(getStringField(data, "buildTriggerId"))
	ts := parseBuildTime(data)
	refs, secRefs := extractCloudBuildPayloadRefs(data, st.projIdx)
	sa := strings.TrimSpace(getStringField(data, "serviceAccount"))

	item := &cbBuildItem{
		uri:            canonURI,
		createTime:     ts,
		refs:           refs,
		secretRefs:     secRefs,
		serviceAccount: sa,
	}
	key := cbHeapKey{projectID: projID, triggerID: triggerID}
	h, ok := st.cbHeaps[key]
	if !ok {
		h = &cbBuildMinHeap{}
		heap.Init(h)
		st.cbHeaps[key] = h
	}
	limit := st.cloudBuildRuns
	if triggerID == "" {
		limit = maxTriggerlessCloudBuildRuns
	}
	if h.Len() < limit {
		heap.Push(h, item)
		return
	}
	oldest := (*h)[0]
	if item.createTime.After(oldest.createTime) || (item.createTime.Equal(oldest.createTime) && item.uri > oldest.uri) {
		(*h)[0] = item
		heap.Fix(h, 0)
	}
}

func parseBuildTime(data *structpb.Struct) time.Time {
	for _, field := range []string{"createTime", "startTime", "finishTime"} {
		if raw := strings.TrimSpace(getStringField(data, field)); raw != "" {
			if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

func (st *extractState) ingestOrganization(rawName string, data *structpb.Struct) {
	orgID := shortResourceName(rawName)
	if orgID == "" {
		orgID = shortResourceName(getStringField(data, "name"))
	}
	if orgID == "" {
		return
	}
	dn := strings.TrimSpace(getStringField(data, "displayName"))
	existing := st.orgs[orgID]
	if dn == "" {
		dn = existing.DisplayName
	}
	st.orgs[orgID] = OrgNode{OrgID: orgID, DisplayName: dn}
}

func (st *extractState) ingestFolder(rawName string, res *assetpb.Resource, data *structpb.Struct, ancestors []string) {
	folderID := shortResourceName(rawName)
	if folderID == "" {
		folderID = shortResourceName(getStringField(data, "name"))
	}
	if folderID == "" {
		return
	}
	dn := strings.TrimSpace(getStringField(data, "displayName"))
	parent := strings.TrimSpace(getStringField(data, "parent"))
	if parent == "" && res != nil {
		parent = strings.TrimSpace(res.GetParent())
	}
	if parent == "" && len(ancestors) >= 2 {
		parent = ancestors[1]
	}
	parent = strings.TrimPrefix(parent, "//cloudresourcemanager.googleapis.com/")
	existing := st.folders[folderID]
	if dn == "" {
		dn = existing.DisplayName
	}
	if parent == "" {
		parent = existing.Parent
	}
	st.folders[folderID] = FolderNode{FolderID: folderID, DisplayName: dn, Parent: parent}
}

func (st *extractState) ingestProject(rawName string, data *structpb.Struct, ancestors []string) {
	p := ProjectNode{
		ProjectID:     strings.TrimSpace(getStringField(data, "projectId")),
		ProjectNumber: strings.TrimSpace(getStringField(data, "projectNumber")),
		DisplayName:   strings.TrimSpace(getStringField(data, "name")),
		Parent:        resolveProjectParent(data, ancestors),
	}
	populateProjectIdentity(&p, rawName, st.projIdx)
	if p.ProjectID == "" {
		return
	}
	if IsPSATenantProject(p.ProjectID) {
		p.IsPSATenant = true
		st.psaProjectSet[p.ProjectID] = true
	}
	existing := st.projects[p.ProjectID]
	p.SharedVPCHostProject = existing.SharedVPCHostProject
	p.IsPSATenant = p.IsPSATenant || existing.IsPSATenant
	st.projects[p.ProjectID] = p
	st.inScopeProjects[p.ProjectID] = true
	if p.ProjectNumber != "" {
		st.projIdx[p.ProjectNumber] = p.ProjectID
	}
}

func populateProjectIdentity(p *ProjectNode, rawName string, projIdx map[string]string) {
	if p.ProjectNumber == "" {
		if last := shortResourceName(rawName); isNumeric(last) {
			p.ProjectNumber = last
		} else if p.ProjectID == "" {
			p.ProjectID = last
		}
	}
	if p.ProjectID == "" {
		p.ProjectID = mapProject(p.ProjectNumber, projIdx)
	}
	if p.DisplayName == "" {
		p.DisplayName = p.ProjectID
	}
}

func resolveProjectParent(data *structpb.Struct, ancestors []string) string {
	if len(ancestors) >= 2 {
		return ancestors[1]
	}
	parent := getStructField(data, "parent")
	if parent == nil {
		return ""
	}
	pt := strings.TrimSpace(getStringField(parent, "type"))
	pid := strings.TrimSpace(getStringField(parent, "id"))
	if pt != "" && pid != "" {
		return pt + "s/" + pid
	}
	return ""
}

func (st *extractState) ingestEnabledService(rawName string, data *structpb.Struct, ancestors []string) {
	if state := getStringField(data, "state"); state != "" && state != "ENABLED" {
		return
	}
	canon := CanonicalizeAIP122(rawName, st.projIdx)
	projID := extractProjectID(canon, ancestors, st.projIdx)
	svcName := shortResourceName(canon)
	if cfg := getStructField(data, "config"); cfg != nil {
		if n := getStringField(cfg, "name"); n != "" {
			svcName = n
		}
	}
	if projID == "" || svcName == "" {
		return
	}
	set, ok := st.enabledServices[projID]
	if !ok {
		set = make(map[string]bool)
		st.enabledServices[projID] = set
	}
	set[strings.ToLower(svcName)] = true
}

func (st *extractState) addFoldCount(parentURI, attrKey string) {
	if parentURI == "" || attrKey == "" {
		return
	}
	m, ok := st.foldedCounts[parentURI]
	if !ok {
		m = make(map[string]int)
		st.foldedCounts[parentURI] = m
	}
	m[attrKey]++
}

func (st *extractState) addFoldedExternalURI(dsURI, u string) {
	if u == "" {
		return
	}
	list := st.foldedExternalURIs[dsURI]
	appendUnique(&list, u)
	st.foldedExternalURIs[dsURI] = list
}

func (st *extractState) foldBigQueryChild(canonURI, countKey string, data *structpb.Struct) {
	dsURI := parentBigQueryDatasetURI(canonURI)
	if dsURI == "" {
		return
	}
	st.addFoldCount(dsURI, countKey)
	if data == nil {
		return
	}
	switch countKey {
	case "table_count":
		st.foldBigQueryExternalTable(dsURI, getStructField(data, "externalDataConfiguration"))
	case "routine_count":
		st.foldBigQueryRemoteConnEndpoint(dsURI, getStructField(data, "remoteFunctionOptions"))
	case "model_count":
		st.foldBigQueryRemoteConnEndpoint(dsURI, getStructField(data, "remoteModelInfo"))
	}
}

func (st *extractState) foldBigQueryExternalTable(dsURI string, ext *structpb.Struct) {
	if ext == nil {
		return
	}
	for _, u := range getStringListField(ext, "sourceUris") {
		st.addFoldedExternalURI(dsURI, CanonicalizeAIP122(SanitizeURL(u), st.projIdx))
	}
	if conn := getStringField(ext, "connectionId"); conn != "" {
		st.addFoldedExternalURI(dsURI, normalizeBigQueryConnectionURI(conn, st.projIdx))
	}
}

func (st *extractState) foldBigQueryRemoteConnEndpoint(dsURI string, remote *structpb.Struct) {
	if remote == nil {
		return
	}
	if conn := getStringField(remote, "connection"); conn != "" {
		st.addFoldedExternalURI(dsURI, normalizeBigQueryConnectionURI(conn, st.projIdx))
	}
	if ep := getStringField(remote, "endpoint"); ep != "" {
		st.addFoldedExternalURI(dsURI, CanonicalizeAIP122(SanitizeURL(ep), st.projIdx))
	}
}

func normalizeBigQueryConnectionURI(conn string, projIdx map[string]string) string {
	s := strings.TrimSpace(conn)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "/") {
		parts := strings.SplitN(s, ".", 3)
		if len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "" {
			return "//bigqueryconnection.googleapis.com/projects/" + mapProject(parts[0], projIdx) + segLocations + parts[1] + "/connections/" + parts[2]
		}
	}
	return CanonicalizeAIP122WithService(s, "bigqueryconnection.googleapis.com", projIdx)
}

func (st *extractState) foldCloudRunRevision(canonURI, projID string, data *structpb.Struct) {
	rev := &revisionSummary{uri: canonURI}
	if data != nil {
		spec := getStructField(data, "spec")
		st.extractCloudRunRevisionMeta(rev, canonURI, projID, data, getStructField(data, "metadata"))
		rev.serviceAccount = getStringField(data, "serviceAccount")
		if rev.serviceAccount == "" && spec != nil {
			rev.serviceAccount = getStringField(spec, "serviceAccountName")
		}
		st.extractCloudRunRevisionVPC(rev, getStructField(data, "vpcAccess"))
		extractContainerAndInitRefs(data, projID, st.projIdx, &rev.envRefs, &rev.secretRefs, &rev.cloudSQLInstances, nil)
		if spec != nil {
			extractContainerAndInitRefs(spec, projID, st.projIdx, &rev.envRefs, &rev.secretRefs, &rev.cloudSQLInstances, nil)
		}
	}
	st.revisions[canonURI] = rev
}

func buildCloudRunServiceURI(projID, location, svcName string) string {
	if projID == "" || location == "" || svcName == "" {
		return ""
	}
	return prefixRunProjects + projID + segLocations + location + segServices + svcName
}

func (st *extractState) extractCloudRunRevisionMeta(rev *revisionSummary, canonURI, projID string, data, meta *structpb.Struct) {
	loc := extractLocationFromURI(canonURI)
	if svcName := getStringField(getStructField(meta, "labels"), "serving.knative.dev/service"); svcName != "" {
		rev.parentURI = buildCloudRunServiceURI(projID, loc, svcName)
	}
	st.extractCloudRunRevisionAnnotations(rev, getStructField(meta, "annotations"))
	if rev.parentURI != "" {
		return
	}
	svc := getStringField(data, "service")
	if svc == "" {
		return
	}
	if strings.Contains(svc, "/") {
		rev.parentURI = CanonicalizeAIP122WithService(svc, svcRun, st.projIdx)
		return
	}
	rev.parentURI = buildCloudRunServiceURI(projID, loc, svc)
}

func (st *extractState) extractCloudRunRevisionAnnotations(rev *revisionSummary, ann *structpb.Struct) {
	if ann == nil {
		return
	}
	if conn := getStringField(ann, "run.googleapis.com/vpc-access-connector"); conn != "" {
		rev.vpcConnector = CanonicalizeAIP122WithService(conn, svcVPCAccess, st.projIdx)
	}
	for _, inst := range strings.Split(getStringField(ann, "run.googleapis.com/cloudsql-instances"), ",") {
		appendUnique(&rev.cloudSQLInstances, cloudSQLConnNameToURI(inst, st.projIdx))
	}
}

func (st *extractState) extractCloudRunRevisionVPC(rev *revisionSummary, vpc *structpb.Struct) {
	if vpc == nil {
		return
	}
	if conn := getStringField(vpc, "connector"); conn != "" {
		rev.vpcConnector = CanonicalizeAIP122WithService(conn, svcVPCAccess, st.projIdx)
	}
	for _, nicVal := range getListField(vpc, "networkInterfaces") {
		nic := nicVal.GetStructValue()
		if nic == nil {
			continue
		}
		if n := getStringField(nic, "network"); n != "" {
			rev.networkURI = CanonicalizeAIP122WithService(n, svcCompute, st.projIdx)
		}
		if s := getStringField(nic, "subnetwork"); s != "" {
			rev.subnetURI = CanonicalizeAIP122WithService(s, svcCompute, st.projIdx)
		}
		break
	}
}

func (st *extractState) foldAppEngineVersion(canonURI string, data *structpb.Struct) {
	rev := &revisionSummary{uri: canonURI}
	rev.parentURI = parentBeforeSegment(canonURI, "/versions/")
	if data != nil {
		rev.serviceAccount = getStringField(data, "serviceAccount")
		if name := getStringField(getStructField(data, "vpcAccessConnector"), "name"); name != "" {
			rev.vpcConnector = CanonicalizeAIP122WithService(name, svcVPCAccess, st.projIdx)
		}
		extractEnvMapRefs(getStructField(data, "envVariables"), st.projIdx, &rev.envRefs, &rev.secretRefs)
	}
	st.revisions[canonURI] = rev
}

func (st *extractState) ingestLBIntermediate(canonURI, assetType, projID, location string, data *structpb.Struct) {
	lb := &lbIntermediate{
		uri:       canonURI,
		assetType: assetType,
		projectID: projID,
		location:  location,
	}
	switch assetType {
	case "compute.googleapis.com/TargetHttpProxy", "compute.googleapis.com/TargetGrpcProxy":
		lb.urlMap = CanonicalizeAIP122WithService(getStringField(data, "urlMap"), svcCompute, st.projIdx)
	case "compute.googleapis.com/TargetHttpsProxy":
		lb.isTLS = true
		lb.urlMap = CanonicalizeAIP122WithService(getStringField(data, "urlMap"), svcCompute, st.projIdx)
	case "compute.googleapis.com/TargetTcpProxy":
		lb.service = CanonicalizeAIP122WithService(getStringField(data, "service"), svcCompute, st.projIdx)
	case "compute.googleapis.com/TargetSslProxy":
		lb.isTLS = true
		lb.service = CanonicalizeAIP122WithService(getStringField(data, "service"), svcCompute, st.projIdx)
	case "compute.googleapis.com/UrlMap":
		st.ingestURLMap(lb, data)
	case "compute.googleapis.com/BackendService":
		for _, beVal := range getListField(data, "backends") {
			if grp := getStringField(beVal.GetStructValue(), "group"); grp != "" {
				appendUnique(&lb.backendGroups, CanonicalizeAIP122WithService(grp, svcCompute, st.projIdx))
			}
		}
	case "compute.googleapis.com/BackendBucket":
		lb.bucketName = strings.TrimSpace(getStringField(data, "bucketName"))
	case "compute.googleapis.com/NetworkEndpointGroup":
		lb.negCloudRunSvc = getStringField(getStructField(data, "cloudRun"), "service")
		lb.negCloudFn = getStringField(getStructField(data, "cloudFunction"), "function")
		lb.negAppEngineSvc = getStringField(getStructField(data, "appEngine"), "service")
		if psc := getStringField(data, "pscTargetService"); psc != "" {
			lb.negPSCTarget = CanonicalizeAIP122WithService(psc, svcCompute, st.projIdx)
		}
	}
	st.lbAssets[canonURI] = lb
}

func (st *extractState) ingestURLMap(lb *lbIntermediate, data *structpb.Struct) {
	lb.defaultService = CanonicalizeAIP122WithService(getStringField(data, "defaultService"), svcCompute, st.projIdx)
	for _, hrVal := range getListField(data, "hostRules") {
		for _, h := range getStringListField(hrVal.GetStructValue(), "hosts") {
			if h != "*" {
				appendUnique(&lb.urlMapHosts, h)
			}
		}
	}
	for _, pmVal := range getListField(data, "pathMatchers") {
		if pm := pmVal.GetStructValue(); pm != nil {
			st.ingestURLMapPathMatcher(lb, pm)
		}
	}
}

func (st *extractState) ingestURLMapPathMatcher(lb *lbIntermediate, pm *structpb.Struct) {
	if ds := getStringField(pm, "defaultService"); ds != "" {
		lb.urlMapBackends = append(lb.urlMapBackends, urlMapBackendEntry{
			backendURI: CanonicalizeAIP122WithService(ds, svcCompute, st.projIdx),
		})
	}
	for _, prVal := range getListField(pm, "pathRules") {
		pr := prVal.GetStructValue()
		if svc := getStringField(pr, "service"); svc != "" {
			lb.urlMapBackends = append(lb.urlMapBackends, urlMapBackendEntry{
				backendURI: CanonicalizeAIP122WithService(svc, svcCompute, st.projIdx),
				paths:      getStringListField(pr, "paths"),
			})
		}
	}
	for _, rrVal := range getListField(pm, "routeRules") {
		if rr := rrVal.GetStructValue(); rr != nil {
			st.ingestURLMapRouteRule(lb, rr)
		}
	}
}

func (st *extractState) ingestURLMapRouteRule(lb *lbIntermediate, rr *structpb.Struct) {
	var paths []string
	for _, mrVal := range getListField(rr, "matchRules") {
		mr := mrVal.GetStructValue()
		if p := getStringField(mr, "prefixMatch"); p != "" {
			appendUnique(&paths, p)
		} else if p := getStringField(mr, "fullPathMatch"); p != "" {
			appendUnique(&paths, p)
		}
	}
	if svc := getStringField(rr, "service"); svc != "" {
		lb.urlMapBackends = append(lb.urlMapBackends, urlMapBackendEntry{
			backendURI: CanonicalizeAIP122WithService(svc, svcCompute, st.projIdx),
			paths:      paths,
		})
	}
	for _, wbVal := range getListField(getStructField(rr, "routeAction"), "weightedBackendServices") {
		if bs := getStringField(wbVal.GetStructValue(), "backendService"); bs != "" {
			lb.urlMapBackends = append(lb.urlMapBackends, urlMapBackendEntry{
				backendURI: CanonicalizeAIP122WithService(bs, svcCompute, st.projIdx),
				paths:      paths,
			})
		}
	}
}

func (st *extractState) ingestAbsorbedRecord(canonURI, assetType, projID, location string, data *structpb.Struct) {
	rec := &absorbedRecord{
		uri:       canonURI,
		assetType: assetType,
		projectID: projID,
		location:  location,
	}
	switch assetType {
	case "compute.googleapis.com/VpnTunnel":
		rec.router = CanonicalizeAIP122WithService(getStringField(data, "router"), svcCompute, st.projIdx)
		gw := getStringField(data, "vpnGateway")
		if gw == "" {
			gw = getStringField(data, "targetVpnGateway")
		}
		rec.vpnGateway = CanonicalizeAIP122WithService(gw, svcCompute, st.projIdx)
	case "networkconnectivity.googleapis.com/Spoke":
		st.populateNCCSpokeRecord(rec, data)
	case "datastream.googleapis.com/ConnectionProfile":
		st.populateDatastreamProfileRecord(rec, data)
	case "datastream.googleapis.com/PrivateConnection":
		if vpc := getStringField(getStructField(data, "vpcPeeringConfig"), "vpc"); vpc != "" {
			rec.vpc = CanonicalizeAIP122WithService(vpc, svcCompute, st.projIdx)
		}
	case "alloydb.googleapis.com/Instance":
		rec.parentURI = parentBeforeSegment(canonURI, segInstances)
		rec.privateIP = parsePrivateIPv4(getStringField(data, "ipAddress"))
	case "aiplatform.googleapis.com/FeatureView":
		rec.parentURI = parentBeforeSegment(canonURI, "/featureViews/")
		rec.bqSourceURI = getStringField(getStructField(data, "bigQuerySource"), "uri")
	case "analyticshub.googleapis.com/Listing":
		rec.parentURI = parentBeforeSegment(canonURI, "/listings/")
		rec.bqSourceURI = getStringField(getStructField(data, "bigqueryDataset"), "dataset")
	case "dataplex.googleapis.com/Asset":
		rec.parentURI = parentBeforeSegment(canonURI, "/zones/")
		rec.resourceSpecName = getStringField(getStructField(data, "resourceSpec"), "name")
	}
	st.absorbed[canonURI] = rec
}

func (st *extractState) populateNCCSpokeRecord(rec *absorbedRecord, data *structpb.Struct) {
	rec.hub = CanonicalizeAIP122WithService(getStringField(data, "hub"), "networkconnectivity.googleapis.com", st.projIdx)
	if rec.hub != "" {
		st.addFoldCount(rec.hub, "spoke_count")
	}
	if vpc := getStringField(getStructField(data, "linkedVpcNetwork"), "uri"); vpc != "" {
		rec.linkedVPC = CanonicalizeAIP122WithService(vpc, svcCompute, st.projIdx)
	}
	for _, u := range getStringListField(getStructField(data, "linkedVpnTunnels"), "uris") {
		rec.linkedVPNs = append(rec.linkedVPNs, CanonicalizeAIP122WithService(u, svcCompute, st.projIdx))
	}
	for _, u := range getStringListField(getStructField(data, "linkedInterconnectAttachments"), "uris") {
		rec.linkedInterconnects = append(rec.linkedInterconnects, CanonicalizeAIP122WithService(u, svcCompute, st.projIdx))
	}
}

func (st *extractState) populateDatastreamProfileRecord(rec *absorbedRecord, data *structpb.Struct) {
	rec.gcsBucket = getStringField(getStructField(data, "gcsProfile"), "bucket")
	if pc := getStringField(getStructField(data, "privateConnectivity"), "privateConnection"); pc != "" {
		rec.privateConnection = CanonicalizeAIP122WithService(pc, svcDatastream, st.projIdx)
	}
	for _, prof := range []string{"mysqlProfile", "postgresqlProfile", "oracleProfile", "sqlServerProfile"} {
		if h := strings.TrimSpace(getStringField(getStructField(data, prof), "hostname")); h != "" {
			appendUnique(&rec.hostnames, h)
		}
	}
}

func (rec *ingestedAsset) addRef(target, defaultSvc, direction, detail, kind string) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return
	}
	rec.outboundRefs = append(rec.outboundRefs, structuralRef{
		targetURI:  trimmed,
		defaultSvc: defaultSvc,
		direction:  direction,
		detail:     detail,
		kind:       kind,
	})
}

func (rec *ingestedAsset) addOutRef(target, defaultSvc, detail, kind string) {
	rec.addRef(target, defaultSvc, "out", detail, kind)
}

func (rec *ingestedAsset) addInRef(source, defaultSvc, detail, kind string) {
	rec.addRef(source, defaultSvc, "in", detail, kind)
}

type metadataExtractor func(*extractState, *ingestedAsset, *structpb.Struct)

var retainedMetadataExtractors = map[string]metadataExtractor{
	"compute.googleapis.com/Network":                     extractNetworkMetadata,
	assetTypeSubnetwork:                                  extractVPCParentMetadata,
	"compute.googleapis.com/Instance":                    extractComputeInstanceMetadata,
	"compute.googleapis.com/ForwardingRule":              extractForwardingRuleMetadata,
	"compute.googleapis.com/GlobalForwardingRule":        extractForwardingRuleMetadata,
	"compute.googleapis.com/Router":                      extractRouterMetadata,
	"compute.googleapis.com/InterconnectAttachment":      extractInterconnectMetadata,
	"compute.googleapis.com/ServiceAttachment":           extractServiceAttachmentMetadata,
	"container.googleapis.com/Cluster":                   extractGKEClusterMetadata,
	"sqladmin.googleapis.com/Instance":                   extractSQLInstanceMetadata,
	"alloydb.googleapis.com/Cluster":                     extractVPCParentMetadata,
	"redis.googleapis.com/Instance":                      extractRedisInstanceMetadata,
	"redis.googleapis.com/Cluster":                       extractRedisClusterMetadata,
	"memcache.googleapis.com/Instance":                   extractMemcacheMetadata,
	"file.googleapis.com/Instance":                       extractFilestoreMetadata,
	"cloudbuild.googleapis.com/BuildTrigger":             extractBuildTriggerMetadata,
	assetTypeRunService:                                  extractCloudRunServiceMetadata,
	"run.googleapis.com/Job":                             extractCloudRunJobMetadata,
	"cloudfunctions.googleapis.com/CloudFunction":        extractCloudFunctionMetadata,
	"appengine.googleapis.com/Service":                   extractAppEngineServiceMetadata,
	"pubsub.googleapis.com/Subscription":                 extractPubSubSubMetadata,
	"eventarc.googleapis.com/Trigger":                    extractEventarcTriggerMetadata,
	"cloudscheduler.googleapis.com/Job":                  extractSchedulerJobMetadata,
	"storagetransfer.googleapis.com/TransferJob":         extractStorageTransferMetadata,
	"bigquerydatatransfer.googleapis.com/TransferConfig": extractBQDTMetadata,
	"bigqueryconnection.googleapis.com/Connection":       extractBQConnectionMetadata,
	"datastream.googleapis.com/Stream":                   extractDatastreamStreamMetadata,
	"dataproc.googleapis.com/Cluster":                    extractDataprocClusterMetadata,
	"dataproc.googleapis.com/Batch":                      extractDataprocBatchMetadata,
	"metastore.googleapis.com/Service":                   extractMetastoreMetadata,
	"composer.googleapis.com/Environment":                extractComposerMetadata,
	"discoveryengine.googleapis.com/Engine":              extractDiscoveryEngineMetadata,
	"discoveryengine.googleapis.com/DataStore":           extractDiscoveryDataStoreMetadata,
	"dialogflow.googleapis.com/Agent":                    extractDialogflowMetadata,
	"aiplatform.googleapis.com/ReasoningEngine":          extractReasoningEngineMetadata,
	assetTypeServiceAccount:                              extractServiceAccountMetadata,
	"cloudkms.googleapis.com/CryptoKey":                  extractCryptoKeyMetadata,
}

func (st *extractState) extractRetainedMetadata(rec *ingestedAsset, data *structpb.Struct) {
	if data == nil {
		return
	}
	if kms := getAttrString(rec.attrs, "kms_key_uri"); kms != "" {
		rec.addOutRef(kms, svcCloudKMS, "cmek", "")
	}
	if fn, ok := retainedMetadataExtractors[rec.assetType]; ok {
		fn(st, rec, data)
	}
}

func extractNetworkMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	for _, pVal := range getListField(data, "peerings") {
		if p := pVal.GetStructValue(); p != nil {
			rec.vpcPeerings = append(rec.vpcPeerings, vpcPeeringInfo{
				name:    getStringField(p, "name"),
				network: getStringField(p, "network"),
				state:   getStringField(p, "state"),
			})
		}
	}
}

func extractVPCParentMetadata(_ *extractState, rec *ingestedAsset, _ *structpb.Struct) {
	if vpc := getAttrString(rec.attrs, "vpc"); vpc != "" {
		rec.parentURI = vpc
	}
}

func extractComputeInstanceMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	// Preserve only non-sensitive short structural VM metadata keys (cluster-name, created-by).
	for _, itVal := range getListField(getStructField(data, "metadata"), "items") {
		it := itVal.GetStructValue()
		k := getStringField(it, "key")
		if k == "cluster-name" || k == "created-by" {
			if rec.computeMetadata == nil {
				rec.computeMetadata = make(map[string]string)
			}
			rec.computeMetadata[k] = getStringField(it, "value")
		}
	}
	for _, nicVal := range getListField(data, "networkInterfaces") {
		if ip := parsePrivateIPv4(getStringField(nicVal.GetStructValue(), "networkIP")); ip != "" {
			appendUnique(&rec.privateIPs, ip)
		}
	}
	if sub := getAttrString(rec.attrs, "subnet"); strings.HasPrefix(sub, "//") {
		rec.parentURI = sub
	} else if vpc := getAttrString(rec.attrs, "vpc"); strings.HasPrefix(vpc, "//") {
		rec.parentURI = vpc
	}
}

func extractForwardingRuleMetadata(st *extractState, rec *ingestedAsset, data *structpb.Struct) {
	if tgt := getStringField(data, "target"); tgt != "" {
		rec.frTarget = CanonicalizeAIP122WithService(tgt, svcCompute, st.projIdx)
	}
	if bs := getStringField(data, "backendService"); bs != "" {
		rec.frBackendSvc = CanonicalizeAIP122WithService(bs, svcCompute, st.projIdx)
	}
	if ip := parsePrivateIPv4(getStringField(data, "IPAddress")); ip != "" {
		scheme := strings.ToUpper(getStringField(data, "loadBalancingScheme"))
		if scheme == "" || strings.HasPrefix(scheme, "INTERNAL") {
			appendUnique(&rec.privateIPs, ip)
		}
	}
}

func extractRouterMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	for _, ifVal := range getListField(data, "interfaces") {
		iface := ifVal.GetStructValue()
		rec.addOutRef(getStringField(iface, "linkedVpnTunnel"), svcCompute, "router.vpn", "")
		rec.addOutRef(getStringField(iface, "linkedInterconnectAttachment"), svcCompute, "router.interconnect", "")
	}
	if vpc := getAttrString(rec.attrs, "vpc"); vpc != "" {
		rec.parentURI = vpc
	}
}

func extractInterconnectMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	rec.addInRef(getStringField(data, "router"), svcCompute, "interconnect.router", "")
}

func extractServiceAttachmentMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	rec.addOutRef(getStringField(data, "targetService"), svcCompute, "psc.target", "")
}

func extractGKEClusterMetadata(st *extractState, rec *ingestedAsset, data *structpb.Struct) {
	if sub := getAttrString(rec.attrs, "subnet"); strings.HasPrefix(sub, "//") {
		rec.parentURI = sub
	} else if vpc := getAttrString(rec.attrs, "vpc"); strings.HasPrefix(vpc, "//") {
		rec.parentURI = vpc
	}
	addIGURLs := func(src *structpb.Struct) {
		for _, ig := range getStringListField(src, "instanceGroupUrls") {
			list := st.gke.clusterIGURLs[rec.uri]
			appendUnique(&list, CanonicalizeAIP122WithService(ig, svcCompute, st.projIdx))
			st.gke.clusterIGURLs[rec.uri] = list
		}
	}
	addIGURLs(data)
	for _, npVal := range getListField(data, "nodePools") {
		addIGURLs(npVal.GetStructValue())
	}
}

func extractSQLInstanceMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	if vpc := getAttrString(rec.attrs, "vpc"); vpc != "" {
		rec.parentURI = vpc
	}
	rec.addOutRef(getStringField(data, "masterInstanceName"), "sqladmin.googleapis.com", "replica_of", "cloudsql")
	for _, ipVal := range getListField(data, "ipAddresses") {
		m := ipVal.GetStructValue()
		typ := strings.ToUpper(getStringField(m, "type"))
		if typ != "" && typ != "PRIVATE" {
			continue
		}
		if ip := parsePrivateIPv4(getStringField(m, "ipAddress")); ip != "" {
			appendUnique(&rec.privateIPs, ip)
		}
	}
}

func extractRedisInstanceMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	if vpc := getAttrString(rec.attrs, "vpc"); vpc != "" {
		rec.parentURI = vpc
	}
	if ip := parsePrivateIPv4(getStringField(data, "host")); ip != "" {
		appendUnique(&rec.privateIPs, ip)
	}
	if ip := parsePrivateIPv4(getStringField(data, "readEndpoint")); ip != "" {
		appendUnique(&rec.privateIPs, ip)
	}
}

func extractRedisClusterMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	if vpc := getAttrString(rec.attrs, "vpc"); vpc != "" {
		rec.parentURI = vpc
	}
	for _, epVal := range getListField(data, "discoveryEndpoints") {
		if ip := parsePrivateIPv4(getStringField(epVal.GetStructValue(), "address")); ip != "" {
			appendUnique(&rec.privateIPs, ip)
		}
	}
}

func extractMemcacheMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	if vpc := getAttrString(rec.attrs, "vpc"); vpc != "" {
		rec.parentURI = vpc
	}
	if ip := parsePrivateIPv4(getStringField(data, "discoveryEndpoint")); ip != "" {
		appendUnique(&rec.privateIPs, ip)
	}
	for _, nVal := range getListField(data, "memcacheNodes") {
		if ip := parsePrivateIPv4(getStringField(nVal.GetStructValue(), "host")); ip != "" {
			appendUnique(&rec.privateIPs, ip)
		}
	}
}

func extractFilestoreMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	if vpc := getAttrString(rec.attrs, "vpc"); vpc != "" {
		rec.parentURI = vpc
	}
	for _, nVal := range getListField(data, "networks") {
		for _, rawIP := range getStringListField(nVal.GetStructValue(), "ipAddresses") {
			if ip := parsePrivateIPv4(rawIP); ip != "" {
				appendUnique(&rec.privateIPs, ip)
			}
		}
	}
}

func extractBuildTriggerMetadata(st *extractState, rec *ingestedAsset, data *structpb.Struct) {
	rec.cbTriggerID = strings.TrimSpace(getStringField(data, "id"))
	if rec.cbTriggerID == "" {
		rec.cbTriggerID = shortResourceName(rec.uri)
	}
	st.cbTriggers[cbHeapKey{projectID: rec.projectID, triggerID: rec.cbTriggerID}] = rec.uri
	extractCloudBuildTriggerStruct(rec, data, st.projIdx)
}

func extractCloudRunServiceMetadata(st *extractState, rec *ingestedAsset, data *structpb.Struct) {
	urls := append(
		[]string{getStringField(getStructField(data, "status"), "url"), getStringField(data, "uri")},
		getStringListField(data, "urls")...,
	)
	for _, u := range urls {
		if su := SanitizeURL(u); su != "" {
			appendUnique(&rec.runURLs, su)
		}
	}
	rec.runLatestReady = getStringField(getStructField(data, "status"), "latestReadyRevisionName")
	if rec.runLatestReady == "" {
		rec.runLatestReady = getStringField(data, "latestReadyRevision")
	}
	rec.runTraffic = extractCloudRunTraffic(data)
	extractCloudRunServiceRefs(rec, data, st.projIdx)
}

func extractCloudRunTraffic(data *structpb.Struct) []runTrafficEntry {
	var trafficVals []*structpb.Value
	switch {
	case len(getListField(getStructField(data, "status"), "traffic")) > 0:
		trafficVals = getListField(getStructField(data, "status"), "traffic")
	case len(getListField(data, "trafficStatuses")) > 0:
		trafficVals = getListField(data, "trafficStatuses")
	default:
		trafficVals = getListField(data, "traffic")
	}
	var out []runTrafficEntry
	for _, tv := range trafficVals {
		tm := tv.GetStructValue()
		pct := getIntField(tm, "percent")
		if tm == nil || pct <= 0 {
			continue
		}
		rName := getStringField(tm, "revisionName")
		if rName == "" {
			rName = getStringField(tm, "revision")
		}
		latest := getBoolValue(tm, "latestRevision") || getStringField(tm, "type") == "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
		out = append(out, runTrafficEntry{revisionName: rName, percent: pct, latest: latest})
	}
	return out
}

func extractCloudRunServiceRefs(rec *ingestedAsset, data *structpb.Struct, projIdx map[string]string) {
	var csql []string
	extractContainerAndInitRefs(getStructField(data, "template"), rec.projectID, projIdx, &rec.envRefs, &rec.secretRefs, &csql, nil)
	if specTpl := getStructField(getStructField(data, "spec"), "template"); specTpl != nil {
		extractContainerAndInitRefs(getStructField(specTpl, "spec"), rec.projectID, projIdx, &rec.envRefs, &rec.secretRefs, &csql, nil)
		if ann := getStructField(getStructField(specTpl, "metadata"), "annotations"); ann != nil {
			for _, inst := range strings.Split(getStringField(ann, "run.googleapis.com/cloudsql-instances"), ",") {
				if uri := cloudSQLConnNameToURI(inst, projIdx); uri != "" {
					appendUnique(&csql, uri)
				}
			}
			rec.addOutRef(getStringField(ann, "run.googleapis.com/vpc-access-connector"), svcVPCAccess, "vpc_connector", "")
		}
	}
	for _, u := range csql {
		appendUnique(&rec.envRefs, u)
	}
	rec.addOutRef(getAttrString(rec.attrs, "vpc_connector"), svcVPCAccess, "vpc_connector", "")
}

func extractCloudRunJobMetadata(st *extractState, rec *ingestedAsset, data *structpb.Struct) {
	var csql []string
	extractContainerAndInitRefs(getStructField(getStructField(data, "template"), "template"), rec.projectID, st.projIdx, &rec.envRefs, &rec.secretRefs, &csql, nil)
	for _, u := range csql {
		appendUnique(&rec.envRefs, u)
	}
	rec.addOutRef(getAttrString(rec.attrs, "vpc_connector"), svcVPCAccess, "vpc_connector", "")
}

func extractCloudFunctionMetadata(st *extractState, rec *ingestedAsset, data *structpb.Struct) {
	if u := SanitizeURL(getStringField(getStructField(data, "serviceConfig"), "uri")); u != "" {
		appendUnique(&rec.runURLs, u)
	}
	if u := SanitizeURL(getStringField(getStructField(data, "httpsTrigger"), "url")); u != "" {
		appendUnique(&rec.runURLs, u)
	}
	rec.addInRef(getStringField(getStructField(data, "eventTrigger"), "pubsubTopic"), svcPubSub, "event_trigger", "")
	if sc := getStructField(data, "serviceConfig"); sc != nil {
		extractEnvMapRefs(getStructField(sc, "environmentVariables"), st.projIdx, &rec.envRefs, &rec.secretRefs)
		for _, field := range []string{"secretEnvironmentVariables", "secretVolumes"} {
			for _, item := range getListField(sc, field) {
				if sec := getStringField(item.GetStructValue(), "secret"); sec != "" {
					appendUnique(&rec.secretRefs, normalizeSecretRef(sec, rec.projectID, st.projIdx))
				}
			}
		}
		rec.addOutRef(getStringField(sc, "vpcConnector"), svcVPCAccess, "vpc_connector", "")
	}
	extractEnvMapRefs(getStructField(data, "environmentVariables"), st.projIdx, &rec.envRefs, &rec.secretRefs)
}

func extractAppEngineServiceMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	allocs := getStructField(getStructField(data, "split"), "allocations")
	if allocs == nil {
		return
	}
	rec.aeSplit = make(map[string]float64, len(allocs.GetFields()))
	for verID, v := range allocs.GetFields() {
		if frac := v.GetNumberValue(); frac > 0 {
			rec.aeSplit[verID] = frac
		}
	}
}

func extractPubSubSubMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	if topic := getStringField(data, "topic"); topic != "" && topic != "_deleted-topic_" {
		rec.addInRef(topic, svcPubSub, "subscription", "")
	}
	rec.addOutRef(SanitizeURL(getStringField(getStructField(data, "pushConfig"), "pushEndpoint")), "", "pubsub.push", "http")
	rec.addOutRef(getStringField(getStructField(data, "bigqueryConfig"), "table"), svcBigQuery, "pubsub.bigquery_sink", "bq_dataset")
	rec.addOutRef(getStringField(getStructField(data, "cloudStorageConfig"), "bucket"), svcStorage, "pubsub.gcs_sink", "bucket")
	rec.addOutRef(getStringField(getStructField(data, "deadLetterPolicy"), "deadLetterTopic"), svcPubSub, "dead_letter", "")
}

func extractEventarcTriggerMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	rec.addInRef(getStringField(getStructField(getStructField(data, "transport"), "pubsub"), "topic"), svcPubSub, "eventarc.transport", "")
	for _, efVal := range getListField(data, "eventFilters") {
		if ef := efVal.GetStructValue(); ef != nil && getStringField(ef, "attribute") == "bucket" {
			rec.addInRef(getStringField(ef, "value"), svcStorage, "eventarc.bucket", "bucket")
		}
	}
	dst := getStructField(data, "destination")
	if dst == nil {
		return
	}
	if cr := getStructField(dst, "cloudRun"); cr != nil {
		if svc := getStringField(cr, "service"); svc != "" {
			reg := getStringField(cr, "region")
			if reg == "" {
				reg = rec.location
			}
			if !strings.Contains(svc, "/") && rec.projectID != "" && reg != "" {
				svc = prefixRunProjects + rec.projectID + segLocations + reg + segServices + svc
			}
			rec.addOutRef(svc, svcRun, detailEventarcDest, "")
		}
	}
	rec.addOutRef(getStringField(dst, "cloudFunction"), "cloudfunctions.googleapis.com", detailEventarcDest, "")
	rec.addOutRef(getStringField(dst, "workflow"), "workflows.googleapis.com", detailEventarcDest, "")
	rec.addOutRef(getStringField(getStructField(dst, "gke"), "cluster"), "container.googleapis.com", detailEventarcDest, "")
}

func extractSchedulerJobMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	rec.addOutRef(getStringField(getStructField(data, "pubsubTarget"), "topicName"), svcPubSub, "scheduler.pubsub", "")
	rec.addOutRef(SanitizeURL(getStringField(getStructField(data, "httpTarget"), "uri")), "", "scheduler.http", "http")
}

func extractStorageTransferMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	ts := getStructField(data, "transferSpec")
	rec.addInRef(getStringField(getStructField(ts, "gcsDataSource"), "bucketName"), svcStorage, "transfer.source", "bucket")
	rec.addOutRef(getStringField(getStructField(ts, "gcsDataSink"), "bucketName"), svcStorage, "transfer.sink", "bucket")
	rec.addOutRef(getStringField(getStructField(data, "notificationConfig"), "pubsubTopic"), svcPubSub, "transfer.notification", "")
}

func extractBQDTMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	rec.addOutRef(getStringField(data, "destinationDatasetId"), svcBigQuery, "bqdt.destination", "bq_dataset")
	rec.addOutRef(getStringField(data, "notificationPubsubTopic"), svcPubSub, "bqdt.notification", "")
}

func extractBQConnectionMetadata(st *extractState, rec *ingestedAsset, data *structpb.Struct) {
	rec.addOutRef(getStringField(getStructField(data, "cloudSql"), "instanceId"), "sqladmin.googleapis.com", "bqconnection.cloudsql", "cloudsql")
	if spDB := getStringField(getStructField(data, "cloudSpanner"), "database"); spDB != "" {
		spURI := CanonicalizeAIP122WithService(spDB, "spanner.googleapis.com", st.projIdx)
		if parent := parentBeforeSegment(spURI, "/databases/"); parent != "" {
			spURI = parent
		}
		rec.addOutRef(spURI, "spanner.googleapis.com", "bqconnection.spanner", "")
	}
}

func extractDatastreamStreamMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	rec.addInRef(getStringField(getStructField(data, "sourceConfig"), "sourceConnectionProfile"), svcDatastream, "datastream.source", "")
	rec.addOutRef(getStringField(getStructField(data, "destinationConfig"), "destinationConnectionProfile"), svcDatastream, "datastream.dest", "")
	bqDS := getStringField(getStructField(getStructField(getStructField(data, "destinationConfig"), "bigqueryDestinationConfig"), "singleTargetDataset"), "datasetId")
	rec.addOutRef(bqDS, svcBigQuery, "datastream.bigquery", "bq_dataset")
}

func extractDataprocClusterMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	cfg := getStructField(data, "config")
	rec.addOutRef(getStringField(cfg, "configBucket"), svcStorage, "dataproc.config_bucket", "bucket")
	rec.addOutRef(getStringField(cfg, "tempBucket"), svcStorage, "dataproc.temp_bucket", "bucket")
	rec.addOutRef(getStringField(getStructField(cfg, "metastoreConfig"), "dataprocMetastoreService"), "metastore.googleapis.com", "dataproc.metastore", "")
}

func extractDataprocBatchMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	ms := getStringField(getStructField(getStructField(data, "environmentConfig"), "peripheralsConfig"), "metastoreService")
	rec.addOutRef(ms, "metastore.googleapis.com", "dataproc.metastore", "")
}

func extractMetastoreMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	rec.addOutRef(getStringField(data, "artifactGcsUri"), svcStorage, "metastore.artifacts", "bucket")
}

func extractComposerMetadata(st *extractState, rec *ingestedAsset, data *structpb.Struct) {
	cfg := getStructField(data, "config")
	rec.addOutRef(getStringField(cfg, "gkeCluster"), "container.googleapis.com", "composer.gke", "")
	rec.addOutRef(getStringField(cfg, "dagGcsPrefix"), svcStorage, "composer.dags", "bucket")
	extractEnvMapRefs(getStructField(getStructField(cfg, "softwareConfig"), "envVariables"), st.projIdx, &rec.envRefs, &rec.secretRefs)
}

func extractDiscoveryEngineMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	for _, dsID := range getStringListField(data, "dataStoreIds") {
		rec.addOutRef(dsID, "discoveryengine.googleapis.com", "discovery.datastore", "datastore_id")
	}
}

func extractDiscoveryDataStoreMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	for _, u := range getStringListField(getStructField(data, "gcsSource"), "inputUris") {
		rec.addInRef(u, svcStorage, "discovery.source", "bucket")
	}
	if bq := getStructField(data, "bigquerySource"); bq != nil {
		if ds := getStringField(bq, "datasetId"); ds != "" {
			if p := getStringField(bq, "projectId"); p != "" {
				ds = p + "." + ds
			}
			rec.addInRef(ds, svcBigQuery, "discovery.source", "bq_dataset")
		}
	}
}

func extractDialogflowMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	for _, whVal := range getListField(data, "webhooks") {
		u := getStringField(getStructField(whVal.GetStructValue(), "genericWebService"), "uri")
		rec.addOutRef(SanitizeURL(u), "", "dialogflow.webhook", "http")
	}
}

func extractReasoningEngineMetadata(st *extractState, rec *ingestedAsset, data *structpb.Struct) {
	depSpec := getStructField(getStructField(data, "spec"), "deploymentSpec")
	if depSpec == nil {
		return
	}
	for _, eVal := range getListField(depSpec, "env") {
		addExtractedRefs(getStringField(eVal.GetStructValue(), "value"), st.projIdx, &rec.envRefs, &rec.secretRefs)
	}
	for _, seVal := range getListField(depSpec, "secretEnv") {
		if sec := getStringField(getStructField(seVal.GetStructValue(), "secretRef"), "secret"); sec != "" {
			appendUnique(&rec.secretRefs, normalizeSecretRef(sec, rec.projectID, st.projIdx))
		}
	}
}

func extractServiceAccountMetadata(_ *extractState, rec *ingestedAsset, data *structpb.Struct) {
	rec.saEmail = strings.ToLower(strings.TrimSpace(getStringField(data, "email")))
	if rec.saEmail == "" {
		rec.saEmail = strings.ToLower(shortResourceName(rec.uri))
	}
	rec.saUniqueID = strings.TrimSpace(getStringField(data, "uniqueId"))
	if rec.saUniqueID == "" {
		rec.saUniqueID = strings.TrimSpace(getStringField(data, "oauth2ClientId"))
	}
}

func extractCryptoKeyMetadata(st *extractState, rec *ingestedAsset, _ *structpb.Struct) {
	st.addFoldCount(parentBeforeSegment(rec.uri, "/cryptoKeys/"), "key_count")
}

func extractCloudBuildTriggerStruct(rec *ingestedAsset, data *structpb.Struct, projIdx map[string]string) {
	_, rec.cbTriggerFilter = extractCloudBuildTriggerRepo(data)
	switch {
	case getStructField(data, "github") != nil:
		gh := getStructField(data, "github")
		rec.cbRepoHost = hostGitHub
		rec.cbRepoOwner = strings.TrimSpace(getStringField(gh, "owner"))
		rec.cbRepoName = strings.TrimSpace(getStringField(gh, "name"))
	case getStructField(data, "sourceToBuild") != nil:
		s2b := getStructField(data, "sourceToBuild")
		rec.cbRepoHost, rec.cbRepoOwner, rec.cbRepoName = parseGitRepoURL(getStringField(s2b, "uri"))
	case getStructField(data, "triggerTemplate") != nil:
		// Mirror convention: github_<owner>_<repo>
		repoName := strings.TrimSpace(getStringField(getStructField(data, "triggerTemplate"), "repoName"))
		if strings.HasPrefix(repoName, "github_") {
			parts := strings.SplitN(strings.TrimPrefix(repoName, "github_"), "_", 2)
			if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
				rec.cbRepoHost, rec.cbRepoOwner, rec.cbRepoName = hostGitHub, parts[0], parts[1]
			}
		}
	case getStructField(data, "repositoryEventConfig") != nil:
		r := getStructField(data, "repositoryEventConfig")
		if repoPath := getStringField(r, "repository"); repoPath != "" {
			if host, owner, repo := parseGitRepoURL(getStringField(r, "remoteUri")); owner != "" {
				rec.cbRepoHost, rec.cbRepoOwner, rec.cbRepoName = host, owner, repo
			} else if seg := shortResourceName(repoPath); seg != "" {
				rec.cbRepoHost, rec.cbRepoOwner, rec.cbRepoName = hostGitHub, "repo", seg
			}
		}
	}
	if build := getStructField(data, "build"); build != nil {
		rec.cbInlineRefs, rec.cbInlineSecs = extractCloudBuildPayloadRefs(build, projIdx)
	}
}

func parseGitRepoURL(raw string) (host, owner, repo string) {
	s := SanitizeURL(raw)
	s = stripHTTPURLScheme(s)
	s = strings.TrimSuffix(s, ".git")
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 3 {
		return "", "", ""
	}
	h := strings.ToLower(parts[0])
	if h != hostGitHub && h != "gitlab.com" {
		return "", "", ""
	}
	return h, parts[1], parts[2]
}

func extractDisplayName(canonURI, assetType string, data *structpb.Struct) string {
	if isRetainedK8sAssetType(assetType) {
		return extractK8sName(canonURI, data)
	}
	if dn := strings.TrimSpace(getStringField(data, "displayName")); dn != "" {
		return dn
	}
	if n := strings.TrimSpace(getStringField(data, "name")); n != "" {
		if strings.Contains(n, "/") {
			return shortResourceName(n)
		}
		return n
	}
	return shortResourceName(canonURI)
}

func extractAssetLabels(data *structpb.Struct) map[string]string {
	if data == nil {
		return nil
	}
	if l := extractStringMap(getStructField(data, "labels")); len(l) > 0 {
		return l
	}
	if l := extractStringMap(getStructField(getStructField(data, "metadata"), "labels")); len(l) > 0 {
		return l
	}
	if l := extractStringMap(getStructField(data, "resourceLabels")); len(l) > 0 {
		return l
	}
	return extractStringMap(getStructField(getStructField(data, "settings"), "userLabels"))
}

func (st *extractState) reconcilePostCAI() {
	st.rekeyProjectsAndServices()
	st.rekeyFoldedMaps()
	st.rekeyIntermediates()
	st.gke.rekey(st.projIdx)
	st.rekeyCloudBuild()
	st.rekeyAndDedupAssets()
	st.indexServiceAccountsAndFoldedCounts()
	for _, a := range st.assets {
		switch a.assetType {
		case assetTypeRunService:
			st.reconcileCloudRunService(a)
		case "appengine.googleapis.com/Service":
			st.reconcileAppEngineService(a)
		}
	}
	for _, b := range st.iamBindings {
		b.resourceURI = CanonicalizeAIP122(b.resourceURI, st.projIdx)
		b.projectID = mapProject(b.projectID, st.projIdx)
	}
}

func (st *extractState) rekeyProjectsAndServices() {
	rekeyedProjects := make(map[string]ProjectNode, len(st.projects))
	for _, k := range sortedMapKeys(st.projects) {
		p := st.projects[k]
		canonID := mapProject(k, st.projIdx)
		if p.ProjectID != "" {
			canonID = mapProject(p.ProjectID, st.projIdx)
		}
		if canonID == "" {
			canonID = k
		}
		p.ProjectID = canonID
		if p.DisplayName == "" || isNumeric(p.DisplayName) {
			p.DisplayName = canonID
		}
		if existing, ok := rekeyedProjects[canonID]; ok {
			p = mergeProjectNodes(p, existing)
		}
		rekeyedProjects[canonID] = p
		st.inScopeProjects[canonID] = true
	}
	st.projects = rekeyedProjects

	rekeyedSvcs := make(map[string]map[string]bool, len(st.enabledServices))
	for proj, set := range st.enabledServices {
		mp := mapProject(proj, st.projIdx)
		dst, ok := rekeyedSvcs[mp]
		if !ok {
			dst = make(map[string]bool, len(set))
			rekeyedSvcs[mp] = dst
		}
		for s := range set {
			dst[s] = true
		}
	}
	st.enabledServices = rekeyedSvcs
}

func mergeProjectNodes(p, existing ProjectNode) ProjectNode {
	if p.ProjectNumber == "" {
		p.ProjectNumber = existing.ProjectNumber
	}
	if p.Parent == "" {
		p.Parent = existing.Parent
	}
	if p.SharedVPCHostProject == "" {
		p.SharedVPCHostProject = existing.SharedVPCHostProject
	}
	p.IsPSATenant = p.IsPSATenant || existing.IsPSATenant
	return p
}

func (st *extractState) rekeyFoldedMaps() {
	rekeyedFold := make(map[string]map[string]int, len(st.foldedCounts))
	for rawKey, counts := range st.foldedCounts {
		ck := CanonicalizeAIP122(rawKey, st.projIdx)
		dst, ok := rekeyedFold[ck]
		if !ok {
			dst = make(map[string]int, len(counts))
			rekeyedFold[ck] = dst
		}
		for k, v := range counts {
			dst[k] += v
		}
	}
	st.foldedCounts = rekeyedFold

	rekeyedExt := make(map[string][]string, len(st.foldedExternalURIs))
	for rawKey, uris := range st.foldedExternalURIs {
		ck := CanonicalizeAIP122(rawKey, st.projIdx)
		for _, u := range uris {
			list := rekeyedExt[ck]
			appendUnique(&list, CanonicalizeAIP122(u, st.projIdx))
			rekeyedExt[ck] = list
		}
	}
	st.foldedExternalURIs = rekeyedExt
}

func (st *extractState) rekeyIntermediates() {
	rekeyedLB := make(map[string]*lbIntermediate, len(st.lbAssets))
	for _, lb := range st.lbAssets {
		lb.uri = CanonicalizeAIP122(lb.uri, st.projIdx)
		lb.projectID = mapProject(lb.projectID, st.projIdx)
		lb.urlMap = CanonicalizeAIP122WithService(lb.urlMap, svcCompute, st.projIdx)
		lb.service = CanonicalizeAIP122WithService(lb.service, svcCompute, st.projIdx)
		lb.defaultService = CanonicalizeAIP122WithService(lb.defaultService, svcCompute, st.projIdx)
		for i := range lb.urlMapBackends {
			lb.urlMapBackends[i].backendURI = CanonicalizeAIP122WithService(lb.urlMapBackends[i].backendURI, svcCompute, st.projIdx)
		}
		for i := range lb.backendGroups {
			lb.backendGroups[i] = CanonicalizeAIP122WithService(lb.backendGroups[i], svcCompute, st.projIdx)
		}
		lb.negPSCTarget = CanonicalizeAIP122WithService(lb.negPSCTarget, svcCompute, st.projIdx)
		rekeyedLB[lb.uri] = lb
	}
	st.lbAssets = rekeyedLB

	rekeyedAbs := make(map[string]*absorbedRecord, len(st.absorbed))
	for _, ab := range st.absorbed {
		ab.uri = CanonicalizeAIP122(ab.uri, st.projIdx)
		ab.projectID = mapProject(ab.projectID, st.projIdx)
		ab.router = CanonicalizeAIP122WithService(ab.router, svcCompute, st.projIdx)
		ab.vpnGateway = CanonicalizeAIP122WithService(ab.vpnGateway, svcCompute, st.projIdx)
		ab.hub = CanonicalizeAIP122WithService(ab.hub, "networkconnectivity.googleapis.com", st.projIdx)
		ab.linkedVPC = CanonicalizeAIP122WithService(ab.linkedVPC, svcCompute, st.projIdx)
		for i := range ab.linkedVPNs {
			ab.linkedVPNs[i] = CanonicalizeAIP122WithService(ab.linkedVPNs[i], svcCompute, st.projIdx)
		}
		for i := range ab.linkedInterconnects {
			ab.linkedInterconnects[i] = CanonicalizeAIP122WithService(ab.linkedInterconnects[i], svcCompute, st.projIdx)
		}
		ab.privateConnection = CanonicalizeAIP122WithService(ab.privateConnection, svcDatastream, st.projIdx)
		ab.vpc = CanonicalizeAIP122WithService(ab.vpc, svcCompute, st.projIdx)
		ab.parentURI = CanonicalizeAIP122(ab.parentURI, st.projIdx)
		rekeyedAbs[ab.uri] = ab
	}
	st.absorbed = rekeyedAbs

	rekeyedRevs := make(map[string]*revisionSummary, len(st.revisions))
	for _, rev := range st.revisions {
		rev.uri = CanonicalizeAIP122(rev.uri, st.projIdx)
		rev.parentURI = CanonicalizeAIP122(rev.parentURI, st.projIdx)
		rev.vpcConnector = CanonicalizeAIP122WithService(rev.vpcConnector, svcVPCAccess, st.projIdx)
		rev.networkURI = CanonicalizeAIP122WithService(rev.networkURI, svcCompute, st.projIdx)
		rev.subnetURI = CanonicalizeAIP122WithService(rev.subnetURI, svcCompute, st.projIdx)
		rev.envRefs = canonicalizeSlice(rev.envRefs, st.projIdx)
		rev.secretRefs = canonicalizeSlice(rev.secretRefs, st.projIdx)
		rev.cloudSQLInstances = canonicalizeSlice(rev.cloudSQLInstances, st.projIdx)
		rekeyedRevs[rev.uri] = rev
	}
	st.revisions = rekeyedRevs
}

func (st *extractState) rekeyCloudBuild() {
	rekeyedTriggers := make(map[cbHeapKey]string, len(st.cbTriggers))
	for k, uri := range st.cbTriggers {
		k.projectID = mapProject(k.projectID, st.projIdx)
		rekeyedTriggers[k] = CanonicalizeAIP122(uri, st.projIdx)
	}
	st.cbTriggers = rekeyedTriggers

	rekeyedHeaps := make(map[cbHeapKey]*cbBuildMinHeap, len(st.cbHeaps))
	for k, h := range st.cbHeaps {
		k.projectID = mapProject(k.projectID, st.projIdx)
		dst, ok := rekeyedHeaps[k]
		if !ok {
			dst = &cbBuildMinHeap{}
			heap.Init(dst)
			rekeyedHeaps[k] = dst
		}
		for _, item := range *h {
			item.uri = CanonicalizeAIP122(item.uri, st.projIdx)
			item.refs = canonicalizeSlice(item.refs, st.projIdx)
			item.secretRefs = canonicalizeSlice(item.secretRefs, st.projIdx)
			heap.Push(dst, item)
		}
		limit := st.cloudBuildRuns
		if k.triggerID == "" {
			limit = maxTriggerlessCloudBuildRuns
		}
		for dst.Len() > limit {
			heap.Pop(dst)
		}
	}
	st.cbHeaps = rekeyedHeaps
}

func (st *extractState) rekeyAndDedupAssets() {
	st.assetsByURI = make(map[string]*ingestedAsset, len(st.assets))
	for _, a := range st.assets {
		a.uri = CanonicalizeAIP122(a.uri, st.projIdx)
		a.projectID = mapProject(a.projectID, st.projIdx)
		a.parentURI = CanonicalizeAIP122(a.parentURI, st.projIdx)
		a.frTarget = CanonicalizeAIP122WithService(a.frTarget, svcCompute, st.projIdx)
		a.frBackendSvc = CanonicalizeAIP122WithService(a.frBackendSvc, svcCompute, st.projIdx)
		a.envRefs = canonicalizeSlice(a.envRefs, st.projIdx)
		a.secretRefs = canonicalizeSlice(a.secretRefs, st.projIdx)
		a.cbInlineRefs = canonicalizeSlice(a.cbInlineRefs, st.projIdx)
		a.cbInlineSecs = canonicalizeSlice(a.cbInlineSecs, st.projIdx)
		recanonicalizeAttrURIs(a.attrs, st.projIdx)
		st.assetsByURI[a.uri] = a
	}

	deduped := make([]*ingestedAsset, 0, len(st.assetsByURI))
	for _, a := range st.assetsByURI {
		deduped = append(deduped, a)
	}
	sort.Slice(deduped, func(i, j int) bool {
		if deduped[i].uri != deduped[j].uri {
			return deduped[i].uri < deduped[j].uri
		}
		return deduped[i].assetType < deduped[j].assetType
	})
	st.assets = deduped
}

func (st *extractState) indexServiceAccountsAndFoldedCounts() {
	for _, a := range st.assets {
		if a.assetType == assetTypeServiceAccount && a.saEmail != "" {
			st.saByEmail[a.saEmail] = a
			if a.saUniqueID != "" {
				st.saByUniqueID[a.saUniqueID] = a.saEmail
			}
		}
		applyFoldedCounts(a, st.foldedCounts[a.uri])
	}
}

func applyFoldedCounts(a *ingestedAsset, fc map[string]int) {
	if len(fc) == 0 {
		return
	}
	if a.attrs == nil {
		a.attrs = make(map[string]any)
	}
	for _, ck := range sortedMapKeys(fc) {
		if prev, ok := a.attrs[ck].(int); ok && prev >= fc[ck] {
			continue
		}
		a.attrs[ck] = fc[ck]
	}
}

func (st *extractState) reconcileCloudRunService(svc *ingestedAsset) {
	matched := 0
	for _, te := range svc.runTraffic {
		rName := te.revisionName
		if rName == "" && te.latest {
			rName = svc.runLatestReady
		}
		if rev, ok := st.revisions[st.cloudRunRevisionURI(svc, rName)]; ok {
			mergeCloudRunRevision(svc, rev)
			matched++
		}
	}
	if matched == 0 && svc.runLatestReady != "" {
		if rev, ok := st.revisions[st.cloudRunRevisionURI(svc, svc.runLatestReady)]; ok {
			mergeCloudRunRevision(svc, rev)
		}
	}
}

func (st *extractState) cloudRunRevisionURI(svc *ingestedAsset, rName string) string {
	if rName == "" {
		return ""
	}
	if strings.Contains(rName, "/") {
		return CanonicalizeAIP122WithService(rName, svcRun, st.projIdx)
	}
	return prefixRunProjects + svc.projectID + segLocations + svc.location + "/revisions/" + rName
}

func mergeServiceRevisionBase(svc *ingestedAsset, rev *revisionSummary) {
	for _, r := range rev.envRefs {
		appendUnique(&svc.envRefs, r)
	}
	for _, s := range rev.secretRefs {
		appendUnique(&svc.secretRefs, s)
	}
	if svc.attrs == nil {
		svc.attrs = make(map[string]any)
	}
	setAttrIfEmpty(svc.attrs, "service_account", rev.serviceAccount)
	if setAttrIfEmpty(svc.attrs, "vpc_connector", rev.vpcConnector) {
		svc.addOutRef(rev.vpcConnector, svcVPCAccess, "vpc_connector", "")
	}
}

func setAttrIfEmpty(attrs map[string]any, key, val string) bool {
	if val == "" || getAttrString(attrs, key) != "" {
		return false
	}
	attrs[key] = val
	return true
}

func mergeCloudRunRevision(svc *ingestedAsset, rev *revisionSummary) {
	mergeServiceRevisionBase(svc, rev)
	for _, db := range rev.cloudSQLInstances {
		appendUnique(&svc.envRefs, db)
	}
	setAttrIfEmpty(svc.attrs, "vpc", rev.networkURI)
	setAttrIfEmpty(svc.attrs, "subnet", rev.subnetURI)
}

func (st *extractState) reconcileAppEngineService(svc *ingestedAsset) {
	if len(svc.aeSplit) == 0 {
		return
	}
	for _, verID := range sortedMapKeys(svc.aeSplit) {
		if rev, ok := st.revisions[svc.uri+"/versions/"+verID]; ok {
			mergeServiceRevisionBase(svc, rev)
		}
	}
}

func (st *extractState) resolveCloudBuild() {
	st.resolveCloudBuildTriggers()
	st.resolveCloudBuildHeaps()
}

func (st *extractState) resolveCloudBuildTriggers() {
	for _, a := range st.assets {
		if a.assetType != "cloudbuild.googleapis.com/BuildTrigger" {
			continue
		}
		if a.cbRepoOwner != "" && a.cbRepoName != "" && a.cbRepoHost != "" {
			extURI := "//" + a.cbRepoHost + "/" + a.cbRepoOwner + "/" + a.cbRepoName
			extType := "external.github.com/Repository"
			if a.cbRepoHost == "gitlab.com" {
				extType = "external.gitlab.com/Repository"
			}
			if _, exists := st.externalResources[extURI]; !exists {
				st.externalResources[extURI] = FactResource{
					URI:         extURI,
					AssetType:   extType,
					ProjectID:   "external",
					DisplayName: a.cbRepoName,
					IsExternal:  true,
				}
			}
			st.addEdge(extURI, a.uri, EdgeCloudBuildRepo, a.cbTriggerFilter)
		}
		if sa := getAttrString(a.attrs, "service_account"); sa != "" {
			st.markServiceAccountRef(a.uri, a.projectID, sa)
		}
		st.resolveCloudBuildRefList(a.uri, a.projectID, a.cbInlineRefs, a.cbInlineSecs)
	}
}

func (st *extractState) resolveCloudBuildHeaps() {
	keys := make([]cbHeapKey, 0, len(st.cbHeaps))
	for k := range st.cbHeaps {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].projectID != keys[j].projectID {
			return keys[i].projectID < keys[j].projectID
		}
		return keys[i].triggerID < keys[j].triggerID
	})

	for _, k := range keys {
		st.flushCloudBuildHeap(k, st.cbHeaps[k])
	}
}

func (st *extractState) flushCloudBuildHeap(k cbHeapKey, h *cbBuildMinHeap) {
	if h.Len() == 0 {
		return
	}
	sourceURI := ""
	if k.triggerID != "" {
		sourceURI = st.cbTriggers[k]
	}
	if sourceURI == "" && k.projectID != "" {
		sourceURI = "//cloudresourcemanager.googleapis.com/projects/" + k.projectID
	}
	if sourceURI == "" {
		return
	}
	items := append([]*cbBuildItem(nil), (*h)...)
	sort.Slice(items, func(i, j int) bool {
		if !items[i].createTime.Equal(items[j].createTime) {
			return items[i].createTime.Before(items[j].createTime)
		}
		return items[i].uri < items[j].uri
	})
	for _, item := range items {
		st.resolveCloudBuildRefList(sourceURI, k.projectID, item.refs, item.secretRefs)
		if item.serviceAccount != "" && st.assetsByURI[sourceURI] != nil {
			sa := item.serviceAccount
			if strings.Contains(sa, "/") {
				sa = shortResourceName(sa)
			}
			st.markServiceAccountRef(sourceURI, k.projectID, sa)
		}
	}
}

func (st *extractState) pruneDefaultSandboxAssets() {
	used := make(map[string]bool, len(st.referencedURIs)+len(st.edges)*2)
	for u := range st.referencedURIs {
		used[u] = true
	}
	for _, e := range st.edges {
		used[e.FromURI] = true
		used[e.ToURI] = true
	}

	isCandidateDefault := func(a *ingestedAsset) bool {
		return isDefaultSandboxAsset(
			a.assetType,
			a.uri,
			a.displayName,
			getAttrString(a.attrs, "vpc"),
			getAttrString(a.attrs, "email"),
		)
	}

	for _, a := range st.assets {
		if !a.suppressed && !isCandidateDefault(a) {
			st.markAssetRefsUsed(a, used)
		}
	}

	for _, a := range st.assets {
		if a.suppressed || !isCandidateDefault(a) {
			continue
		}
		if a.assetType == "logging.googleapis.com/LogBucket" || !used[a.uri] {
			a.suppressed = true
			st.suppressed[a.assetType]++
		}
	}
}

func (st *extractState) markAssetRefsUsed(a *ingestedAsset, used map[string]bool) {
	if a.parentURI != "" {
		used[a.parentURI] = true
		if vpc := st.subnetToVPC[a.parentURI]; vpc != "" {
			used[vpc] = true
		}
	}
	if vpc := getAttrString(a.attrs, "vpc"); vpc != "" {
		used[vpc] = true
	}
	if sub := getAttrString(a.attrs, "subnet"); sub != "" {
		used[sub] = true
		if vpc := st.subnetToVPC[sub]; vpc != "" {
			used[vpc] = true
		}
	}
	if sa := getAttrString(a.attrs, "service_account"); sa != "" {
		used[st.serviceAccountURI(sa, a.projectID)] = true
	}
}

func (st *extractState) buildFactsFile() *FactsFile {
	rt := st.readTime
	if rt.IsZero() {
		rt = time.Now().UTC()
	}

	resources, validURIs := st.collectFactResources()
	edges := st.filterFactEdges(&resources, validURIs)

	var hints []OutOfScopeHint
	for _, h := range st.outOfScope {
		hints = append(hints, *h)
	}

	out := normalizedFactsCopy(&FactsFile{
		SchemaVersion:            FactsSchemaVersion,
		ReadTime:                 rt.Format(time.RFC3339),
		Scopes:                   st.scopes,
		Hierarchy:                st.collectFactHierarchy(),
		Resources:                resources,
		Edges:                    edges,
		SuppressedPlumbingCounts: st.suppressed,
		OutOfScopeTargets:        hints,
		CoverageLedger:           st.ledger,
	})
	return &out
}

func (st *extractState) collectFactHierarchy() Hierarchy {
	orgs := make([]OrgNode, 0, len(st.orgs))
	for _, o := range st.orgs {
		orgs = append(orgs, o)
	}
	folders := make([]FolderNode, 0, len(st.folders))
	for _, f := range st.folders {
		folders = append(folders, f)
	}
	projects := make([]ProjectNode, 0, len(st.projects))
	for _, p := range st.projects {
		projects = append(projects, p)
	}
	return Hierarchy{
		Organizations: orgs,
		Folders:       folders,
		Projects:      projects,
	}
}

func (st *extractState) collectFactResources() ([]FactResource, map[string]bool) {
	resources := make([]FactResource, 0, len(st.assets)+len(st.externalResources))
	validURIs := make(map[string]bool, len(st.assets)+len(st.externalResources))
	for _, a := range st.assets {
		if a.suppressed || validURIs[a.uri] {
			continue
		}
		resources = append(resources, FactResource{
			URI:                 a.uri,
			AssetType:           a.assetType,
			ProjectID:           a.projectID,
			Location:            a.location,
			DisplayName:         a.displayName,
			ParentURI:           a.parentURI,
			Attributes:          a.attrs,
			ProjectCapabilities: a.capabilities,
		})
		validURIs[a.uri] = true
	}
	for _, ext := range st.externalResources {
		if validURIs[ext.URI] {
			continue
		}
		resources = append(resources, ext)
		validURIs[ext.URI] = true
	}
	return resources, validURIs
}

func (st *extractState) filterFactEdges(resources *[]FactResource, validURIs map[string]bool) []FactEdge {
	// Include project URIs for triggerless Cloud Build builds only if referenced by edges.
	edges := make([]FactEdge, 0, len(st.edges))
	for _, e := range st.edges {
		if !validURIs[e.FromURI] {
			st.maybeAddInScopeProjectResource(e.FromURI, resources, validURIs)
		}
		if validURIs[e.FromURI] && validURIs[e.ToURI] {
			edges = append(edges, e)
		}
	}
	return edges
}

func (st *extractState) maybeAddInScopeProjectResource(uri string, resources *[]FactResource, validURIs map[string]bool) {
	if !strings.HasPrefix(uri, "//cloudresourcemanager.googleapis.com/projects/") {
		return
	}
	proj := shortResourceName(uri)
	if !st.inScopeProjects[proj] {
		return
	}
	*resources = append(*resources, FactResource{
		URI:         uri,
		AssetType:   assetTypeProject,
		ProjectID:   proj,
		DisplayName: proj,
	})
	validURIs[uri] = true
}

func recanonicalizeAttrURIs(attrs map[string]any, projIdx map[string]string) {
	if attrs == nil {
		return
	}
	for _, k := range []string{"vpc", "subnet", "kms_key_uri", "vpc_connector"} {
		if v, ok := attrs[k].(string); ok && v != "" && strings.HasPrefix(v, "//") {
			attrs[k] = CanonicalizeAIP122(v, projIdx)
		}
	}
	if v, ok := attrs["subnet_project"].(string); ok && v != "" {
		attrs["subnet_project"] = mapProject(v, projIdx)
	}
}

func canonicalizeSlice(items []string, projIdx map[string]string) []string {
	if len(items) == 0 {
		return nil
	}
	var out []string
	for _, it := range items {
		v := it
		switch {
		case strings.HasPrefix(it, "//") || strings.HasPrefix(it, prefixProjects) || strings.HasPrefix(it, "apps/"):
			v = CanonicalizeAIP122(it, projIdx)
		case strings.HasPrefix(it, flagProjectPrefix):
			v = flagProjectPrefix + mapProject(strings.TrimPrefix(it, flagProjectPrefix), projIdx)
		case strings.HasPrefix(it, prefixGCR):
			v = prefixGCR + mapProject(strings.TrimPrefix(it, prefixGCR), projIdx)
		}
		appendUnique(&out, v)
	}
	sort.Strings(out)
	return out
}

func getAttrString(attrs map[string]any, key string) string {
	if attrs == nil {
		return ""
	}
	s, _ := attrs[key].(string)
	return s
}
