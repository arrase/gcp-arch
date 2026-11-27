package extract

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/bigquery/v2"
	"google.golang.org/api/dataflow/v1b3"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxEnrichWorkers     = 8
	maxBQJobPages        = 20
	bqJobPageSize        = 1000
	enrichmentLookback   = 7 * 24 * time.Hour
	dailyAlignmentSecond = 86400

	bqJobListFields       = "jobs(user_email,principal_subject,configuration(query/destinationTable,load/destinationTable,copy(sourceTable,sourceTables,destinationTable)),statistics/query/referencedTables),nextPageToken"
	dataflowJobListFields = "jobs(id,location),nextPageToken"
	dataflowJobGetFields  = "id,name,type,currentState,location,environment(serviceAccountEmail,tempStoragePrefix,dataset,workerPools(network,subnetwork)),pipelineDescription(displayData(strValue,url),originalPipelineTransform(displayData(strValue,url)))"

	sourceBQJobs             = "bq_jobs"
	sourceDataflowJobs       = "dataflow_jobs"
	callerKindServiceAccount = "serviceaccount"
)

var (
	anonTableRe       = regexp.MustCompile(`^anon(?:_|[0-9a-f]{8,})`)
	principalSASubjRe = regexp.MustCompile(`^serviceAccount:([^\s]+@[^\s]+)$`)

	// companionAPIHostMap aliases GCP companion API hosts to their canonical service host
	// (salvaged from base_catalog.yaml).
	companionAPIHostMap = map[string]string{
		"storage-component.googleapis.com":   svcStorage,
		"storage-api.googleapis.com":         svcStorage,
		"bigquerystorage.googleapis.com":     svcBigQuery,
		"bigquerymigration.googleapis.com":   svcBigQuery,
		"containerfilesystem.googleapis.com": "container.googleapis.com",
		"bigtable.googleapis.com":            "bigtableadmin.googleapis.com",
	}

	transversalControlAPIs = map[string]bool{
		"modelarmor.googleapis.com":       true,
		"cloudtrace.googleapis.com":       true,
		"logging.googleapis.com":          true,
		"monitoring.googleapis.com":       true,
		"secretmanager.googleapis.com":    true,
		svcCloudKMS:                       true,
		"dlp.googleapis.com":              true,
		"artifactregistry.googleapis.com": true,
		"cloudbuild.googleapis.com":       true,
		"clouddeploy.googleapis.com":      true,
	}

	consumedAPITargetTypes = map[string][]string{
		svcStorage:                       {"storage.googleapis.com/Bucket"},
		svcBigQuery:                      {"bigquery.googleapis.com/Dataset"},
		"sqladmin.googleapis.com":        {"sqladmin.googleapis.com/Instance"},
		"alloydb.googleapis.com":         {"alloydb.googleapis.com/Cluster"},
		"spanner.googleapis.com":         {"spanner.googleapis.com/Instance"},
		"firestore.googleapis.com":       {"firestore.googleapis.com/Database"},
		"bigtableadmin.googleapis.com":   {"bigtableadmin.googleapis.com/Instance"},
		"redis.googleapis.com":           {"redis.googleapis.com/Instance", "redis.googleapis.com/Cluster"},
		"memcache.googleapis.com":        {"memcache.googleapis.com/Instance"},
		svcPubSub:                        {"pubsub.googleapis.com/Topic"},
		"cloudtasks.googleapis.com":      {"cloudtasks.googleapis.com/Queue"},
		"workflows.googleapis.com":       {"workflows.googleapis.com/Workflow"},
		svcRun:                           {assetTypeRunService},
		"cloudfunctions.googleapis.com":  {"cloudfunctions.googleapis.com/CloudFunction"},
		"discoveryengine.googleapis.com": {"discoveryengine.googleapis.com/Engine", "discoveryengine.googleapis.com/DataStore"},
		"dialogflow.googleapis.com":      {"dialogflow.googleapis.com/Agent"},
		"documentai.googleapis.com":      {"documentai.googleapis.com/Processor"},
	}

	// dataPlaneReadMethods lists data-plane reads (lowercased method leaf or "<collection>.<verb>")
	// that carry real runtime traffic and must never be filtered as read-only discovery. Reads that
	// do not look like get*/list*/batchget*/watch*/poll* are listed too, so the intent stays explicit.
	dataPlaneReadMethods = map[string]bool{
		// Cloud Storage (JSON API and gRPC).
		"objects.get": true, "objects.list": true, "getobject": true, "readobject": true, "listobjects": true,
		// Firestore.
		"listen": true, "batchgetdocuments": true, "getdocument": true, "listdocuments": true, "runquery": true, "runaggregationquery": true,
		// Datastore.
		"lookup": true,
		// Bigtable and BigQuery Storage.
		"readrows": true,
		// Spanner.
		"read": true, "streamingread": true, "executesql": true, "executestreamingsql": true,
		// BigQuery.
		"getqueryresults": true, "tabledata.list": true, "tabledataservice.list": true,
		// Pub/Sub.
		"pull": true, "streamingpull": true,
	}
)

type bqDatasetEdgeKey struct {
	fromDS string
	toDS   string
}

type bqPrincipalDSKey struct {
	email string
	dsURI string
}

type bqEnrichResult struct {
	lineageEdges []bqDatasetEdgeKey
	principalDS  []bqPrincipalDSKey
}

type dataflowJobLink struct {
	projectID      string
	location       string
	jobID          string
	jobName        string
	jobType        string
	state          string
	serviceAccount string
	networkURI     string
	subnetURI      string
	resourceURIs   []string
}

type consumedAPIKey struct {
	proj, saID, svc, method, kind string
}

type consumedAPIStat struct {
	projectID   string
	saUniqueID  string
	serviceHost string
	method      string
	callerKind  string
	count       int64
}

type publisherModelKey struct {
	proj, loc, pub, model string
}

type publisherModelStat struct {
	projectID           string
	location            string
	publisher           string
	modelID             string
	invocations         int64
	dedicatedThroughput bool
}

type monitoringResult struct {
	consumed        []*consumedAPIStat
	publisherModels []*publisherModelStat
}

// CanonicalServiceHost maps companion API hosts (e.g. bigquerystorage, storage-api) to
// their canonical service host.
func CanonicalServiceHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if mapped, ok := companionAPIHostMap[h]; ok {
		return mapped
	}
	return h
}

// IsReadOnlyDiscoveryMethod reports whether a consumed_api resource.labels.method represents
// a read-only discovery, listing, IAM check, or watch/poll method (case-insensitive) that
// should be excluded from runtime MONITORING_TRAFFIC attribution (e.g., terraform plan refreshes).
// Data-plane reads (isDataPlaneReadMethod) are never treated as discovery.
func IsReadOnlyDiscoveryMethod(method string) bool {
	s := strings.TrimSpace(method)
	if s == "" || isDataPlaneReadMethod(s) {
		return false
	}
	if idx := strings.LastIndexAny(s, "./"); idx >= 0 && idx+1 < len(s) {
		s = s[idx+1:]
	}
	leaf := strings.ToLower(s)
	return strings.HasPrefix(leaf, "get") ||
		strings.HasPrefix(leaf, "list") ||
		strings.HasPrefix(leaf, "aggregatedlist") ||
		strings.HasPrefix(leaf, "batchget") ||
		leaf == "testiampermissions" ||
		strings.HasSuffix(leaf, "testiampermissions") ||
		leaf == "watch" || strings.HasPrefix(leaf, "watch") ||
		leaf == "poll" || strings.HasPrefix(leaf, "poll")
}

// isDataPlaneReadMethod reports whether method's lowercased leaf or "<collection>.<verb>" suffix is a
// data-plane read in dataPlaneReadMethods.
func isDataPlaneReadMethod(method string) bool {
	segs := strings.FieldsFunc(strings.ToLower(method), func(r rune) bool { return r == '.' || r == '/' })
	n := len(segs)
	return n > 0 && (dataPlaneReadMethods[segs[n-1]] || (n > 1 && dataPlaneReadMethods[segs[n-2]+"."+segs[n-1]]))
}

func (st *extractState) runEnrichers(ctx context.Context, clients *Clients) error {
	var (
		bqRes   *bqEnrichResult
		dfLinks []*dataflowJobLink
		monRes  *monitoringResult
	)

	g, gctx := errgroup.WithContext(ctx)
	if clients.BigQuery != nil {
		g.Go(func() error {
			var err error
			bqRes, err = st.fetchBigQueryJobs(gctx, clients)
			return err
		})
	}
	if clients.Dataflow != nil {
		g.Go(func() error {
			var err error
			dfLinks, err = st.fetchDataflowJobs(gctx, clients)
			return err
		})
	}
	if clients.Monitoring != nil {
		g.Go(func() error {
			var err error
			monRes, err = st.fetchMonitoring(gctx, clients)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// Apply results sequentially on the main goroutine for determinism and zero data races.
	st.applyDataflowLinks(dfLinks)
	if bqRes != nil || monRes != nil {
		saWorkloads := st.indexSAEmailToWorkloads()
		st.applyBigQueryResult(bqRes, saWorkloads)
		st.applyMonitoringResult(monRes, saWorkloads)
	}
	return nil
}

func (st *extractState) enabledProjects(candidates []string, serviceHost string) []string {
	projects := make([]string, 0, len(candidates))
	for _, proj := range candidates {
		if st.isServiceMaybeEnabled(proj, serviceHost) {
			projects = append(projects, proj)
		}
	}
	return projects
}

func (st *extractState) fetchBigQueryJobs(ctx context.Context, clients *Clients) (*bqEnrichResult, error) {
	projects := st.enabledProjects(st.allDiscoveredProjects(), svcBigQuery)
	if len(projects) == 0 {
		return nil, nil
	}

	endTime := st.effectiveReadTime()
	minCreationMs := uint64(endTime.Add(-enrichmentLookback).UnixMilli())

	var (
		mu           sync.Mutex
		lineageSet   = make(map[bqDatasetEdgeKey]struct{})
		principalSet = make(map[bqPrincipalDSKey]struct{})
		completed    atomic.Int32
	)
	total := len(projects)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxEnrichWorkers)

	for _, proj := range projects {
		g.Go(func() error {
			localLineage, localPrincipal, err := st.fetchProjectBigQueryJobs(gctx, clients, proj, minCreationMs)
			if err != nil {
				return err
			}
			if len(localLineage) > 0 || len(localPrincipal) > 0 {
				mu.Lock()
				maps.Copy(lineageSet, localLineage)
				maps.Copy(principalSet, localPrincipal)
				mu.Unlock()
			}
			done := completed.Add(1)
			clients.ReportProgress(false, "Enrich BigQuery jobs: %d/%d projects", done, total)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if len(lineageSet) == 0 && len(principalSet) == 0 {
		return nil, nil
	}
	return buildBQEnrichResult(lineageSet, principalSet), nil
}

func buildBQEnrichResult(lineageSet map[bqDatasetEdgeKey]struct{}, principalSet map[bqPrincipalDSKey]struct{}) *bqEnrichResult {
	lineageEdges := slices.Collect(maps.Keys(lineageSet))
	slices.SortFunc(lineageEdges, func(a, b bqDatasetEdgeKey) int {
		return cmp.Or(cmp.Compare(a.fromDS, b.fromDS), cmp.Compare(a.toDS, b.toDS))
	})
	principalDS := slices.Collect(maps.Keys(principalSet))
	slices.SortFunc(principalDS, func(a, b bqPrincipalDSKey) int {
		return cmp.Or(cmp.Compare(a.email, b.email), cmp.Compare(a.dsURI, b.dsURI))
	})
	return &bqEnrichResult{
		lineageEdges: lineageEdges,
		principalDS:  principalDS,
	}
}

type bqProjectFetch struct {
	clients       *Clients
	proj          string
	projNum       string
	scope         string
	minCreationMs uint64
	lineage       map[bqDatasetEdgeKey]struct{}
	principals    map[bqPrincipalDSKey]struct{}
}

func (st *extractState) fetchProjectBigQueryJobs(
	ctx context.Context,
	clients *Clients,
	proj string,
	minCreationMs uint64,
) (map[bqDatasetEdgeKey]struct{}, map[bqPrincipalDSKey]struct{}, error) {
	fetch := &bqProjectFetch{
		clients:       clients,
		proj:          proj,
		projNum:       st.projectNumberFor(proj),
		scope:         prefixProjects + proj,
		minCreationMs: minCreationMs,
		lineage:       make(map[bqDatasetEdgeKey]struct{}),
		principals:    make(map[bqPrincipalDSKey]struct{}),
	}
	pageToken := ""

	for page := 1; page <= maxBQJobPages; page++ {
		nextToken, degraded, err := st.fetchProjectBigQueryPage(ctx, fetch, pageToken)
		if err != nil {
			return nil, nil, err
		}
		if degraded {
			return fetch.lineage, fetch.principals, nil
		}
		pageToken = nextToken
		if pageToken == "" {
			st.addCoverage(CoverageEntry{Scope: fetch.scope, Source: sourceBQJobs, State: CoverageOK})
			return fetch.lineage, fetch.principals, nil
		}
	}
	st.addCoverage(CoverageEntry{
		Scope:  fetch.scope,
		Source: sourceBQJobs,
		State:  CoverageTruncatingCap,
		Detail: fmt.Sprintf("reached %d-page BigQuery jobs.list cap within 7-day window", maxBQJobPages),
	})
	return fetch.lineage, fetch.principals, nil
}

func (st *extractState) fetchProjectBigQueryPage(
	ctx context.Context,
	fetch *bqProjectFetch,
	pageToken string,
) (nextToken string, degraded bool, err error) {
	resp, listErr := listBigQueryJobsPage(ctx, fetch.clients, fetch.proj, fetch.minCreationMs, pageToken)
	if listErr != nil {
		deg, fatalErr := st.handleEnrichError(listErr, fetch.clients, fetch.proj, fetch.projNum, fetch.scope, sourceBQJobs)
		if fatalErr != nil {
			return "", false, fatalErr
		}
		if deg {
			return "", true, nil
		}
		return "", false, fmt.Errorf("bigquery jobs.list(%s): %w", fetch.proj, listErr)
	}
	for _, job := range resp.Jobs {
		recordBQJob(job, st.projIdx, fetch.lineage, fetch.principals)
	}
	return resp.NextPageToken, false, nil
}

func listBigQueryJobsPage(ctx context.Context, clients *Clients, proj string, minCreationMs uint64, pageToken string) (*bigquery.JobList, error) {
	var resp *bigquery.JobList
	err := retryOnRateLimit(ctx, clients.retryBackoff, func() error {
		if err := waitLimiter(ctx, clients.BQLimiter); err != nil {
			return err
		}
		call := clients.BigQuery.Jobs.List(proj).
			AllUsers(true).
			Projection("full").
			MinCreationTime(minCreationMs).
			MaxResults(bqJobPageSize).
			Fields(googleapi.Field(bqJobListFields)).
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		var doErr error
		resp, doErr = call.Do()
		return doErr
	})
	return resp, err
}

func recordBQJob(
	job *bigquery.JobListJobs,
	projIdx map[string]string,
	lineage map[bqDatasetEdgeKey]struct{},
	principals map[bqPrincipalDSKey]struct{},
) {
	if job == nil {
		return
	}
	destDS, refDatasets := extractBQJobDatasets(job, projIdx)
	if len(refDatasets) == 0 && destDS == "" {
		return
	}

	email := extractPrincipalEmail(job.PrincipalSubject, job.UserEmail)
	for _, refDS := range refDatasets {
		if destDS != "" && refDS != destDS {
			lineage[bqDatasetEdgeKey{fromDS: refDS, toDS: destDS}] = struct{}{}
		}
		if email != "" {
			principals[bqPrincipalDSKey{email: email, dsURI: refDS}] = struct{}{}
		}
	}
	if destDS != "" && email != "" {
		principals[bqPrincipalDSKey{email: email, dsURI: destDS}] = struct{}{}
	}
}

func extractBQJobDatasets(job *bigquery.JobListJobs, projIdx map[string]string) (string, []string) {
	var refDatasets []string
	if job.Statistics != nil && job.Statistics.Query != nil {
		for _, tr := range job.Statistics.Query.ReferencedTables {
			appendUnique(&refDatasets, canonicalBQDatasetRef(tr, projIdx))
		}
	}
	destDS, copyRefs := extractBQJobConfigDatasets(job.Configuration, projIdx)
	for _, ds := range copyRefs {
		appendUnique(&refDatasets, ds)
	}
	return destDS, refDatasets
}

func extractBQJobConfigDatasets(cfg *bigquery.JobConfiguration, projIdx map[string]string) (string, []string) {
	if cfg == nil {
		return "", nil
	}
	switch {
	case cfg.Query != nil && cfg.Query.DestinationTable != nil:
		return canonicalBQDatasetRef(cfg.Query.DestinationTable, projIdx), nil
	case cfg.Load != nil && cfg.Load.DestinationTable != nil:
		return canonicalBQDatasetRef(cfg.Load.DestinationTable, projIdx), nil
	case cfg.Copy != nil:
		return extractBQCopyDatasets(cfg.Copy, projIdx)
	}
	return "", nil
}

func extractBQCopyDatasets(cp *bigquery.JobConfigurationTableCopy, projIdx map[string]string) (string, []string) {
	dest := canonicalBQDatasetRef(cp.DestinationTable, projIdx)
	var refs []string
	appendUnique(&refs, canonicalBQDatasetRef(cp.SourceTable, projIdx))
	for _, tr := range cp.SourceTables {
		appendUnique(&refs, canonicalBQDatasetRef(tr, projIdx))
	}
	return dest, refs
}

func canonicalBQDatasetRef(tr *bigquery.TableReference, projIdx map[string]string) string {
	if tr == nil || tr.ProjectId == "" || tr.DatasetId == "" || tr.TableId == "" {
		return ""
	}
	if strings.HasPrefix(tr.DatasetId, "_") || strings.HasPrefix(tr.TableId, "_script") || anonTableRe.MatchString(tr.TableId) {
		return ""
	}
	return prefixBQProjects + mapProject(tr.ProjectId, projIdx) + segDatasets + tr.DatasetId
}

func (st *extractState) applyBigQueryResult(res *bqEnrichResult, saWorkloads map[string][]string) {
	if res == nil {
		return
	}
	for _, e := range res.lineageEdges {
		st.emitTargetEdge(e.fromDS, e.toDS, EdgeBQLineage, "sql_elt", oosConfigRefCrossProject)
	}
	for _, pd := range res.principalDS {
		srcURI, heuristic := st.resolvePrincipalSource(pd.email, saWorkloads)
		if srcURI == "" {
			continue
		}
		if from, to, ok := st.resolveEdgeEndpoints(srcURI, pd.dsURI, oosConfigRefCrossProject); ok {
			st.putEdge(FactEdge{FromURI: from, ToURI: to, Kind: EdgeBQLineage, Detail: "bq_job", Heuristic: heuristic})
		}
	}
}

// resolvePrincipalSource attributes a Service Account's activity to its dedicated workload when exactly
// one workload runs as it; otherwise (0 or >1 workloads) to the unsuppressed Service Account itself.
// heuristic is true when >1 workloads share the Service Account.
func (st *extractState) resolvePrincipalSource(email string, saWorkloads map[string][]string) (string, bool) {
	wls := saWorkloads[email]
	if len(wls) == 1 {
		return wls[0], false
	}
	if sa := st.saByEmail[email]; sa != nil && !sa.suppressed {
		return sa.uri, len(wls) > 1
	}
	return "", false
}

func extractPrincipalEmail(principalSubject, userEmail string) string {
	s := strings.TrimSpace(principalSubject)
	if m := principalSASubjRe.FindStringSubmatch(s); len(m) == 2 {
		return strings.ToLower(m[1])
	}
	if strings.HasPrefix(strings.ToLower(s), prefixServiceAccount) {
		return strings.ToLower(strings.TrimSpace(s[len(prefixServiceAccount):]))
	}
	return strings.ToLower(strings.TrimSpace(userEmail))
}

func (st *extractState) fetchDataflowJobs(ctx context.Context, clients *Clients) ([]*dataflowJobLink, error) {
	projects := st.enabledProjects(st.allDiscoveredProjects(), "dataflow.googleapis.com")
	if len(projects) == 0 {
		return nil, nil
	}

	var (
		mu        sync.Mutex
		links     []*dataflowJobLink
		completed atomic.Int32
	)
	total := len(projects)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxEnrichWorkers)

	for _, proj := range projects {
		g.Go(func() error {
			projLinks, err := st.fetchProjectDataflowJobs(gctx, clients, proj)
			if err != nil {
				return err
			}
			if len(projLinks) > 0 {
				mu.Lock()
				links = append(links, projLinks...)
				mu.Unlock()
			}
			done := completed.Add(1)
			clients.ReportProgress(false, "Enrich Dataflow jobs: %d/%d projects", done, total)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	slices.SortFunc(links, func(a, b *dataflowJobLink) int {
		return cmp.Or(cmp.Compare(a.projectID, b.projectID), cmp.Compare(a.jobID, b.jobID))
	})
	return links, nil
}

func (st *extractState) fetchProjectDataflowJobs(ctx context.Context, clients *Clients, proj string) ([]*dataflowJobLink, error) {
	scope := prefixProjects + proj
	projNum := st.projectNumberFor(proj)
	pageToken := ""
	degraded := false
	var projLinks []*dataflowJobLink

	for {
		resp, err := listDataflowJobsPage(ctx, clients, proj, pageToken)
		if err != nil {
			if deg, fatalErr := st.handleEnrichError(err, clients, proj, projNum, scope, sourceDataflowJobs); fatalErr != nil {
				return nil, fatalErr
			} else if deg {
				degraded = true
				break
			}
			return nil, fmt.Errorf("dataflow jobs.list(%s): %w", proj, err)
		}

		pageLinks, deg, err := st.fetchDataflowJobDetails(ctx, clients, proj, projNum, scope, resp.Jobs)
		if err != nil {
			return nil, err
		}
		projLinks = append(projLinks, pageLinks...)
		if deg {
			degraded = true
			break
		}
		pageToken = resp.NextPageToken
		if pageToken == "" {
			break
		}
	}
	if !degraded {
		st.addCoverage(CoverageEntry{Scope: scope, Source: sourceDataflowJobs, State: CoverageOK})
	}
	return projLinks, nil
}

func listDataflowJobsPage(ctx context.Context, clients *Clients, proj, pageToken string) (*dataflow.ListJobsResponse, error) {
	var resp *dataflow.ListJobsResponse
	err := retryOnRateLimit(ctx, clients.retryBackoff, func() error {
		if err := waitLimiter(ctx, clients.DataflowLimiter); err != nil {
			return err
		}
		call := clients.Dataflow.Projects.Locations.Jobs.List(proj, "-").
			Filter("ACTIVE").
			Fields(googleapi.Field(dataflowJobListFields)).
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		var doErr error
		resp, doErr = call.Do()
		return doErr
	})
	return resp, err
}

func (st *extractState) fetchDataflowJobDetails(ctx context.Context, clients *Clients, proj, projNum, scope string, summaries []*dataflow.Job) ([]*dataflowJobLink, bool, error) {
	var links []*dataflowJobLink
	for _, summary := range summaries {
		loc := summary.Location
		if loc == "" {
			loc = "us-central1"
		}
		detail, err := getDataflowJob(ctx, clients, proj, loc, summary.Id)
		if err != nil {
			if deg, fatalErr := st.handleEnrichError(err, clients, proj, projNum, scope, sourceDataflowJobs); fatalErr != nil {
				return nil, false, fatalErr
			} else if deg {
				return links, true, nil
			}
			return nil, false, fmt.Errorf("dataflow jobs.get(%s/%s/%s): %w", proj, loc, summary.Id, err)
		}
		links = append(links, extractDataflowJobLink(proj, loc, detail, st.projIdx))
	}
	return links, false, nil
}

func getDataflowJob(ctx context.Context, clients *Clients, proj, loc, jobID string) (*dataflow.Job, error) {
	var detail *dataflow.Job
	err := retryOnRateLimit(ctx, clients.retryBackoff, func() error {
		if err := waitLimiter(ctx, clients.DataflowLimiter); err != nil {
			return err
		}
		var doErr error
		detail, doErr = clients.Dataflow.Projects.Locations.Jobs.Get(proj, loc, jobID).
			View("JOB_VIEW_DESCRIPTION").
			Fields(googleapi.Field(dataflowJobGetFields)).
			Context(ctx).
			Do()
		return doErr
	})
	return detail, err
}

func extractDataflowJobLink(proj, loc string, job *dataflow.Job, projIdx map[string]string) *dataflowJobLink {
	link := &dataflowJobLink{
		projectID: proj,
		location:  loc,
		jobID:     job.Id,
		jobName:   job.Name,
		jobType:   job.Type,
		state:     job.CurrentState,
	}
	var refs []string
	extractDataflowEnvironment(link, job.Environment, projIdx, &refs)
	extractDataflowPipelineRefs(job.PipelineDescription, projIdx, &refs)
	slices.Sort(refs)
	link.resourceURIs = refs
	return link
}

func appendSanitizedRefs(dst *[]string, raw string, projIdx map[string]string) {
	if raw == "" {
		return
	}
	eRefs, _ := ExtractSanitizedRefs(raw, projIdx)
	for _, r := range eRefs {
		appendUnique(dst, r)
	}
}

func extractDataflowEnvironment(link *dataflowJobLink, env *dataflow.Environment, projIdx map[string]string, refs *[]string) {
	if env == nil {
		return
	}
	link.serviceAccount = env.ServiceAccountEmail
	appendSanitizedRefs(refs, env.TempStoragePrefix, projIdx)
	appendSanitizedRefs(refs, env.Dataset, projIdx)
	for _, wp := range env.WorkerPools {
		if wp.Network != "" && link.networkURI == "" {
			link.networkURI = normalizeComputeNetworkURI(wp.Network, link.projectID, projIdx)
		}
		if wp.Subnetwork != "" && link.subnetURI == "" {
			link.subnetURI = CanonicalizeAIP122WithService(wp.Subnetwork, svcCompute, projIdx)
		}
	}
}

func extractDataflowPipelineRefs(desc *dataflow.PipelineDescription, projIdx map[string]string, refs *[]string) {
	if desc == nil {
		return
	}
	appendDisplayDataRefs(refs, desc.DisplayData, projIdx)
	for _, tr := range desc.OriginalPipelineTransform {
		appendDisplayDataRefs(refs, tr.DisplayData, projIdx)
	}
}

func appendDisplayDataRefs(refs *[]string, items []*dataflow.DisplayData, projIdx map[string]string) {
	for _, dd := range items {
		appendSanitizedRefs(refs, dd.StrValue, projIdx)
		appendSanitizedRefs(refs, dd.Url, projIdx)
	}
}

func (st *extractState) applyDataflowLinks(links []*dataflowJobLink) {
	for _, dl := range links {
		a := st.ensureDataflowAsset(dl)
		if dl.serviceAccount != "" {
			st.markServiceAccountRef(a.uri, dl.projectID, dl.serviceAccount)
		}
		for _, ref := range dl.resourceURIs {
			st.resolveSingleConfigRef(a.uri, dl.projectID, dl.networkURI, ref, EdgeConfigRef, oosConfigRefCrossProject)
		}
	}
}

func (st *extractState) ensureDataflowAsset(dl *dataflowJobLink) *ingestedAsset {
	uri := "//dataflow.googleapis.com/projects/" + dl.projectID + segLocations + dl.location + "/jobs/" + dl.jobID
	if a := st.assetsByURI[uri]; a != nil {
		return a
	}
	dn := dl.jobName
	if dn == "" {
		dn = dl.jobID
	}
	parentURI := dl.subnetURI
	if parentURI == "" {
		parentURI = dl.networkURI
	}
	a := &ingestedAsset{
		uri:         uri,
		assetType:   "dataflow.googleapis.com/Job",
		projectID:   dl.projectID,
		location:    dl.location,
		displayName: dn,
		parentURI:   parentURI,
		attrs:       buildDataflowAttrs(dl),
	}
	st.assets = append(st.assets, a)
	st.assetsByURI[uri] = a
	return a
}

func buildDataflowAttrs(dl *dataflowJobLink) map[string]any {
	attrs := map[string]any{}
	if dl.jobType != "" {
		attrs["job_type"] = dl.jobType
	}
	if dl.state != "" {
		attrs["state"] = dl.state
	}
	if dl.serviceAccount != "" {
		attrs["service_account"] = dl.serviceAccount
	}
	if dl.networkURI != "" {
		attrs["vpc"] = dl.networkURI
	}
	if dl.subnetURI != "" {
		attrs["subnet"] = dl.subnetURI
	}
	return attrs
}

type monitoringQueryParams struct {
	proj        string
	interval    *monitoringpb.TimeInterval
	dailyPeriod *durationpb.Duration
}

func (st *extractState) fetchMonitoring(ctx context.Context, clients *Clients) (*monitoringResult, error) {
	projects := st.enabledProjects(st.nonEmptyProjects(), "monitoring.googleapis.com")
	if len(projects) == 0 {
		return nil, nil
	}

	endTime := st.effectiveReadTime()
	interval := &monitoringpb.TimeInterval{
		StartTime: timestamppb.New(endTime.Add(-enrichmentLookback)),
		EndTime:   timestamppb.New(endTime),
	}
	dailyPeriod := durationpb.New(dailyAlignmentSecond * time.Second)

	var mu sync.Mutex
	consumedAgg := make(map[consumedAPIKey]int64)
	pubAgg := make(map[publisherModelKey]*publisherModelStat)
	var completed atomic.Int32
	total := len(projects)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxEnrichWorkers)

	for _, proj := range projects {
		g.Go(func() error {
			params := monitoringQueryParams{proj: proj, interval: interval, dailyPeriod: dailyPeriod}
			localConsumed, localPub, err := st.fetchProjectMonitoring(gctx, clients, params)
			if err != nil {
				return err
			}
			if len(localConsumed) > 0 || len(localPub) > 0 {
				mu.Lock()
				mergeMonitoringMaps(consumedAgg, pubAgg, localConsumed, localPub)
				mu.Unlock()
			}
			done := completed.Add(1)
			clients.ReportProgress(false, "Enrich Monitoring: %d/%d projects", done, total)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return buildMonitoringResult(consumedAgg, pubAgg), nil
}

func (st *extractState) fetchProjectMonitoring(
	ctx context.Context,
	clients *Clients,
	params monitoringQueryParams,
) (map[consumedAPIKey]int64, map[publisherModelKey]*publisherModelStat, error) {
	scope := prefixProjects + params.proj
	projNum := st.projectNumberFor(params.proj)
	localConsumed := make(map[consumedAPIKey]int64)
	localPub := make(map[publisherModelKey]*publisherModelStat)

	degraded, err := st.runMonitoringQuery(clients, params.proj, projNum, scope, "consumed_api", func() error {
		return queryConsumedAPI(ctx, clients, params, localConsumed)
	})
	if err != nil {
		return nil, nil, err
	}
	if !degraded && st.isServiceMaybeEnabled(params.proj, "aiplatform.googleapis.com") {
		degraded, err = st.fetchProjectPublisherMetrics(ctx, clients, params, projNum, scope, localPub)
		if err != nil {
			return nil, nil, err
		}
	}
	if !degraded {
		st.addCoverage(CoverageEntry{Scope: scope, Source: "monitoring", State: CoverageOK})
	}
	return localConsumed, localPub, nil
}

func (st *extractState) runMonitoringQuery(
	clients *Clients,
	proj, projNum, scope, label string,
	fn func() error,
) (bool, error) {
	err := fn()
	if err == nil {
		return false, nil
	}
	if deg, fatalErr := st.handleEnrichError(err, clients, proj, projNum, scope, "monitoring"); fatalErr != nil {
		return false, fatalErr
	} else if deg {
		return true, nil
	}
	return false, fmt.Errorf("monitoring %s(%s): %w", label, proj, err)
}

func (st *extractState) fetchProjectPublisherMetrics(
	ctx context.Context,
	clients *Clients,
	params monitoringQueryParams,
	projNum, scope string,
	localPub map[publisherModelKey]*publisherModelStat,
) (bool, error) {
	degraded, err := st.runMonitoringQuery(clients, params.proj, projNum, scope, "PublisherModel invocations", func() error {
		return queryPublisherInvocations(ctx, clients, params, localPub)
	})
	if err != nil || degraded || len(localPub) == 0 {
		return degraded, err
	}
	return st.runMonitoringQuery(clients, params.proj, projNum, scope, "PublisherModel throughput", func() error {
		return queryPublisherThroughput(ctx, clients, params, localPub)
	})
}

func mergeMonitoringMaps(
	consumedAgg map[consumedAPIKey]int64,
	pubAgg map[publisherModelKey]*publisherModelStat,
	localConsumed map[consumedAPIKey]int64,
	localPub map[publisherModelKey]*publisherModelStat,
) {
	for k, cnt := range localConsumed {
		consumedAgg[k] += cnt
	}
	for k, ps := range localPub {
		if existing, ok := pubAgg[k]; ok {
			existing.invocations += ps.invocations
			if ps.dedicatedThroughput {
				existing.dedicatedThroughput = true
			}
		} else {
			pubAgg[k] = ps
		}
	}
}

func buildMonitoringResult(consumedAgg map[consumedAPIKey]int64, pubAgg map[publisherModelKey]*publisherModelStat) *monitoringResult {
	res := &monitoringResult{
		consumed:        make([]*consumedAPIStat, 0, len(consumedAgg)),
		publisherModels: slices.Collect(maps.Values(pubAgg)),
	}
	for k, cnt := range consumedAgg {
		res.consumed = append(res.consumed, &consumedAPIStat{
			projectID:   k.proj,
			saUniqueID:  k.saID,
			serviceHost: k.svc,
			method:      k.method,
			callerKind:  k.kind,
			count:       cnt,
		})
	}
	slices.SortFunc(res.consumed, func(a, b *consumedAPIStat) int {
		return cmp.Or(
			cmp.Compare(a.projectID, b.projectID),
			cmp.Compare(a.serviceHost, b.serviceHost),
			cmp.Compare(a.saUniqueID, b.saUniqueID),
			cmp.Compare(a.method, b.method),
		)
	})
	slices.SortFunc(res.publisherModels, func(a, b *publisherModelStat) int {
		return cmp.Or(
			cmp.Compare(a.projectID, b.projectID),
			cmp.Compare(a.location, b.location),
			cmp.Compare(a.modelID, b.modelID),
		)
	})
	return res
}

func listSummedTimeSeries(
	ctx context.Context,
	clients *Clients,
	params monitoringQueryParams,
	filter string,
	groupBy []string,
	onSeries func(proj string, rLabels, mLabels map[string]string, total int64),
) error {
	return listAllTimeSeries(ctx, clients, &monitoringpb.ListTimeSeriesRequest{
		Name:     prefixProjects + params.proj,
		Filter:   filter,
		Interval: params.interval,
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    params.dailyPeriod,
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_SUM,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
			GroupByFields:      groupBy,
		},
		View: monitoringpb.ListTimeSeriesRequest_FULL,
	}, func(ts *monitoringpb.TimeSeries) {
		total := sumPointsInt64(ts.GetPoints())
		if total <= 0 {
			return
		}
		rLabels := ts.GetResource().GetLabels()
		p := rLabels["project_id"]
		if p == "" {
			p = params.proj
		}
		onSeries(p, rLabels, ts.GetMetric().GetLabels(), total)
	})
}

func queryConsumedAPI(
	ctx context.Context,
	clients *Clients,
	params monitoringQueryParams,
	agg map[consumedAPIKey]int64,
) error {
	filter := `metric.type = "serviceruntime.googleapis.com/api/request_count" AND resource.type = "consumed_api"`
	groupBy := []string{
		labelResourceProjectID,
		"resource.labels.service",
		"resource.labels.credential_id",
		"resource.labels.method",
	}
	return listSummedTimeSeries(ctx, clients, params, filter, groupBy, func(proj string, rLabels, _ map[string]string, total int64) {
		kind, saUID := parseCredentialID(rLabels["credential_id"])
		k := consumedAPIKey{
			proj:   proj,
			saID:   saUID,
			svc:    strings.ToLower(rLabels["service"]),
			method: rLabels["method"],
			kind:   kind,
		}
		agg[k] += total
	})
}

func queryPublisherModelMetric(
	ctx context.Context,
	clients *Clients,
	params monitoringQueryParams,
	metricType string,
	onPoint func(k publisherModelKey, total int64, dedicated bool),
) error {
	filter := fmt.Sprintf(`metric.type = "%s" AND resource.type = "aiplatform.googleapis.com/PublisherModel"`, metricType)
	groupBy := []string{
		labelResourceProjectID,
		"resource.labels.location",
		"resource.labels.publisher",
		"resource.labels.model_user_id",
		"metric.labels.request_type",
	}
	err := listSummedTimeSeries(ctx, clients, params, filter, groupBy, func(proj string, rLabels, mLabels map[string]string, total int64) {
		k := publisherModelKey{
			proj:  proj,
			loc:   rLabels["location"],
			pub:   rLabels["publisher"],
			model: rLabels["model_user_id"],
		}
		onPoint(k, total, mLabels["request_type"] == "dedicated")
	})
	if isGRPCNotFound(err) {
		return nil
	}
	return err
}

func queryPublisherInvocations(
	ctx context.Context,
	clients *Clients,
	params monitoringQueryParams,
	agg map[publisherModelKey]*publisherModelStat,
) error {
	return queryPublisherModelMetric(ctx, clients, params, "aiplatform.googleapis.com/publisher/online_serving/model_invocation_count", func(k publisherModelKey, total int64, dedicated bool) {
		stat, ok := agg[k]
		if !ok {
			stat = &publisherModelStat{
				projectID: k.proj,
				location:  k.loc,
				publisher: k.pub,
				modelID:   k.model,
			}
			agg[k] = stat
		}
		stat.invocations += total
		if dedicated {
			stat.dedicatedThroughput = true
		}
	})
}

func queryPublisherThroughput(
	ctx context.Context,
	clients *Clients,
	params monitoringQueryParams,
	agg map[publisherModelKey]*publisherModelStat,
) error {
	return queryPublisherModelMetric(ctx, clients, params, "aiplatform.googleapis.com/publisher/online_serving/consumed_token_throughput", func(k publisherModelKey, _ int64, dedicated bool) {
		if !dedicated {
			return
		}
		if stat, ok := agg[k]; ok {
			stat.dedicatedThroughput = true
		}
	})
}

type monitoringAggKey struct {
	proj  string
	host  string
	saUID string
}

func (st *extractState) applyMonitoringResult(res *monitoringResult, saWorkloads map[string][]string) {
	if res == nil {
		return
	}
	pubByProj := st.materializePublisherModels(res.publisherModels)
	targetsByProjType := st.indexTargetsByProjType()
	totals, genTotals, hasNonSAGenAI := st.aggregateConsumedAPICalls(res.consumed)
	st.emitConsumedAPIEdges(totals, targetsByProjType, saWorkloads)
	st.emitPublisherModelEdges(genTotals, hasNonSAGenAI, pubByProj, saWorkloads)
}

func (st *extractState) materializePublisherModels(models []*publisherModelStat) map[string][]string {
	pubByProj := make(map[string][]string)
	for _, pm := range models {
		if pm.invocations <= 0 || pm.modelID == "" {
			continue
		}
		proj := mapProject(pm.projectID, st.projIdx)
		loc := pm.location
		if loc == "" {
			loc = "global"
		}
		pub := pm.publisher
		if pub == "" {
			pub = "google"
		}
		uri := fmt.Sprintf("//aiplatform.googleapis.com/projects/%s/locations/%s/publishers/%s/models/%s", proj, loc, pub, pm.modelID)
		if st.assetsByURI[uri] == nil {
			attrs := map[string]any{
				"publisher":   pub,
				"model_id":    pm.modelID,
				"invocations": int(pm.invocations),
			}
			if pm.dedicatedThroughput {
				attrs["dedicated_throughput"] = true
			}
			rec := &ingestedAsset{
				uri:         uri,
				assetType:   "aiplatform.googleapis.com/PublisherModel",
				projectID:   proj,
				location:    loc,
				displayName: pm.modelID,
				attrs:       attrs,
			}
			st.assets = append(st.assets, rec)
			st.assetsByURI[uri] = rec
		}
		appendUniqueMap(pubByProj, proj, uri)
	}
	return pubByProj
}

func (st *extractState) indexTargetsByProjType() map[projAssetTypeKey][]string {
	targets := make(map[projAssetTypeKey][]string)
	for _, a := range st.assets {
		if a.suppressed {
			continue
		}
		k := projAssetTypeKey{project: a.projectID, assetType: a.assetType}
		targets[k] = append(targets[k], a.uri)
	}
	return targets
}

func (st *extractState) aggregateConsumedAPICalls(consumed []*consumedAPIStat) (
	map[monitoringAggKey]int64,
	map[monitoringAggKey]int64,
	map[string]bool,
) {
	totals := make(map[monitoringAggKey]int64)
	genTotals := make(map[monitoringAggKey]int64)
	hasNonSAGenAI := make(map[string]bool)

	for _, s := range consumed {
		if s.count <= 0 || s.serviceHost == "" || IsReadOnlyDiscoveryMethod(s.method) {
			continue
		}
		rawHost := strings.ToLower(strings.TrimSpace(s.serviceHost))
		canonHost := CanonicalServiceHost(rawHost)
		if suppressedPlumbingServices[canonHost] || transversalControlAPIs[rawHost] || transversalControlAPIs[canonHost] {
			continue
		}
		proj := mapProject(s.projectID, st.projIdx)

		if canonHost == "aiplatform.googleapis.com" {
			accumulateGenAICall(s, proj, canonHost, genTotals, hasNonSAGenAI)
			continue
		}
		if s.callerKind != callerKindServiceAccount || s.saUniqueID == "" {
			continue
		}
		if _, ok := consumedAPITargetTypes[canonHost]; ok {
			totals[monitoringAggKey{proj: proj, host: canonHost, saUID: s.saUniqueID}] += s.count
		}
	}
	return totals, genTotals, hasNonSAGenAI
}

func accumulateGenAICall(s *consumedAPIStat, proj, canonHost string, genTotals map[monitoringAggKey]int64, hasNonSAGenAI map[string]bool) {
	low := strings.ToLower(s.method)
	if !strings.Contains(low, "generatecontent") && !strings.Contains(low, "embedcontent") {
		return
	}
	if s.callerKind != callerKindServiceAccount || s.saUniqueID == "" {
		hasNonSAGenAI[proj] = true
		return
	}
	genTotals[monitoringAggKey{proj: proj, host: canonHost, saUID: s.saUniqueID}] += s.count
}

func (st *extractState) resolveMonitoringSource(saUID string, saWorkloads map[string][]string) string {
	email := strings.ToLower(st.saByUniqueID[saUID])
	if email == "" && strings.Contains(saUID, "@") {
		email = strings.ToLower(saUID)
	}
	src, _ := st.resolvePrincipalSource(email, saWorkloads)
	return src
}

func (st *extractState) emitConsumedAPIEdges(totals map[monitoringAggKey]int64, targetsByProjType map[projAssetTypeKey][]string, saWorkloads map[string][]string) {
	keys := slices.Collect(maps.Keys(totals))
	slices.SortFunc(keys, func(a, b monitoringAggKey) int {
		return cmp.Or(cmp.Compare(a.proj, b.proj), cmp.Compare(a.host, b.host), cmp.Compare(a.saUID, b.saUID))
	})

	for _, k := range keys {
		srcURI := st.resolveMonitoringSource(k.saUID, saWorkloads)
		if srcURI == "" {
			continue
		}
		var targetURIs []string
		for _, at := range consumedAPITargetTypes[k.host] {
			targetURIs = append(targetURIs, targetsByProjType[projAssetTypeKey{project: k.proj, assetType: at}]...)
		}
		if len(targetURIs) != 1 || targetURIs[0] == srcURI {
			continue
		}
		st.referencedURIs[srcURI] = true
		st.putEdge(FactEdge{FromURI: srcURI, ToURI: targetURIs[0], Kind: EdgeMonitoringTraffic, Detail: fmt.Sprintf("%s (%d reqs/7d)", k.host, totals[k]), Heuristic: true})
	}
}

func (st *extractState) emitPublisherModelEdges(genTotals map[monitoringAggKey]int64, hasNonSAGenAI map[string]bool, pubByProj, saWorkloads map[string][]string) {
	genKeys := slices.Collect(maps.Keys(genTotals))
	slices.SortFunc(genKeys, func(a, b monitoringAggKey) int {
		return cmp.Or(cmp.Compare(a.proj, b.proj), cmp.Compare(a.saUID, b.saUID))
	})
	for _, k := range genKeys {
		if hasNonSAGenAI[k.proj] {
			continue
		}
		models := pubByProj[k.proj]
		if len(models) != 1 {
			continue
		}
		srcURI := st.resolveMonitoringSource(k.saUID, saWorkloads)
		if srcURI == "" || srcURI == models[0] {
			continue
		}
		st.referencedURIs[srcURI] = true
		st.putEdge(FactEdge{FromURI: srcURI, ToURI: models[0], Kind: EdgeMonitoringTraffic, Detail: fmt.Sprintf("aiplatform.googleapis.com (%d reqs/7d)", genTotals[k]), Heuristic: true})
	}
}

func (st *extractState) indexSAEmailToWorkloads() map[string][]string {
	out := make(map[string][]string)
	for _, a := range st.assets {
		if a.suppressed || a.assetType == assetTypeServiceAccount {
			continue
		}
		sa := strings.ToLower(strings.TrimSpace(getAttrString(a.attrs, "service_account")))
		if sa == "" {
			continue
		}
		if sa == "default" {
			if num := st.projectNumberFor(a.projectID); num != "" {
				sa = num + "-compute@developer.gserviceaccount.com"
			} else {
				continue
			}
		}
		appendUniqueMap(out, sa, a.uri)
	}
	return out
}

func listAllTimeSeries(
	ctx context.Context,
	clients *Clients,
	req *monitoringpb.ListTimeSeriesRequest,
	fn func(*monitoringpb.TimeSeries),
) error {
	pageToken := ""
	for {
		req.PageToken = pageToken
		var resp *monitoringpb.ListTimeSeriesResponse
		err := retryOnRateLimit(ctx, clients.retryBackoff, func() error {
			if err := waitLimiter(ctx, clients.MonitoringLimiter); err != nil {
				return err
			}
			var doErr error
			resp, doErr = clients.Monitoring.ListTimeSeries(ctx, req)
			return doErr
		})
		if err != nil {
			return err
		}
		for _, ts := range resp.GetTimeSeries() {
			fn(ts)
		}
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			return nil
		}
	}
}

func parseCredentialID(credID string) (callerKind, saUniqueID string) {
	if strings.HasPrefix(credID, prefixServiceAccount) {
		return callerKindServiceAccount, strings.TrimPrefix(credID, prefixServiceAccount)
	}
	if credID == "" {
		return "unknown", ""
	}
	return "non_sa", ""
}

func sumPointsInt64(points []*monitoringpb.Point) int64 {
	var total int64
	for _, p := range points {
		val := p.GetValue()
		if val == nil {
			continue
		}
		switch v := val.GetValue().(type) {
		case *monitoringpb.TypedValue_Int64Value:
			total += v.Int64Value
		case *monitoringpb.TypedValue_DoubleValue:
			total += int64(v.DoubleValue)
		}
	}
	return total
}

func isGRPCNotFound(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.NotFound
}

func (st *extractState) handleEnrichError(err error, clients *Clients, projID, projNum, scope, source string) (degraded bool, fatalErr error) {
	ce := ClassifyGCPError(err, clients.QuotaProject, projID, projNum)
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
	return false, nil
}

func (st *extractState) allDiscoveredProjects() []string {
	seen := make(map[string]struct{})
	for p, node := range st.projects {
		if p != "" && !node.IsPSATenant {
			seen[p] = struct{}{}
		}
	}
	for _, a := range st.assets {
		if !a.suppressed && a.projectID != "" && !st.isPSATenant(a.projectID) {
			seen[a.projectID] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func (st *extractState) nonEmptyProjects() []string {
	seen := make(map[string]struct{})
	for _, a := range st.assets {
		if a.suppressed || a.projectID == "" || st.isPSATenant(a.projectID) {
			continue
		}
		if !isDefaultSandboxAsset(a.assetType, a.uri, a.displayName, getAttrString(a.attrs, "vpc"), getAttrString(a.attrs, "email")) {
			seen[a.projectID] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func (st *extractState) isServiceMaybeEnabled(proj, host string) bool {
	if len(st.enabledServices) == 0 {
		return true
	}
	svcs := st.enabledServices[proj]
	if len(svcs) == 0 {
		return true
	}
	return svcs[strings.ToLower(host)]
}

func (st *extractState) projectNumberFor(projID string) string {
	if p, ok := st.projects[projID]; ok && p.ProjectNumber != "" {
		return p.ProjectNumber
	}
	for num, id := range st.projIdx {
		if id == projID {
			return num
		}
	}
	return ""
}

func (st *extractState) effectiveReadTime() time.Time {
	if !st.readTime.IsZero() {
		return st.readTime.UTC()
	}
	return time.Now().UTC()
}
