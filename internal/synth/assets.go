package synth

import (
	"strings"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

type assetSpec struct {
	componentKind manifest.ComponentKind
	roleTag       string
	baseLayer     int
	iconKey       string
}

var structuralContainerAssets = map[string]bool{
	"compute.googleapis.com/Network":    true,
	"compute.googleapis.com/Subnetwork": true,
	"container.googleapis.com/Cluster":  true,
}

var transversalAssetCategories = map[string]string{
	"iam.googleapis.com/ServiceAccount":     "IAM_SA",
	"cloudkms.googleapis.com/CryptoKey":     "KMS",
	"secretmanager.googleapis.com/Secret":   "SECRET",
	"logging.googleapis.com/LogSink":        "OBSERVABILITY",
	"logging.googleapis.com/LogBucket":      "OBSERVABILITY",
	"monitoring.googleapis.com/AlertPolicy": "OBSERVABILITY",
	"compute.googleapis.com/Router":         "NAT_ROUTER",
	"compute.googleapis.com/Firewall":       "FIREWALL_ARMOR",
	"compute.googleapis.com/Policy":         "FIREWALL_ARMOR",
	"compute.googleapis.com/SecurityPolicy": "FIREWALL_ARMOR",
	"compute.googleapis.com/Route":          "FIREWALL_ARMOR",
	"vpcaccess.googleapis.com/Connector":    "VPC_ACCESS",
}

var externalAssetSpecs = map[string]assetSpec{
	"external.internet/ClientGroup":               {manifest.KindExternalCaller, "external_caller", 0, "external_client"},
	"external.github.com/Repository":              {manifest.KindExternalCaller, "external_caller", 0, "github"},
	"external.onprem/Datacenter":                  {manifest.KindExternalCaller, "external_caller", 0, "cloud_interconnect"},
	"cloudresourcemanager.googleapis.com/Project": {manifest.KindExternalTarget, "external_target", 6, "project_cohort"},
	"compute.googleapis.com/Network":              {manifest.KindExternalTarget, "external_target", 6, "virtual_private_cloud"},
}

var defaultExternalAssetSpec = assetSpec{manifest.KindExternalTarget, "external_target", 6, "family_network"}
var defaultInternalAssetSpec = assetSpec{manifest.KindWorkload, "compute", 2, "family_compute"}

var internalAssetSpecs = map[string]assetSpec{
	"compute.googleapis.com/VpnGateway":                  {manifest.KindIngress, "hybrid", 0, "cloud_vpn"},
	"compute.googleapis.com/VpnTunnel":                   {manifest.KindIngress, "hybrid", 0, "cloud_vpn"},
	"compute.googleapis.com/InterconnectAttachment":      {manifest.KindIngress, "hybrid", 0, "cloud_interconnect"},
	"networkconnectivity.googleapis.com/Hub":             {manifest.KindIngress, "hybrid", 1, "network_connectivity_center"},
	"compute.googleapis.com/GlobalForwardingRule":        {manifest.KindIngress, "ingress", 1, "cloud_load_balancing"},
	"compute.googleapis.com/ForwardingRule":              {manifest.KindIngress, "ingress", 1, "cloud_load_balancing"},
	"compute.googleapis.com/BackendService":              {manifest.KindIngress, "ingress", 1, "cloud_load_balancing"},
	"compute.googleapis.com/ServiceAttachment":           {manifest.KindIngress, "ingress", 2, "private_service_connect"},
	"dns.googleapis.com/ManagedZone":                     {manifest.KindIngress, "ingress", 1, "cloud_dns"},
	"apigateway.googleapis.com/Gateway":                  {manifest.KindIngress, "ingress", 1, "api_gateway"},
	"apigee.googleapis.com/Organization":                 {manifest.KindIngress, "ingress", 1, "apigee_api_platform"},
	"networking.k8s.io/Ingress":                          {manifest.KindIngress, "ingress", 1, "k8s_ingress"},
	"cloudbuild.googleapis.com/BuildTrigger":             {manifest.KindPipeline, "cicd", 1, "cloud_build"},
	"clouddeploy.googleapis.com/DeliveryPipeline":        {manifest.KindPipeline, "cicd", 2, "cloud_deploy"},
	"cloudscheduler.googleapis.com/Job":                  {manifest.KindPipeline, "cicd", 1, "cloud_scheduler"},
	"apps.k8s.io/Deployment":                             {manifest.KindWorkload, "compute", 2, "k8s_workload"},
	"k8s.io/Service":                                     {manifest.KindWorkload, "compute", 2, "k8s_workload"},
	"batch.k8s.io/CronJob":                               {manifest.KindWorkload, "compute", 2, "batch"},
	"batch.googleapis.com/Job":                           {manifest.KindWorkload, "compute", 2, "batch"},
	"appengine.googleapis.com/Service":                   {manifest.KindWorkload, "compute", 2, "app_engine"},
	"run.googleapis.com/Service":                         {manifest.KindWorkload, "compute", 2, "cloud_run"},
	"run.googleapis.com/Job":                             {manifest.KindWorkload, "compute", 2, "cloud_run"},
	"cloudfunctions.googleapis.com/CloudFunction":        {manifest.KindWorkload, "compute", 2, "cloud_functions"},
	"compute.googleapis.com/Instance":                    {manifest.KindWorkload, "compute", 2, "compute_engine"},
	"compute.googleapis.com/InstanceGroupManager":        {manifest.KindWorkload, "compute", 2, "compute_engine"},
	"workflows.googleapis.com/Workflow":                  {manifest.KindPipeline, "pipeline", 2, "workflows"},
	"eventarc.googleapis.com/Trigger":                    {manifest.KindMessaging, "async", 2, "eventarc"},
	"cloudtasks.googleapis.com/Queue":                    {manifest.KindMessaging, "async", 3, "cloud_tasks"},
	"managedkafka.googleapis.com/Cluster":                {manifest.KindMessaging, "async", 3, "managed_kafka"},
	"pubsub.googleapis.com/Topic":                        {manifest.KindMessaging, "async", 3, "pubsub"},
	"pubsub.googleapis.com/Subscription":                 {manifest.KindMessaging, "async", 3, "pubsub"},
	"dataflow.googleapis.com/Job":                        {manifest.KindPipeline, "pipeline", 3, "dataflow"},
	"datastream.googleapis.com/Stream":                   {manifest.KindPipeline, "pipeline", 3, "datastream"},
	"composer.googleapis.com/Environment":                {manifest.KindPipeline, "pipeline", 3, "cloud_composer"},
	"dataproc.googleapis.com/Batch":                      {manifest.KindPipeline, "pipeline", 3, "dataproc"},
	"dataproc.googleapis.com/Cluster":                    {manifest.KindPipeline, "pipeline", 3, "dataproc"},
	"dataform.googleapis.com/Repository":                 {manifest.KindPipeline, "data", 3, "dataform"},
	"datafusion.googleapis.com/Instance":                 {manifest.KindPipeline, "pipeline", 3, "cloud_data_fusion"},
	"bigquerydatatransfer.googleapis.com/TransferConfig": {manifest.KindPipeline, "pipeline", 3, "data_transfer"},
	"bigqueryconnection.googleapis.com/Connection":       {manifest.KindPipeline, "data", 4, "bigquery"},
	"looker.googleapis.com/Instance":                     {manifest.KindWorkload, "data", 6, "looker"},
	"artifactregistry.googleapis.com/Repository":         {manifest.KindDatastore, "cicd", 3, "artifact_registry"},
	"sqladmin.googleapis.com/Instance":                   {manifest.KindDatastore, "data", 5, "cloud_sql"},
	"alloydb.googleapis.com/Cluster":                     {manifest.KindDatastore, "data", 5, "alloydb"},
	"alloydb.googleapis.com/Instance":                    {manifest.KindDatastore, "data", 5, "alloydb"},
	"redis.googleapis.com/Instance":                      {manifest.KindDatastore, "data", 5, "memorystore"},
	"redis.googleapis.com/Cluster":                       {manifest.KindDatastore, "data", 5, "memorystore"},
	"firestore.googleapis.com/Database":                  {manifest.KindDatastore, "data", 5, "firestore"},
	"file.googleapis.com/Instance":                       {manifest.KindDatastore, "data", 5, "filestore"},
	"storage.googleapis.com/Bucket":                      {manifest.KindDatastore, "data", 5, "cloud_storage"},
	"bigquery.googleapis.com/Dataset":                    {manifest.KindDatastore, "data", 5, "bigquery"},
	"dataplex.googleapis.com/Lake":                       {manifest.KindDatastore, "data", 5, "dataplex"},
	"metastore.googleapis.com/Service":                   {manifest.KindDatastore, "data", 5, "dataproc_metastore"},
	"spanner.googleapis.com/Instance":                    {manifest.KindDatastore, "data", 5, "cloud_spanner"},
	"spanner.googleapis.com/Database":                    {manifest.KindDatastore, "data", 5, "cloud_spanner"},
	"bigtableadmin.googleapis.com/Instance":              {manifest.KindDatastore, "data", 5, "cloud_bigtable"},
	"aiplatform.googleapis.com/ReasoningEngine":          {manifest.KindWorkload, "ai", 3, "vertex_agent_engine"},
	"aiplatform.googleapis.com/Endpoint":                 {manifest.KindWorkload, "ai", 4, "vertexai"},
	"aiplatform.googleapis.com/FeatureOnlineStore":       {manifest.KindDatastore, "ai", 5, "vertexai"},
	"aiplatform.googleapis.com/IndexEndpoint":            {manifest.KindDatastore, "ai", 5, "vertex_vector_search"},
	"aiplatform.googleapis.com/PublisherModel":           {manifest.KindDatastore, "ai", 5, "foundation_model"},
	"discoveryengine.googleapis.com/Engine":              {manifest.KindWorkload, "ai", 4, "vertex_search"},
	"discoveryengine.googleapis.com/DataStore":           {manifest.KindDatastore, "ai", 5, "vertex_search"},
	"dialogflow.googleapis.com/Agent":                    {manifest.KindWorkload, "ai", 2, "dialogflow"},
	"documentai.googleapis.com/Processor":                {manifest.KindPipeline, "ai", 3, "document_ai"},
}

func isStructuralContainerAsset(r extract.FactResource) bool {
	return !r.IsExternal && structuralContainerAssets[r.AssetType]
}

// classifyTransversalAsset returns (category, true) for resources separated into TransversalItems.
func classifyTransversalAsset(assetType string) (string, bool) {
	cat, ok := transversalAssetCategories[assetType]
	return cat, ok
}

func classifyAssetSpec(r extract.FactResource, reg *icons.Registry) assetSpec {
	var spec assetSpec
	if r.IsExternal {
		var ok bool
		if spec, ok = externalAssetSpecs[r.AssetType]; !ok {
			spec = defaultExternalAssetSpec
		}
	} else {
		var ok bool
		if spec, ok = internalAssetSpecs[r.AssetType]; !ok {
			spec = defaultInternalAssetSpec
		}
		applyRetryDLQRoleOverride(&spec, r)
	}
	if reg != nil && !reg.Has(spec.iconKey) && reg.Has("family_compute") {
		spec.iconKey = "family_compute"
	}
	return spec
}

func applyRetryDLQRoleOverride(spec *assetSpec, r extract.FactResource) {
	low := strings.ToLower(r.DisplayName)
	isDLQ := strings.Contains(low, "dlq") || strings.Contains(low, "dead_letter") || strings.Contains(low, "dead-letter")
	isRetry := strings.Contains(low, "replay") || strings.Contains(low, "reprocessor") || strings.Contains(low, "remediation")

	switch r.AssetType {
	case "apps.k8s.io/Deployment", "k8s.io/Service",
		"run.googleapis.com/Service", "run.googleapis.com/Job",
		"cloudfunctions.googleapis.com/CloudFunction":
		if isDLQ || isRetry {
			spec.roleTag = "retry"
			spec.baseLayer = 3
		}
	case "pubsub.googleapis.com/Topic", "pubsub.googleapis.com/Subscription":
		if isDLQ {
			spec.roleTag = "dlq"
			spec.baseLayer = 4
		}
	}
}
