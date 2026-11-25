package extract

import (
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

var (
	secretRefRe        = regexp.MustCompile(`(?://secretmanager\.googleapis\.com/)?projects/[^/\s"',]+/secrets/[^/\s"',]+(?:/versions/[^/\s"',]+)?`)
	aip122URIRe        = regexp.MustCompile(`//[a-z0-9.-]+\.googleapis\.com/[^\s"',?#]+`)
	googleapisURLRe    = regexp.MustCompile(`https?://(?:www\.|[a-z0-9-]+\.)?googleapis\.com/[^\s"',]+`)
	gcsBucketRe        = regexp.MustCompile(`gs://([a-z0-9][a-z0-9._-]{1,220}[a-z0-9])`)
	runAppURLRe        = regexp.MustCompile(`https?://(?:[^\s/@"',]+@)?([a-z0-9.-]+\.run\.app)(?::\d+)?(?:/[^\s"',]*)?`)
	cloudFuncURLRe     = regexp.MustCompile(`https?://(?:[^\s/@"',]+@)?([a-z0-9.-]+\.cloudfunctions\.net)(?::\d+)?(?:/([a-zA-Z0-9_-]+))?`)
	connURLRe          = regexp.MustCompile(`(?:jdbc:)?[a-z][a-z0-9+.-]*://[^\s"',]+`)
	cloudSQLPathRe     = regexp.MustCompile(`/cloudsql/([a-z0-9-]+):([a-z0-9-]+):([a-zA-Z0-9_-]+)`)
	bareCloudSQLRe     = regexp.MustCompile(`(?:^|[\s"',=;(:/])([a-z][a-z0-9-]{1,29}|[0-9]{3,}):((?:us|europe|asia|australia|northamerica|southamerica|me|africa)-[a-z]+\d+):([a-zA-Z0-9][a-zA-Z0-9_-]{0,97})(?:$|[\s"',;)/?=])`)
	artifactRegistryRe = regexp.MustCompile(`(?:^|[\s"',=;(/])([a-z0-9-]+)-(?:docker|python|maven|npm|apt|yum|go|generic|kfp)\.pkg\.dev/([a-z][a-z0-9-]{1,29}|[0-9]{3,})/([a-zA-Z0-9][a-zA-Z0-9._-]*)`)
	gcrImageRe         = regexp.MustCompile(`(?:^|[\s"',=;(/])((?:(?:us|eu|asia)\.)?gcr\.io)/([a-z][a-z0-9-]{1,29}|[0-9]{3,})/[^\s"',;)]+`)
	flagProjectRe      = regexp.MustCompile(`(?:^|[\s"'])--project(?:=|\s+)([a-z][a-z0-9-]{1,29}|[0-9]{3,})\b`)
	flagClusterRe      = regexp.MustCompile(`(?:^|[\s"'])--cluster(?:=|\s+)([a-zA-Z0-9][a-zA-Z0-9._-]{0,62})\b`)
	gcloudRunDeployRe  = regexp.MustCompile(`\bgcloud(?:\s+(?:beta|alpha))?\s+run\s+deploy\s+([a-z][a-z0-9-]{0,62})\b`)
	relResourceRe      = regexp.MustCompile(`\bprojects/[a-z0-9-]+/(?:topics|subscriptions|datasets|global|regions|zones|locations|instances)/[^\s"',?#]+`)
	projDatasetRe      = regexp.MustCompile(`^([a-z][a-z0-9-]{4,28}[a-z0-9])[.:]([a-zA-Z_][a-zA-Z0-9_]*)(?:\.[a-zA-Z0-9_-]+)?$`)
	ipv4Re             = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	psaTenantRe        = regexp.MustCompile(`^p[a-z0-9]+-tp$`)

	excludedDatasetSuffixes = map[string]bool{
		"com": true, "net": true, "org": true, "io": true, "gov": true, "edu": true,
		"dev": true, "app": true, "internal": true, "local": true, "svc": true,
		"cluster": true, "json": true, "yaml": true, "yml": true, "txt": true,
		"csv": true, "parquet": true, "sql": true, "py": true, "go": true, "sh": true,
		"googleapis": true,
	}

	// sensitiveDroppedKeys lists keys unconditionally excluded from any emitted metadata.
	sensitiveDroppedKeys = map[string]bool{
		"startup-script":            true,
		"startup-script-url":        true,
		"shutdown-script":           true,
		"user-data":                 true,
		"env":                       true,
		"envvariables":              true,
		"secretenv":                 true,
		"environmentvariables":      true,
		"buildenvironmentvariables": true,
		"args":                      true,
		"command":                   true,
		"script":                    true,
		"data":                      true,
		"binarydata":                true,
		"stringdata":                true,
		"substitutions":             true,
		"query":                     true,
		"rawquery":                  true,
		"schema":                    true,
		"webhookconfig":             true,
		"kubectl.kubernetes.io/last-applied-configuration": true,
		"last-applied-configuration":                       true,
	}

	// suppressedPlumbingServices salvaged from base_catalog.yaml control-plane/plumbing APIs.
	suppressedPlumbingServices = map[string]bool{
		"sts.googleapis.com":                  true,
		"iamcredentials.googleapis.com":       true,
		"iam.googleapis.com":                  true,
		"oauth2.googleapis.com":               true,
		"oslogin.googleapis.com":              true,
		"serviceusage.googleapis.com":         true,
		"servicemanagement.googleapis.com":    true,
		"servicecontrol.googleapis.com":       true,
		"cloudresourcemanager.googleapis.com": true,
		"compute.googleapis.com":              true,
		"container.googleapis.com":            true,
		"cloudasset.googleapis.com":           true,
		"orgpolicy.googleapis.com":            true,
		"cloudbilling.googleapis.com":         true,
		"recommender.googleapis.com":          true,
		"cloudapis.googleapis.com":            true,
	}

	// plumbingAssetTypes lists CAI asset types dropped into SuppressedPlumbingCounts
	// (after any structural/GKE/Cloud Build folding or edge extraction).
	plumbingAssetTypes = map[string]bool{
		// Compute / VPC / LB plumbing & absorbed children
		"compute.googleapis.com/Route":                true,
		"compute.googleapis.com/Firewall":             true,
		"compute.googleapis.com/Policy":               true,
		"compute.googleapis.com/Project":              true,
		"compute.googleapis.com/InstanceTemplate":     true,
		"compute.googleapis.com/Autoscaler":           true,
		"compute.googleapis.com/HealthCheck":          true,
		"compute.googleapis.com/Disk":                 true,
		"compute.googleapis.com/Address":              true,
		"compute.googleapis.com/GlobalAddress":        true,
		"compute.googleapis.com/TargetHttpProxy":      true,
		"compute.googleapis.com/TargetHttpsProxy":     true,
		"compute.googleapis.com/TargetGrpcProxy":      true,
		"compute.googleapis.com/TargetTcpProxy":       true,
		"compute.googleapis.com/TargetSslProxy":       true,
		"compute.googleapis.com/SslCertificate":       true,
		"compute.googleapis.com/UrlMap":               true,
		"compute.googleapis.com/BackendService":       true,
		"compute.googleapis.com/BackendBucket":        true,
		"compute.googleapis.com/SecurityPolicy":       true,
		"compute.googleapis.com/NetworkEndpointGroup": true,
		"compute.googleapis.com/ExternalVpnGateway":   true,
		"compute.googleapis.com/VpnTunnel":            true,
		// Cloud Build historical builds
		"cloudbuild.googleapis.com/Build": true,
		// GKE / Kubernetes plumbing
		"container.googleapis.com/NodePool": true,
		"gkehub.googleapis.com/Membership":  true,
		"k8s.io/Namespace":                  true,
		"k8s.io/ServiceAccount":             true,
		"k8s.io/Secret":                     true,
		"k8s.io/ConfigMap":                  true,
		"k8s.io/Endpoints":                  true,
		"discovery.k8s.io/EndpointSlice":    true,
		"k8s.io/Pod":                        true,
		"apps.k8s.io/ReplicaSet":            true,
		"k8s.io/Event":                      true,
		"k8s.io/Node":                       true,
		"k8s.io/ResourceQuota":              true,
		"k8s.io/PersistentVolume":           true,
		"k8s.io/PersistentVolumeClaim":      true,
		"storage.k8s.io/StorageClass":       true,
		// SQL / DB / Storage / Analytics absorbed children & backups
		"sqladmin.googleapis.com/BackupRun":                true,
		"sqladmin.googleapis.com/Backup":                   true,
		"alloydb.googleapis.com/Instance":                  true,
		"spanner.googleapis.com/Database":                  true,
		"bigtableadmin.googleapis.com/Cluster":             true,
		"bigtableadmin.googleapis.com/AppProfile":          true,
		"bigtableadmin.googleapis.com/Table":               true,
		"netapp.googleapis.com/Volume":                     true,
		"bigquery.googleapis.com/Table":                    true,
		"bigquery.googleapis.com/Routine":                  true,
		"bigquery.googleapis.com/Model":                    true,
		"bigquery.googleapis.com/RowAccessPolicy":          true,
		"artifactregistry.googleapis.com/DockerImage":      true,
		"artifactregistry.googleapis.com/Package":          true,
		"artifactregistry.googleapis.com/Version":          true,
		"run.googleapis.com/Revision":                      true,
		"appengine.googleapis.com/Application":             true,
		"appengine.googleapis.com/Version":                 true,
		"dns.googleapis.com/ResourceRecordSet":             true,
		"networkconnectivity.googleapis.com/Spoke":         true,
		"certificatemanager.googleapis.com/Certificate":    true,
		"certificatemanager.googleapis.com/CertificateMap": true,
		"apigateway.googleapis.com/Api":                    true,
		"apigateway.googleapis.com/ApiConfig":              true,
		"apigee.googleapis.com/Instance":                   true,
		"apigee.googleapis.com/EnvironmentGroup":           true,
		"apigee.googleapis.com/Environment":                true,
		"apigee.googleapis.com/EndpointAttachment":         true,
		"apigee.googleapis.com/ApiProxy":                   true,
		"datastream.googleapis.com/ConnectionProfile":      true,
		"datastream.googleapis.com/PrivateConnection":      true,
		"dataproc.googleapis.com/AutoscalingPolicy":        true,
		"dataform.googleapis.com/Workspace":                true,
		"dataform.googleapis.com/CompilationResult":        true,
		"dataform.googleapis.com/WorkflowInvocation":       true,
		"dataform.googleapis.com/WorkflowConfig":           true,
		"dataplex.googleapis.com/Zone":                     true,
		"dataplex.googleapis.com/Asset":                    true,
		"analyticshub.googleapis.com/Listing":              true,
		"aiplatform.googleapis.com/Model":                  true,
		"aiplatform.googleapis.com/Index":                  true,
		"aiplatform.googleapis.com/FeatureGroup":           true,
		"aiplatform.googleapis.com/FeatureView":            true,
		"documentai.googleapis.com/ProcessorVersion":       true,
		// Control plane & misc plumbing
		"cloudresourcemanager.googleapis.com/Organization":     true,
		"cloudresourcemanager.googleapis.com/Folder":           true,
		"cloudresourcemanager.googleapis.com/Project":          true,
		"serviceusage.googleapis.com/Service":                  true,
		"accesscontextmanager.googleapis.com/AccessPolicy":     true,
		"accesscontextmanager.googleapis.com/ServicePerimeter": true,
		"servicenetworking.googleapis.com/Connection":          true,
		"containeranalysis.googleapis.com/Occurrence":          true,
	}

	plumbingAssetPrefixes = []string{
		"admissionregistration.k8s.io/",
		"rbac.authorization.k8s.io/",
		"gateway.networking.k8s.io/",
		"autoscaling.k8s.io/",
		"servicedirectory.googleapis.com/",
	}
)

// IsPlumbingAssetType reports whether assetType is classified as plumbing or an absorbed
// sub-resource that belongs in SuppressedPlumbingCounts rather than active FactResources.
func IsPlumbingAssetType(assetType string) bool {
	if plumbingAssetTypes[assetType] {
		return true
	}
	for _, pfx := range plumbingAssetPrefixes {
		if strings.HasPrefix(assetType, pfx) {
			return true
		}
	}
	host, _, _ := strings.Cut(assetType, "/")
	if suppressedPlumbingServices[host] &&
		host != "compute.googleapis.com" &&
		host != "container.googleapis.com" &&
		host != "iam.googleapis.com" {
		return true
	}
	return false
}

// IsPSATenantProject reports whether projectID matches Google's Private Services Access
// managed tenant project convention (^p[a-z0-9]+-tp$).
func IsPSATenantProject(projectID string) bool {
	return psaTenantRe.MatchString(strings.ToLower(strings.TrimSpace(projectID)))
}

// IsSensitiveKey reports whether key is unconditionally dropped by sanitization rules.
func IsSensitiveKey(key string) bool {
	return sensitiveDroppedKeys[strings.ToLower(strings.TrimSpace(key))]
}

// SanitizeURL removes userinfo (credentials), query strings, and fragments from a URL.
func SanitizeURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(s), "jdbc:") {
		s = s[5:]
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		if i := strings.IndexAny(s, "?#"); i >= 0 {
			s = s[:i]
		}
		return s
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

type refCollector struct {
	seenEnv    map[string]struct{}
	seenSec    map[string]struct{}
	envRefs    []string
	secretRefs []string
}

func (c *refCollector) addEnv(v string) {
	if v == "" {
		return
	}
	if _, dup := c.seenEnv[v]; !dup {
		c.seenEnv[v] = struct{}{}
		c.envRefs = append(c.envRefs, v)
	}
}

func (c *refCollector) addSec(v string) {
	if v == "" {
		return
	}
	if _, dup := c.seenSec[v]; !dup {
		c.seenSec[v] = struct{}{}
		c.secretRefs = append(c.secretRefs, v)
	}
}

func (c *refCollector) addCanonical(canon string) {
	if strings.HasPrefix(canon, "//secretmanager.googleapis.com/") {
		c.addSec(canon)
	} else if strings.HasPrefix(canon, "//") {
		c.addEnv(canon)
	}
}

// ExtractSanitizedRefs scans raw config/env/arg/command/script/ConfigMap strings and retains
// ONLY recognized sanitized references (AIP-122 URIs, Artifact Registry repos, GCR projects,
// gs://<bucket>, Cloud SQL Unix/bare connection names, --project/--cluster/gcloud run deploy,
// *.run.app, *.cloudfunctions.net, <project>.<dataset>, private IPv4s, and Secret Manager names),
// unconditionally discarding raw secrets, credentials, and scripts.
func ExtractSanitizedRefs(raw string, projIdx map[string]string) (envRefs []string, secretRefs []string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	c := &refCollector{
		seenEnv: make(map[string]struct{}),
		seenSec: make(map[string]struct{}),
	}
	scanSecretAndSQLRefs(s, projIdx, c)
	scanRegistryAndCLIFlagRefs(s, projIdx, c)
	scanURIAndEndpointRefs(s, projIdx, c)
	scanNetworkAndDatasetRefs(s, projIdx, c)
	return c.envRefs, c.secretRefs
}

func scanSecretAndSQLRefs(s string, projIdx map[string]string, c *refCollector) {
	for _, m := range secretRefRe.FindAllString(s, -1) {
		c.addSec(CanonicalizeAIP122(m, projIdx))
	}
	for _, m := range cloudSQLPathRe.FindAllStringSubmatch(s, -1) {
		c.addEnv("//sqladmin.googleapis.com/projects/" + mapProject(m[1], projIdx) + "/instances/" + m[3])
	}
	for _, m := range bareCloudSQLRe.FindAllStringSubmatch(s, -1) {
		c.addEnv("//sqladmin.googleapis.com/projects/" + mapProject(m[1], projIdx) + "/instances/" + m[3])
	}
}

func scanRegistryAndCLIFlagRefs(s string, projIdx map[string]string, c *refCollector) {
	for _, m := range artifactRegistryRe.FindAllStringSubmatch(s, -1) {
		loc := strings.ToLower(m[1])
		proj := mapProject(m[2], projIdx)
		c.addEnv("//artifactregistry.googleapis.com/projects/" + proj + "/locations/" + loc + "/repositories/" + m[3])
	}
	for _, m := range gcrImageRe.FindAllStringSubmatch(s, -1) {
		c.addEnv("gcr.io/" + mapProject(m[2], projIdx))
	}
	for _, m := range flagProjectRe.FindAllStringSubmatch(s, -1) {
		c.addEnv("--project=" + mapProject(m[1], projIdx))
	}
	for _, m := range flagClusterRe.FindAllStringSubmatch(s, -1) {
		c.addEnv("--cluster=" + m[1])
	}
	for _, m := range gcloudRunDeployRe.FindAllStringSubmatch(s, -1) {
		c.addEnv("run-deploy:" + m[1])
	}
}

func scanURIAndEndpointRefs(s string, projIdx map[string]string, c *refCollector) {
	for _, idx := range aip122URIRe.FindAllStringIndex(s, -1) {
		if idx[0] > 0 && s[idx[0]-1] == ':' {
			continue
		}
		c.addCanonical(CanonicalizeAIP122(s[idx[0]:idx[1]], projIdx))
	}
	for _, m := range googleapisURLRe.FindAllString(s, -1) {
		c.addCanonical(CanonicalizeAIP122(SanitizeURL(m), projIdx))
	}
	for _, m := range gcsBucketRe.FindAllStringSubmatch(s, -1) {
		c.addEnv("gs://" + m[1])
	}
	for _, m := range runAppURLRe.FindAllStringSubmatch(s, -1) {
		c.addEnv("https://" + strings.ToLower(m[1]))
	}
	for _, m := range cloudFuncURLRe.FindAllStringSubmatch(s, -1) {
		ref := "https://" + strings.ToLower(m[1])
		if m[2] != "" {
			ref += "/" + m[2]
		}
		c.addEnv(ref)
	}
	for _, m := range relResourceRe.FindAllString(s, -1) {
		c.addCanonical(CanonicalizeAIP122(m, projIdx))
	}
}

func scanNetworkAndDatasetRefs(s string, projIdx map[string]string, c *refCollector) {
	for _, m := range connURLRe.FindAllString(s, -1) {
		if u, err := url.Parse(SanitizeURL(m)); err == nil {
			if ip := parsePrivateIPv4(u.Hostname()); ip != "" {
				c.addEnv(ip)
			}
		}
	}
	for _, m := range ipv4Re.FindAllString(s, -1) {
		if ip := parsePrivateIPv4(m); ip != "" {
			c.addEnv(ip)
		}
	}
	if m := projDatasetRe.FindStringSubmatch(s); len(m) == 3 {
		if !excludedDatasetSuffixes[strings.ToLower(m[2])] {
			c.addEnv(mapProject(m[1], projIdx) + "." + m[2])
		}
	}
}

type attrProjector struct {
	attrs   map[string]any
	projID  string
	projIdx map[string]string
	region  string
}

func (p *attrProjector) setStr(k, v string) {
	if v = strings.TrimSpace(v); v != "" {
		p.attrs[k] = v
	}
}

func (p *attrProjector) setBool(k string, v bool) {
	if v {
		p.attrs[k] = true
	}
}

func (p *attrProjector) setInt(k string, v int) {
	if v > 0 {
		p.attrs[k] = v
	}
}

func (p *attrProjector) setURI(k, raw, defaultSvc string) {
	if raw = strings.TrimSpace(raw); raw == "" {
		return
	}
	if defaultSvc == "compute.googleapis.com" && !strings.Contains(raw, "/") && p.projID != "" {
		switch k {
		case "vpc":
			p.attrs[k] = "//compute.googleapis.com/projects/" + mapProject(p.projID, p.projIdx) + "/global/networks/" + raw
			return
		case "subnet":
			if p.region != "" {
				p.attrs[k] = "//compute.googleapis.com/projects/" + mapProject(p.projID, p.projIdx) + "/regions/" + p.region + "/subnetworks/" + raw
				return
			}
			p.attrs[k] = raw
			return
		}
	}
	if canon := CanonicalizeAIP122WithService(raw, defaultSvc, p.projIdx); canon != "" {
		p.attrs[k] = canon
	}
}

func (p *attrProjector) setFirstNICNetwork(parent *structpb.Struct) {
	for _, nicVal := range getListField(parent, "networkInterfaces") {
		if nic := nicVal.GetStructValue(); nic != nil {
			p.setURI("vpc", getStringField(nic, "network"), "compute.googleapis.com")
			p.setURI("subnet", getStringField(nic, "subnetwork"), "compute.googleapis.com")
			return
		}
	}
}

type attrProjectorFn func(p *attrProjector, data *structpb.Struct)

var assetAttrProjectors = map[string]attrProjectorFn{
	"compute.googleapis.com/Network":                     projectComputeNetworkAttrs,
	"compute.googleapis.com/Subnetwork":                  projectComputeSubnetworkAttrs,
	"compute.googleapis.com/Instance":                    projectComputeInstanceAttrs,
	"compute.googleapis.com/InstanceGroupManager":        projectComputeIGMAttrs,
	"compute.googleapis.com/ForwardingRule":              projectForwardingRuleAttrs,
	"compute.googleapis.com/GlobalForwardingRule":        projectForwardingRuleAttrs,
	"compute.googleapis.com/Router":                      projectComputeRouterAttrs,
	"compute.googleapis.com/VpnGateway":                  projectComputeVPCOnlyAttr,
	"compute.googleapis.com/TargetVpnGateway":            projectComputeVPCOnlyAttr,
	"compute.googleapis.com/InterconnectAttachment":      projectInterconnectAttrs,
	"compute.googleapis.com/ServiceAttachment":           projectServiceAttachmentAttrs,
	"container.googleapis.com/Cluster":                   projectGKEClusterAttrs,
	"apps.k8s.io/Deployment":                             projectK8sWorkloadAttrs,
	"apps.k8s.io/StatefulSet":                            projectK8sWorkloadAttrs,
	"apps.k8s.io/DaemonSet":                              projectK8sWorkloadAttrs,
	"batch.k8s.io/CronJob":                               projectK8sCronJobAttrs,
	"batch.k8s.io/Job":                                   projectK8sJobAttrs,
	"k8s.io/Service":                                     projectK8sServiceAttrs,
	"networking.k8s.io/Ingress":                          projectK8sIngressAttrs,
	"sqladmin.googleapis.com/Instance":                   projectCloudSQLAttrs,
	"alloydb.googleapis.com/Cluster":                     projectAlloyDBAttrs,
	"spanner.googleapis.com/Instance":                    projectSpannerAttrs,
	"firestore.googleapis.com/Database":                  projectFirestoreAttrs,
	"bigtableadmin.googleapis.com/Instance":              projectBigtableAttrs,
	"redis.googleapis.com/Instance":                      projectRedisInstanceAttrs,
	"redis.googleapis.com/Cluster":                       projectRedisClusterAttrs,
	"memcache.googleapis.com/Instance":                   projectMemcacheAttrs,
	"storage.googleapis.com/Bucket":                      projectGCSBucketAttrs,
	"file.googleapis.com/Instance":                       projectFilestoreAttrs,
	"netapp.googleapis.com/StoragePool":                  projectNetAppAttrs,
	"bigquery.googleapis.com/Dataset":                    projectBQDatasetAttrs,
	"bigqueryconnection.googleapis.com/Connection":       projectBQConnectionAttrs,
	"bigquerydatatransfer.googleapis.com/TransferConfig": projectBQTransferAttrs,
	"artifactregistry.googleapis.com/Repository":         projectArtifactRegistryAttrs,
	"cloudbuild.googleapis.com/BuildTrigger":             projectCloudBuildTriggerAttrs,
	"clouddeploy.googleapis.com/DeliveryPipeline":        projectCloudDeployAttrs,
	"run.googleapis.com/Service":                         projectCloudRunServiceAttrs,
	"run.googleapis.com/Job":                             projectCloudRunJobAttrs,
	"cloudfunctions.googleapis.com/CloudFunction":        projectCloudFunctionAttrs,
	"appengine.googleapis.com/Service":                   projectAppEngineAttrs,
	"batch.googleapis.com/Job":                           projectBatchJobAttrs,
	"pubsub.googleapis.com/Topic":                        projectPubSubTopicAttrs,
	"pubsub.googleapis.com/Subscription":                 projectPubSubSubAttrs,
	"managedkafka.googleapis.com/Cluster":                projectManagedKafkaAttrs,
	"eventarc.googleapis.com/Trigger":                    projectEventarcTriggerAttrs,
	"cloudtasks.googleapis.com/Queue":                    projectStateOnlyAttr,
	"cloudscheduler.googleapis.com/Job":                  projectSchedulerJobAttrs,
	"workflows.googleapis.com/Workflow":                  projectWorkflowAttrs,
	"dataflow.googleapis.com/Job":                        projectDataflowJobAttrs,
	"datastream.googleapis.com/Stream":                   projectStateOnlyAttr,
	"dataproc.googleapis.com/Cluster":                    projectDataprocClusterAttrs,
	"dataproc.googleapis.com/Batch":                      projectDataprocBatchAttrs,
	"metastore.googleapis.com/Service":                   projectMetastoreAttrs,
	"composer.googleapis.com/Environment":                projectComposerAttrs,
	"dataform.googleapis.com/Repository":                 projectDataformAttrs,
	"dataplex.googleapis.com/Lake":                       projectStateOnlyAttr,
	"datafusion.googleapis.com/Instance":                 projectDataFusionAttrs,
	"looker.googleapis.com/Instance":                     projectLookerAttrs,
	"dns.googleapis.com/ManagedZone":                     projectDNSZoneAttrs,
	"vpcaccess.googleapis.com/Connector":                 projectVPCConnectorAttrs,
	"apigateway.googleapis.com/Gateway":                  projectAPIGatewayAttrs,
	"apigee.googleapis.com/Organization":                 projectApigeeAttrs,
	"aiplatform.googleapis.com/Endpoint":                 projectVertexEndpointAttrs,
	"aiplatform.googleapis.com/IndexEndpoint":            projectVertexIndexEndpointAttrs,
	"aiplatform.googleapis.com/ReasoningEngine":          projectVertexReasoningEngineAttrs,
	"aiplatform.googleapis.com/FeatureOnlineStore":       projectStateOnlyAttr,
	"discoveryengine.googleapis.com/Engine":              projectDiscoveryEngineAttrs,
	"discoveryengine.googleapis.com/DataStore":           projectDiscoveryDataStoreAttrs,
	"dialogflow.googleapis.com/Agent":                    projectDialogflowAttrs,
	"documentai.googleapis.com/Processor":                projectDocumentAIAttrs,
	"iam.googleapis.com/ServiceAccount":                  projectServiceAccountAttrs,
	"secretmanager.googleapis.com/Secret":                projectSecretAttrs,
	"cloudkms.googleapis.com/CryptoKey":                  projectCryptoKeyAttrs,
	"logging.googleapis.com/LogBucket":                   projectLogBucketAttrs,
	"monitoring.googleapis.com/AlertPolicy":              projectAlertPolicyAttrs,
}

// ProjectAttributes constructs FactResource.Attributes strictly from a per-asset_type
// metadata allowlist. Raw CAI resource data maps are never forwarded, and sensitive keys
// (env, args, command, script, ConfigMap data, last-applied-configuration, etc.) can never appear.
func ProjectAttributes(assetType string, data *structpb.Struct, projID string, projIdx map[string]string) map[string]any {
	if data == nil {
		return nil
	}
	fn, ok := assetAttrProjectors[assetType]
	if !ok {
		return nil
	}
	region := regionFromLocation(getStringField(data, "region"))
	if region == "" {
		region = regionFromLocation(getStringField(data, "location"))
	}
	if region == "" {
		region = regionFromLocation(getStringField(data, "zone"))
	}
	p := &attrProjector{
		attrs:   make(map[string]any),
		projID:  projID,
		projIdx: projIdx,
		region:  region,
	}
	fn(p, data)
	if len(p.attrs) == 0 {
		return nil
	}
	return p.attrs
}

func projectComputeNetworkAttrs(p *attrProjector, data *structpb.Struct) {
	if rc := getStructField(data, "routingConfig"); rc != nil {
		p.setStr("routing_mode", getStringField(rc, "routingMode"))
	}
	if v, ok := getBoolField(data, "autoCreateSubnetworks"); ok {
		p.attrs["auto_create_subnetworks"] = v
	}
}

func projectComputeSubnetworkAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("cidr", getStringField(data, "ipCidrRange"))
	p.setStr("purpose", getStringField(data, "purpose"))
	p.setBool("private_ip_google_access", getBoolValue(data, "privateIpGoogleAccess"))
	p.setURI("vpc", getStringField(data, "network"), "compute.googleapis.com")
}

func projectComputeInstanceAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("machine_type", shortResourceName(getStringField(data, "machineType")))
	p.setStr("zone", shortResourceName(getStringField(data, "zone")))
	for _, nicVal := range getListField(data, "networkInterfaces") {
		if nic := nicVal.GetStructValue(); nic != nil {
			p.setStr("private_ip", getStringField(nic, "networkIP"))
			p.setURI("vpc", getStringField(nic, "network"), "compute.googleapis.com")
			p.setURI("subnet", getStringField(nic, "subnetwork"), "compute.googleapis.com")
			break
		}
	}
	for _, saVal := range getListField(data, "serviceAccounts") {
		if sa := saVal.GetStructValue(); sa != nil {
			if email := getStringField(sa, "email"); email != "" {
				p.setStr("service_account", email)
				break
			}
		}
	}
	for _, dVal := range getListField(data, "disks") {
		if d := dVal.GetStructValue(); d != nil {
			if kms := getStringField(getStructField(d, "diskEncryptionKey"), "kmsKeyName"); kms != "" {
				p.setURI("kms_key_uri", kms, "cloudkms.googleapis.com")
				break
			}
		}
	}
}

func projectComputeIGMAttrs(p *attrProjector, data *structpb.Struct) {
	p.setInt("target_size", getIntField(data, "targetSize"))
	p.setStr("base_instance_name", getStringField(data, "baseInstanceName"))
}

func projectForwardingRuleAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("ip_address", getStringField(data, "IPAddress"))
	p.setStr("port_range", getStringField(data, "portRange"))
	p.setStr("ip_protocol", getStringField(data, "IPProtocol"))
	p.setStr("load_balancing_scheme", getStringField(data, "loadBalancingScheme"))
	if ports := getStringListField(data, "ports"); len(ports) > 0 {
		sort.Strings(ports)
		p.attrs["ports"] = ports
	}
	p.setURI("vpc", getStringField(data, "network"), "compute.googleapis.com")
	p.setURI("subnet", getStringField(data, "subnetwork"), "compute.googleapis.com")
}

func projectComputeRouterAttrs(p *attrProjector, data *structpb.Struct) {
	p.setURI("vpc", getStringField(data, "network"), "compute.googleapis.com")
	p.setInt("nat_count", len(getListField(data, "nats")))
	if bgp := getStructField(data, "bgp"); bgp != nil {
		p.setInt("bgp_asn", getIntField(bgp, "asn"))
	}
}

func projectComputeVPCOnlyAttr(p *attrProjector, data *structpb.Struct) {
	p.setURI("vpc", getStringField(data, "network"), "compute.googleapis.com")
}

func projectInterconnectAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("type", getStringField(data, "type"))
	p.setStr("bandwidth", getStringField(data, "bandwidth"))
	p.setInt("vlan_tag", getIntField(data, "vlanTag8021q"))
}

func projectServiceAttachmentAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("connection_preference", getStringField(data, "connectionPreference"))
	p.setBool("enable_proxy_protocol", getBoolValue(data, "enableProxyProtocol"))
}

func projectGKEClusterAttrs(p *attrProjector, data *structpb.Struct) {
	if getBoolValue(getStructField(data, "autopilot"), "enabled") {
		p.attrs["mode"] = "Autopilot"
	} else {
		p.attrs["mode"] = "Standard"
	}
	p.setBool("private_nodes", getBoolValue(getStructField(data, "privateClusterConfig"), "enablePrivateNodes"))
	p.setStr("release_channel", getStringField(getStructField(data, "releaseChannel"), "channel"))
	nc := getStructField(data, "networkConfig")
	netRaw := getStringField(nc, "network")
	if netRaw == "" {
		netRaw = getStringField(data, "network")
	}
	subRaw := getStringField(nc, "subnetwork")
	if subRaw == "" {
		subRaw = getStringField(data, "subnetwork")
	}
	p.setURI("vpc", netRaw, "compute.googleapis.com")
	p.setURI("subnet", subRaw, "compute.googleapis.com")
	p.setURI("kms_key_uri", getStringField(getStructField(data, "databaseEncryption"), "keyName"), "cloudkms.googleapis.com")
	p.setStr("workload_pool", getStringField(getStructField(data, "workloadIdentityConfig"), "workloadPool"))
	p.setInt("node_pool_count", len(getListField(data, "nodePools")))
}

func projectK8sWorkloadAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("namespace", getStringField(getStructField(data, "metadata"), "namespace"))
	spec := getStructField(data, "spec")
	p.setInt("replicas", getIntField(spec, "replicas"))
	if podSpec := getStructField(getStructField(spec, "template"), "spec"); podSpec != nil {
		p.setStr("k8s_service_account", getStringField(podSpec, "serviceAccountName"))
	}
}

func projectK8sCronJobAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("namespace", getStringField(getStructField(data, "metadata"), "namespace"))
	spec := getStructField(data, "spec")
	p.setStr("schedule", getStringField(spec, "schedule"))
	if jt := getStructField(spec, "jobTemplate"); jt != nil {
		if podSpec := getStructField(getStructField(getStructField(jt, "spec"), "template"), "spec"); podSpec != nil {
			p.setStr("k8s_service_account", getStringField(podSpec, "serviceAccountName"))
		}
	}
}

func projectK8sJobAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("namespace", getStringField(getStructField(data, "metadata"), "namespace"))
	spec := getStructField(data, "spec")
	p.setInt("completions", getIntField(spec, "completions"))
	if podSpec := getStructField(getStructField(spec, "template"), "spec"); podSpec != nil {
		p.setStr("k8s_service_account", getStringField(podSpec, "serviceAccountName"))
	}
}

func projectK8sServiceAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("namespace", getStringField(getStructField(data, "metadata"), "namespace"))
	spec := getStructField(data, "spec")
	p.setStr("service_type", getStringField(spec, "type"))
	p.setStr("cluster_ip", getStringField(spec, "clusterIP"))
	var ports []int
	for _, pVal := range getListField(spec, "ports") {
		if portObj := pVal.GetStructValue(); portObj != nil {
			if port := getIntField(portObj, "port"); port > 0 {
				ports = append(ports, port)
			}
		}
	}
	if len(ports) > 0 {
		sort.Ints(ports)
		p.attrs["ports"] = ports
	}
}

func projectK8sIngressAttrs(p *attrProjector, data *structpb.Struct) {
	meta := getStructField(data, "metadata")
	p.setStr("namespace", getStringField(meta, "namespace"))
	ic := getStringField(getStructField(data, "spec"), "ingressClassName")
	if ic == "" {
		ic = getStringField(getStructField(meta, "annotations"), "kubernetes.io/ingress.class")
	}
	p.setStr("ingress_class", ic)
}

func projectCloudSQLAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("database_version", getStringField(data, "databaseVersion"))
	settings := getStructField(data, "settings")
	p.setStr("tier", getStringField(settings, "tier"))
	p.setStr("availability_type", getStringField(settings, "availabilityType"))
	ipCfg := getStructField(settings, "ipConfiguration")
	p.setURI("vpc", getStringField(ipCfg, "privateNetwork"), "compute.googleapis.com")
	if v, ok := getBoolField(ipCfg, "ipv4Enabled"); ok && v {
		p.attrs["public_ip_enabled"] = true
	}
	for _, ipVal := range getListField(data, "ipAddresses") {
		m := ipVal.GetStructValue()
		if m == nil {
			continue
		}
		typ := strings.ToUpper(getStringField(m, "type"))
		if typ == "" || typ == "PRIVATE" {
			if ip := parsePrivateIPv4(getStringField(m, "ipAddress")); ip != "" {
				p.attrs["private_ip"] = ip
				break
			}
		}
	}
	p.setURI("kms_key_uri", getStringField(getStructField(data, "diskEncryptionConfiguration"), "kmsKeyName"), "cloudkms.googleapis.com")
}

func projectAlloyDBAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("database_version", getStringField(data, "databaseVersion"))
	netRaw := getStringField(getStructField(data, "networkConfig"), "network")
	if netRaw == "" {
		netRaw = getStringField(data, "network")
	}
	p.setURI("vpc", netRaw, "compute.googleapis.com")
	p.setURI("kms_key_uri", getStringField(getStructField(data, "encryptionConfig"), "kmsKeyName"), "cloudkms.googleapis.com")
}

func projectSpannerAttrs(p *attrProjector, data *structpb.Struct) {
	p.setInt("node_count", getIntField(data, "nodeCount"))
	p.setInt("processing_units", getIntField(data, "processingUnits"))
	p.setStr("config", shortResourceName(getStringField(data, "config")))
}

func projectFirestoreAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("database_type", getStringField(data, "type"))
	p.setStr("concurrency_mode", getStringField(data, "concurrencyMode"))
	p.setURI("kms_key_uri", getStringField(getStructField(data, "cmekConfig"), "kmsKeyName"), "cloudkms.googleapis.com")
}

func projectBigtableAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("instance_type", getStringField(data, "type"))
}

func projectRedisInstanceAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("tier", getStringField(data, "tier"))
	p.setInt("memory_size_gb", getIntField(data, "memorySizeGb"))
	p.setStr("redis_version", getStringField(data, "redisVersion"))
	p.setStr("private_ip", parsePrivateIPv4(getStringField(data, "host")))
	p.setInt("port", getIntField(data, "port"))
	p.setURI("vpc", getStringField(data, "authorizedNetwork"), "compute.googleapis.com")
	p.setURI("kms_key_uri", getStringField(data, "customerManagedKey"), "cloudkms.googleapis.com")
}

func projectRedisClusterAttrs(p *attrProjector, data *structpb.Struct) {
	p.setInt("shard_count", getIntField(data, "shardCount"))
	p.setStr("node_type", getStringField(data, "nodeType"))
	for _, pcVal := range getListField(data, "pscConfigs") {
		if pc := pcVal.GetStructValue(); pc != nil {
			p.setURI("vpc", getStringField(pc, "network"), "compute.googleapis.com")
			return
		}
	}
}

func projectMemcacheAttrs(p *attrProjector, data *structpb.Struct) {
	p.setInt("node_count", getIntField(data, "nodeCount"))
	if nc := getStructField(data, "nodeConfig"); nc != nil {
		p.setInt("memory_size_mb", getIntField(nc, "memorySizeMb"))
	}
	p.setURI("vpc", getStringField(data, "authorizedNetwork"), "compute.googleapis.com")
}

func projectGCSBucketAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("storage_class", getStringField(data, "storageClass"))
	p.setStr("location_type", getStringField(data, "locationType"))
	if iamCfg := getStructField(data, "iamConfiguration"); iamCfg != nil {
		p.setBool("uniform_bucket_level_access", getBoolValue(getStructField(iamCfg, "uniformBucketLevelAccess"), "enabled"))
	}
	p.setURI("kms_key_uri", getStringField(getStructField(data, "encryption"), "defaultKmsKeyName"), "cloudkms.googleapis.com")
}

func projectFilestoreAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("tier", getStringField(data, "tier"))
	for _, nVal := range getListField(data, "networks") {
		if n := nVal.GetStructValue(); n != nil {
			p.setURI("vpc", getStringField(n, "network"), "compute.googleapis.com")
			if ips := getStringListField(n, "ipAddresses"); len(ips) > 0 {
				p.setStr("private_ip", parsePrivateIPv4(ips[0]))
			}
			break
		}
	}
	p.setURI("kms_key_uri", getStringField(data, "kmsKeyName"), "cloudkms.googleapis.com")
}

func projectNetAppAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("service_level", getStringField(data, "serviceLevel"))
	p.setInt("capacity_gib", getIntField(data, "capacityGib"))
	p.setURI("vpc", getStringField(data, "network"), "compute.googleapis.com")
	p.setURI("kms_key_uri", getStringField(data, "kmsConfig"), "cloudkms.googleapis.com")
}

func projectBQDatasetAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("location", getStringField(data, "location"))
	p.setURI("kms_key_uri", getStringField(getStructField(data, "defaultEncryptionConfiguration"), "kmsKeyName"), "cloudkms.googleapis.com")
}

func projectBQConnectionAttrs(p *attrProjector, data *structpb.Struct) {
	switch {
	case getStructField(data, "cloudSql") != nil:
		p.attrs["connection_type"] = "cloud_sql"
	case getStructField(data, "cloudSpanner") != nil:
		p.attrs["connection_type"] = "cloud_spanner"
	case getStructField(data, "cloudResource") != nil:
		p.attrs["connection_type"] = "cloud_resource"
	}
}

func projectBQTransferAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("data_source_id", getStringField(data, "dataSourceId"))
	p.setStr("schedule", getStringField(data, "schedule"))
}

func projectArtifactRegistryAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("format", getStringField(data, "format"))
	p.setStr("mode", getStringField(data, "mode"))
	p.setURI("kms_key_uri", getStringField(data, "kmsKeyName"), "cloudkms.googleapis.com")
}

func projectCloudBuildTriggerAttrs(p *attrProjector, data *structpb.Struct) {
	repoURL, filter := extractCloudBuildTriggerRepo(data)
	p.setStr("repo_url", repoURL)
	p.setStr("trigger_filter", filter)
	p.setStr("filename", getStringField(data, "filename"))
	sa := getStringField(data, "serviceAccount")
	if strings.Contains(sa, "/") {
		sa = shortResourceName(sa)
	}
	p.setStr("service_account", sa)
	p.setBool("disabled", getBoolValue(data, "disabled"))
}

func projectCloudDeployAttrs(p *attrProjector, data *structpb.Struct) {
	p.setBool("suspended", getBoolValue(data, "suspended"))
}

func projectCloudRunServiceAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("ingress", getStringField(data, "ingress"))
	u := getStringField(data, "uri")
	if u == "" {
		u = getStringField(getStructField(data, "status"), "url")
	}
	p.setStr("url", SanitizeURL(u))
	tpl := getStructField(data, "template")
	p.setStr("service_account", getStringField(tpl, "serviceAccount"))
	p.setURI("kms_key_uri", getStringField(tpl, "encryptionKey"), "cloudkms.googleapis.com")
	if vpc := getStructField(tpl, "vpcAccess"); vpc != nil {
		p.setURI("vpc_connector", getStringField(vpc, "connector"), "vpcaccess.googleapis.com")
		p.setFirstNICNetwork(vpc)
	}
}

func projectCloudRunJobAttrs(p *attrProjector, data *structpb.Struct) {
	inner := getStructField(getStructField(data, "template"), "template")
	p.setStr("service_account", getStringField(inner, "serviceAccount"))
	if vpc := getStructField(inner, "vpcAccess"); vpc != nil {
		p.setURI("vpc_connector", getStringField(vpc, "connector"), "vpcaccess.googleapis.com")
		p.setFirstNICNetwork(vpc)
	}
}

func projectCloudFunctionAttrs(p *attrProjector, data *structpb.Struct) {
	bc := getStructField(data, "buildConfig")
	sc := getStructField(data, "serviceConfig")
	rt := getStringField(bc, "runtime")
	if rt == "" {
		rt = getStringField(data, "runtime")
	}
	p.setStr("runtime", rt)
	ep := getStringField(bc, "entryPoint")
	if ep == "" {
		ep = getStringField(data, "entryPoint")
	}
	p.setStr("entry_point", ep)
	u := getStringField(sc, "uri")
	if u == "" {
		u = getStringField(getStructField(data, "httpsTrigger"), "url")
	}
	p.setStr("url", SanitizeURL(u))
	p.setStr("ingress", getStringField(sc, "ingressSettings"))
	sa := getStringField(sc, "serviceAccountEmail")
	if sa == "" {
		sa = getStringField(data, "serviceAccountEmail")
	}
	p.setStr("service_account", sa)
	p.setURI("vpc_connector", getStringField(sc, "vpcConnector"), "vpcaccess.googleapis.com")
	p.setURI("kms_key_uri", getStringField(data, "kmsKeyName"), "cloudkms.googleapis.com")
}

func projectAppEngineAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("service_account", getStringField(data, "serviceAccount"))
}

func projectBatchJobAttrs(p *attrProjector, data *structpb.Struct) {
	ap := getStructField(data, "allocationPolicy")
	p.setStr("service_account", getStringField(getStructField(ap, "serviceAccount"), "email"))
	if net := getStructField(ap, "network"); net != nil {
		p.setFirstNICNetwork(net)
	}
}

func projectPubSubTopicAttrs(p *attrProjector, data *structpb.Struct) {
	p.setURI("kms_key_uri", getStringField(data, "kmsKeyName"), "cloudkms.googleapis.com")
}

func projectPubSubSubAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("push_endpoint", SanitizeURL(getStringField(getStructField(data, "pushConfig"), "pushEndpoint")))
	p.setInt("ack_deadline_seconds", getIntField(data, "ackDeadlineSeconds"))
}

func projectManagedKafkaAttrs(p *attrProjector, data *structpb.Struct) {
	if capCfg := getStructField(data, "capacityConfig"); capCfg != nil {
		p.setInt("vcpu_count", getIntField(capCfg, "vcpuCount"))
	}
	p.setURI("kms_key_uri", getStringField(getStructField(data, "gcpConfig"), "kmsKey"), "cloudkms.googleapis.com")
}

func projectEventarcTriggerAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("service_account", getStringField(data, "serviceAccount"))
	for _, efVal := range getListField(data, "eventFilters") {
		if ef := efVal.GetStructValue(); ef != nil && getStringField(ef, "attribute") == "type" {
			p.setStr("event_type", getStringField(ef, "value"))
			return
		}
	}
}

func projectStateOnlyAttr(p *attrProjector, data *structpb.Struct) {
	p.setStr("state", getStringField(data, "state"))
}

func projectSchedulerJobAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("schedule", getStringField(data, "schedule"))
	p.setStr("state", getStringField(data, "state"))
	if ht := getStructField(data, "httpTarget"); ht != nil {
		sa := getStringField(getStructField(ht, "oidcToken"), "serviceAccountEmail")
		if sa == "" {
			sa = getStringField(getStructField(ht, "oauthToken"), "serviceAccountEmail")
		}
		p.setStr("service_account", sa)
	}
}

func projectWorkflowAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("state", getStringField(data, "state"))
	p.setStr("service_account", getStringField(data, "serviceAccount"))
}

func projectDataflowJobAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("job_type", getStringField(data, "type"))
	p.setStr("state", getStringField(data, "currentState"))
	p.setStr("service_account", getStringField(getStructField(data, "environment"), "serviceAccountEmail"))
}

func projectDataprocClusterAttrs(p *attrProjector, data *structpb.Struct) {
	cfg := getStructField(data, "config")
	gce := getStructField(cfg, "gceClusterConfig")
	p.setURI("vpc", getStringField(gce, "networkUri"), "compute.googleapis.com")
	p.setURI("subnet", getStringField(gce, "subnetworkUri"), "compute.googleapis.com")
	p.setStr("service_account", getStringField(gce, "serviceAccount"))
	p.setURI("kms_key_uri", getStringField(getStructField(cfg, "encryptionConfig"), "gcePdKmsKeyName"), "cloudkms.googleapis.com")
}

func projectDataprocBatchAttrs(p *attrProjector, data *structpb.Struct) {
	ec := getStructField(getStructField(data, "environmentConfig"), "executionConfig")
	p.setURI("vpc", getStringField(ec, "networkUri"), "compute.googleapis.com")
	p.setURI("subnet", getStringField(ec, "subnetworkUri"), "compute.googleapis.com")
	p.setStr("service_account", getStringField(ec, "serviceAccount"))
}

func projectMetastoreAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("tier", getStringField(data, "tier"))
	p.setURI("vpc", getStringField(data, "network"), "compute.googleapis.com")
}

func projectComposerAttrs(p *attrProjector, data *structpb.Struct) {
	cfg := getStructField(data, "config")
	p.setStr("image_version", getStringField(getStructField(cfg, "softwareConfig"), "imageVersion"))
	nc := getStructField(cfg, "nodeConfig")
	p.setURI("vpc", getStringField(nc, "network"), "compute.googleapis.com")
	p.setURI("subnet", getStringField(nc, "subnetwork"), "compute.googleapis.com")
	p.setStr("service_account", getStringField(nc, "serviceAccount"))
	p.setURI("kms_key_uri", getStringField(getStructField(cfg, "encryptionConfig"), "kmsKeyName"), "cloudkms.googleapis.com")
}

func projectDataformAttrs(p *attrProjector, data *structpb.Struct) {
	if grs := getStructField(data, "gitRemoteSettings"); grs != nil {
		p.setStr("repo_url", SanitizeURL(getStringField(grs, "url")))
	}
}

func projectDataFusionAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("type", getStringField(data, "type"))
	p.setStr("version", getStringField(data, "version"))
	p.setURI("vpc", getStringField(getStructField(data, "networkConfig"), "network"), "compute.googleapis.com")
}

func projectLookerAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("platform_edition", getStringField(data, "platformEdition"))
	p.setURI("vpc", getStringField(data, "consumerNetwork"), "compute.googleapis.com")
}

func projectDNSZoneAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("dns_name", getStringField(data, "dnsName"))
	p.setStr("visibility", getStringField(data, "visibility"))
}

func projectVPCConnectorAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("cidr", getStringField(data, "ipCidrRange"))
	p.setStr("machine_type", getStringField(data, "machineType"))
	p.setURI("vpc", getStringField(data, "network"), "compute.googleapis.com")
	sub := getStructField(data, "subnet")
	if sub == nil {
		return
	}
	subName := strings.TrimSpace(getStringField(sub, "name"))
	subProj := strings.TrimSpace(getStringField(sub, "projectId"))
	if subProj != "" {
		p.setStr("subnet_project", mapProject(subProj, p.projIdx))
	}
	if subName == "" {
		return
	}
	if subProj != "" && !strings.Contains(subName, "/") && p.region != "" {
		p.attrs["subnet"] = "//compute.googleapis.com/projects/" + mapProject(subProj, p.projIdx) + "/regions/" + p.region + "/subnetworks/" + subName
		return
	}
	p.setStr("subnet", subName)
}

func projectAPIGatewayAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("default_hostname", getStringField(data, "defaultHostname"))
	p.setStr("state", getStringField(data, "state"))
}

func projectApigeeAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("runtime_type", getStringField(data, "runtimeType"))
	p.setURI("vpc", getStringField(data, "authorizedNetwork"), "compute.googleapis.com")
}

func projectVertexEndpointAttrs(p *attrProjector, data *structpb.Struct) {
	p.setInt("deployed_model_count", len(getListField(data, "deployedModels")))
	p.setURI("vpc", getStringField(data, "network"), "compute.googleapis.com")
	p.setURI("kms_key_uri", getStringField(getStructField(data, "encryptionSpec"), "kmsKeyName"), "cloudkms.googleapis.com")
}

func projectVertexIndexEndpointAttrs(p *attrProjector, data *structpb.Struct) {
	p.setInt("deployed_index_count", len(getListField(data, "deployedIndexes")))
	p.setURI("vpc", getStringField(data, "network"), "compute.googleapis.com")
}

func projectVertexReasoningEngineAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("service_account", getStringField(getStructField(data, "spec"), "serviceAccount"))
}

func projectDiscoveryEngineAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("solution_type", getStringField(data, "solutionType"))
}

func projectDiscoveryDataStoreAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("industry_vertical", getStringField(data, "industryVertical"))
	p.setStr("content_config", getStringField(data, "contentConfig"))
}

func projectDialogflowAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("default_language_code", getStringField(data, "defaultLanguageCode"))
}

func projectDocumentAIAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("processor_type", getStringField(data, "type"))
	p.setStr("state", getStringField(data, "state"))
}

func projectServiceAccountAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("email", getStringField(data, "email"))
	p.setBool("disabled", getBoolValue(data, "disabled"))
}

func projectSecretAttrs(p *attrProjector, data *structpb.Struct) {
	rep := getStructField(data, "replication")
	if rep == nil {
		return
	}
	if getStructField(rep, "automatic") != nil {
		p.attrs["replication"] = "automatic"
	} else if getStructField(rep, "userManaged") != nil {
		p.attrs["replication"] = "user_managed"
	}
	if kms := getStringField(getStructField(getStructField(rep, "automatic"), "customerManagedEncryption"), "kmsKeyName"); kms != "" {
		p.setURI("kms_key_uri", kms, "cloudkms.googleapis.com")
	}
}

func projectCryptoKeyAttrs(p *attrProjector, data *structpb.Struct) {
	p.setStr("purpose", getStringField(data, "purpose"))
}

func projectLogBucketAttrs(p *attrProjector, data *structpb.Struct) {
	p.setInt("retention_days", getIntField(data, "retentionDays"))
}

func projectAlertPolicyAttrs(p *attrProjector, data *structpb.Struct) {
	if v, ok := getBoolField(data, "enabled"); ok {
		p.attrs["enabled"] = v
	}
}

// extractCloudBuildTriggerRepo returns sanitized repo_url and trigger_filter from a BuildTrigger.
func extractCloudBuildTriggerRepo(data *structpb.Struct) (repoURL, triggerFilter string) {
	if data == nil {
		return "", ""
	}
	if gh := getStructField(data, "github"); gh != nil {
		owner := strings.TrimSpace(getStringField(gh, "owner"))
		name := strings.TrimSpace(getStringField(gh, "name"))
		if owner != "" && name != "" {
			repoURL = "https://github.com/" + owner + "/" + name
		}
		return repoURL, extractPushOrPRFilter(gh)
	}
	if rec := getStructField(data, "repositoryEventConfig"); rec != nil {
		return strings.TrimSpace(getStringField(rec, "repository")), extractPushOrPRFilter(rec)
	}
	if s2b := getStructField(data, "sourceToBuild"); s2b != nil {
		return SanitizeURL(getStringField(s2b, "uri")), getStringField(s2b, "ref")
	}
	if tt := getStructField(data, "triggerTemplate"); tt != nil {
		repoURL = strings.TrimSpace(getStringField(tt, "repoName"))
		if br := getStringField(tt, "branchName"); br != "" {
			triggerFilter = "push:" + br
		} else if tag := getStringField(tt, "tagName"); tag != "" {
			triggerFilter = "tag:" + tag
		}
	}
	return repoURL, triggerFilter
}

func extractPushOrPRFilter(src *structpb.Struct) string {
	if push := getStructField(src, "push"); push != nil {
		if br := getStringField(push, "branch"); br != "" {
			return "push:" + br
		}
		if tag := getStringField(push, "tag"); tag != "" {
			return "tag:" + tag
		}
		return ""
	}
	if pr := getStructField(src, "pullRequest"); pr != nil {
		if br := getStringField(pr, "branch"); br != "" {
			return "pr:" + br
		}
	}
	return ""
}

// extractContainerAndInitRefs scans BOTH containers[] and initContainers[] (command, args, env)
// plus volumes[] on any K8s pod spec or Cloud Run template BEFORE raw payloads are discarded.
func extractContainerAndInitRefs(parent *structpb.Struct, projID string, projIdx map[string]string, envRefs, secretRefs, cloudSQL, configMaps *[]string) {
	if parent == nil {
		return
	}
	for _, field := range []string{"containers", "initContainers"} {
		for _, cVal := range getListField(parent, field) {
			if c := cVal.GetStructValue(); c != nil {
				extractSingleContainerRefs(c, projID, projIdx, envRefs, secretRefs, configMaps)
			}
		}
	}
	extractPodVolumeRefs(parent, projID, projIdx, secretRefs, cloudSQL, configMaps)
}

func extractSingleContainerRefs(c *structpb.Struct, projID string, projIdx map[string]string, envRefs, secretRefs, configMaps *[]string) {
	for _, cmd := range getStringListField(c, "command") {
		addExtractedRefs(cmd, projIdx, envRefs, secretRefs)
	}
	for _, arg := range getStringListField(c, "args") {
		addExtractedRefs(arg, projIdx, envRefs, secretRefs)
	}
	for _, efVal := range getListField(c, "envFrom") {
		if ef := efVal.GetStructValue(); ef != nil {
			if cm := getStructField(ef, "configMapRef"); cm != nil && configMaps != nil {
				appendUnique(configMaps, getStringField(cm, "name"))
			}
		}
	}
	for _, eVal := range getListField(c, "env") {
		if e := eVal.GetStructValue(); e != nil {
			extractContainerEnvEntry(e, projID, projIdx, envRefs, secretRefs, configMaps)
		}
	}
}

func extractContainerEnvEntry(e *structpb.Struct, projID string, projIdx map[string]string, envRefs, secretRefs, configMaps *[]string) {
	if val := getStringField(e, "value"); val != "" {
		addExtractedRefs(val, projIdx, envRefs, secretRefs)
	}
	if vs := getStructField(e, "valueSource"); vs != nil {
		if sec := getStringField(getStructField(vs, "secretKeyRef"), "secret"); sec != "" {
			appendUnique(secretRefs, normalizeSecretRef(sec, projID, projIdx))
		}
	}
	vf := getStructField(e, "valueFrom")
	if vf == nil {
		return
	}
	if sec := getStringField(getStructField(vf, "secretKeyRef"), "name"); sec != "" {
		// Cloud Run v1 secretKeyRef references Secret Manager; K8s secretKeyRef is local.
		if strings.Contains(sec, "/") || configMaps == nil {
			appendUnique(secretRefs, normalizeSecretRef(sec, projID, projIdx))
		}
	}
	if cmr := getStructField(vf, "configMapKeyRef"); cmr != nil && configMaps != nil {
		appendUnique(configMaps, getStringField(cmr, "name"))
	}
}

func extractPodVolumeRefs(parent *structpb.Struct, projID string, projIdx map[string]string, secretRefs, cloudSQL, configMaps *[]string) {
	for _, vVal := range getListField(parent, "volumes") {
		v := vVal.GetStructValue()
		if v == nil {
			continue
		}
		if cm := getStructField(v, "configMap"); cm != nil && configMaps != nil {
			appendUnique(configMaps, getStringField(cm, "name"))
		}
		if sec := getStructField(v, "secret"); sec != nil {
			name := getStringField(sec, "secret")
			if name == "" {
				name = getStringField(sec, "secretName")
			}
			if name != "" && (strings.Contains(name, "/") || configMaps == nil) {
				appendUnique(secretRefs, normalizeSecretRef(name, projID, projIdx))
			}
		}
		if csi := getStructField(v, "cloudSqlInstance"); csi != nil {
			for _, inst := range getStringListField(csi, "instances") {
				if uri := cloudSQLConnNameToURI(inst, projIdx); uri != "" {
					appendUnique(cloudSQL, uri)
				}
			}
		}
	}
}

// extractCloudBuildPayloadRefs scans Cloud Build steps[] (name, entrypoint, args, script),
// images[], and artifacts.objects.location via ExtractSanitizedRefs before raw payloads are discarded.
func extractCloudBuildPayloadRefs(build *structpb.Struct, projIdx map[string]string) (refs []string, secretRefs []string) {
	if build == nil {
		return nil, nil
	}
	for _, sVal := range getListField(build, "steps") {
		step := sVal.GetStructValue()
		if step == nil {
			continue
		}
		addExtractedRefs(getStringField(step, "name"), projIdx, &refs, &secretRefs)
		addExtractedRefs(getStringField(step, "entrypoint"), projIdx, &refs, &secretRefs)
		addExtractedRefs(getStringField(step, "script"), projIdx, &refs, &secretRefs)
		args := getStringListField(step, "args")
		for _, a := range args {
			addExtractedRefs(a, projIdx, &refs, &secretRefs)
		}
		// Join args so multi-token invocations like ["run", "deploy", "my-svc", "--project", "target-p"] match.
		if len(args) > 1 {
			joined := strings.Join(args, " ")
			if ep := getStringField(step, "entrypoint"); ep != "" {
				joined = ep + " " + joined
			} else if name := getStringField(step, "name"); strings.Contains(name, "gcloud") || strings.Contains(name, "cloud-sdk") {
				joined = "gcloud " + joined
			}
			addExtractedRefs(joined, projIdx, &refs, &secretRefs)
		}
	}
	for _, img := range getStringListField(build, "images") {
		addExtractedRefs(img, projIdx, &refs, &secretRefs)
	}
	if arts := getStructField(build, "artifacts"); arts != nil {
		if objs := getStructField(arts, "objects"); objs != nil {
			addExtractedRefs(getStringField(objs, "location"), projIdx, &refs, &secretRefs)
		}
	}
	sort.Strings(refs)
	sort.Strings(secretRefs)
	return refs, secretRefs
}

func addExtractedRefs(raw string, projIdx map[string]string, envRefs, secretRefs *[]string) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	eR, sR := ExtractSanitizedRefs(raw, projIdx)
	for _, r := range eR {
		appendUnique(envRefs, r)
	}
	for _, r := range sR {
		appendUnique(secretRefs, r)
	}
}

func extractEnvMapRefs(envMap *structpb.Struct, projIdx map[string]string, envRefs, secretRefs *[]string) {
	if envMap == nil {
		return
	}
	keys := make([]string, 0, len(envMap.GetFields()))
	for k := range envMap.GetFields() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		addExtractedRefs(envMap.GetFields()[k].GetStringValue(), projIdx, envRefs, secretRefs)
	}
}

// isDefaultSandboxAsset classifies GCP default/sandbox assets that should be suppressed
// when unreferenced (default VPC, default subnets, _Default/_Required LogBuckets, default SAs).
func isDefaultSandboxAsset(assetType, uri, displayName, networkAttr, emailAttr string) bool {
	switch assetType {
	case "compute.googleapis.com/Network":
		return shortResourceName(uri) == "default" || displayName == "default"
	case "compute.googleapis.com/Subnetwork":
		if shortResourceName(uri) != "default" && displayName != "default" {
			return false
		}
		return networkAttr == "" || strings.HasSuffix(strings.TrimRight(networkAttr, "/"), "/networks/default")
	case "logging.googleapis.com/LogBucket":
		name := shortResourceName(uri)
		if name == "" {
			name = displayName
		}
		return name == "_Default" || name == "_Required"
	case "iam.googleapis.com/ServiceAccount":
		email := emailAttr
		if email == "" {
			email = shortResourceName(uri)
		}
		return isDefaultServiceAccount(email)
	default:
		return false
	}
}

func isDefaultServiceAccount(email string) bool {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return false
	}
	if strings.HasSuffix(e, "-compute@developer.gserviceaccount.com") ||
		strings.HasSuffix(e, "@appspot.gserviceaccount.com") ||
		strings.HasSuffix(e, "@cloudservices.gserviceaccount.com") ||
		strings.HasSuffix(e, "@cloudbuild.gserviceaccount.com") {
		return true
	}
	if strings.HasPrefix(e, "service-") && strings.HasSuffix(e, ".iam.gserviceaccount.com") {
		local, _, ok := strings.Cut(e, "@")
		return ok && isNumeric(strings.TrimPrefix(local, "service-"))
	}
	return false
}

func regionFromLocation(loc string) string {
	s := strings.TrimSpace(shortResourceName(loc))
	if s == "" || s == "global" {
		return ""
	}
	if i := strings.LastIndexByte(s, '-'); i > 0 && len(s)-i == 2 && s[i+1] >= 'a' && s[i+1] <= 'z' {
		return s[:i]
	}
	return s
}

func parsePrivateIPv4(host string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil || !addr.Is4() {
		return ""
	}
	b := addr.As4()
	switch {
	case b[0] == 10:
		return addr.String()
	case b[0] == 172 && b[1] >= 16 && b[1] <= 31:
		return addr.String()
	case b[0] == 192 && b[1] == 168:
		return addr.String()
	case b[0] == 100 && b[1] >= 64 && b[1] <= 127:
		return addr.String()
	default:
		return ""
	}
}

func mapProject(id string, projIdx map[string]string) string {
	if mapped, ok := projIdx[id]; ok && mapped != "" {
		return mapped
	}
	return id
}

func getStructField(st *structpb.Struct, key string) *structpb.Struct {
	if st == nil {
		return nil
	}
	v, ok := st.Fields[key]
	if !ok || v == nil {
		return nil
	}
	return v.GetStructValue()
}

func getStringField(st *structpb.Struct, key string) string {
	if st == nil {
		return ""
	}
	v, ok := st.Fields[key]
	if !ok || v == nil {
		return ""
	}
	return v.GetStringValue()
}

func getBoolField(st *structpb.Struct, key string) (bool, bool) {
	if st == nil {
		return false, false
	}
	v, ok := st.Fields[key]
	if !ok || v == nil {
		return false, false
	}
	if bv, ok := v.Kind.(*structpb.Value_BoolValue); ok {
		return bv.BoolValue, true
	}
	return false, false
}

func getBoolValue(st *structpb.Struct, key string) bool {
	v, _ := getBoolField(st, key)
	return v
}

func getIntField(st *structpb.Struct, key string) int {
	if st == nil {
		return 0
	}
	v, ok := st.Fields[key]
	if !ok || v == nil {
		return 0
	}
	if nv, ok := v.Kind.(*structpb.Value_NumberValue); ok {
		return int(nv.NumberValue)
	}
	return 0
}

func getListField(st *structpb.Struct, key string) []*structpb.Value {
	if st == nil {
		return nil
	}
	v, ok := st.Fields[key]
	if !ok || v == nil || v.GetListValue() == nil {
		return nil
	}
	return v.GetListValue().GetValues()
}

func getStringListField(st *structpb.Struct, key string) []string {
	vals := getListField(st, key)
	if len(vals) == 0 {
		return nil
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if s := v.GetStringValue(); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func extractStringMap(st *structpb.Struct) map[string]string {
	if st == nil || len(st.GetFields()) == 0 {
		return nil
	}
	out := make(map[string]string, len(st.GetFields()))
	for k, v := range st.GetFields() {
		out[k] = v.GetStringValue()
	}
	return out
}

func normalizeSecretRef(raw, projID string, projIdx map[string]string) string {
	if strings.Contains(raw, "/") {
		return CanonicalizeAIP122WithService(raw, "secretmanager.googleapis.com", projIdx)
	}
	if projID != "" {
		return "//secretmanager.googleapis.com/projects/" + mapProject(projID, projIdx) + "/secrets/" + raw
	}
	return raw
}

func stripSecretVersion(uri string) string {
	if idx := strings.Index(uri, "/versions/"); idx > 0 {
		return uri[:idx]
	}
	return uri
}

func cloudSQLConnNameToURI(conn string, projIdx map[string]string) string {
	parts := strings.Split(strings.TrimSpace(conn), ":")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return ""
	}
	return "//sqladmin.googleapis.com/projects/" + mapProject(parts[0], projIdx) + "/instances/" + parts[2]
}

func parentBigQueryDatasetURI(canonURI string) string {
	for _, marker := range []string{"/tables/", "/routines/", "/models/"} {
		if idx := strings.Index(canonURI, marker); idx > 0 {
			return canonURI[:idx]
		}
	}
	return ""
}

func parentArtifactRepoURI(canonURI string) string {
	for _, marker := range []string{"/dockerImages/", "/packages/"} {
		if idx := strings.Index(canonURI, marker); idx > 0 {
			return canonURI[:idx]
		}
	}
	return ""
}

func extractGKEClusterURI(rawName, parentURI string, projIdx map[string]string) string {
	for _, candidate := range []string{rawName, parentURI} {
		canon := CanonicalizeAIP122(candidate, projIdx)
		if idx := strings.Index(canon, "/clusters/"); idx > 0 {
			rest := canon[idx+len("/clusters/"):]
			clusterName, _, _ := strings.Cut(rest, "/")
			if clusterName != "" {
				return canon[:idx+len("/clusters/")] + clusterName
			}
		}
	}
	return ""
}

func extractProjectID(canonURI string, ancestors []string, projIdx map[string]string) string {
	if p := extractSegmentAfter(canonURI, "projects"); p != "" {
		return mapProject(p, projIdx)
	}
	if p := extractSegmentAfter(canonURI, "apps"); p != "" {
		return mapProject(p, projIdx)
	}
	for _, anc := range ancestors {
		if strings.HasPrefix(anc, "projects/") {
			return mapProject(strings.TrimPrefix(anc, "projects/"), projIdx)
		}
	}
	return ""
}

func extractLocationFromURI(canonURI string) string {
	for _, key := range []string{"locations", "regions", "zones"} {
		if loc := extractSegmentAfter(canonURI, key); loc != "" {
			return loc
		}
	}
	if strings.Contains(canonURI, "/global/") {
		return "global"
	}
	return ""
}

func extractSegmentAfter(uri, key string) string {
	marker := "/" + key + "/"
	idx := strings.Index(uri, marker)
	if idx < 0 {
		return ""
	}
	rest := uri[idx+len(marker):]
	seg, _, _ := strings.Cut(rest, "/")
	return seg
}

func shortResourceName(uri string) string {
	s := strings.TrimRight(uri, "/")
	if idx := strings.LastIndex(s, "/"); idx >= 0 && idx+1 < len(s) {
		return s[idx+1:]
	}
	return s
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func appendUnique(slice *[]string, v string) {
	if v == "" {
		return
	}
	for _, existing := range *slice {
		if existing == v {
			return
		}
	}
	*slice = append(*slice, v)
}
