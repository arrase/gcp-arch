package extract

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
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

	bqJobListFields = googleapi.Field("jobs(id,jobReference,user_email,principal_subject,configuration(query/destinationTable,load/destinationTable,copy(sourceTable,sourceTables,destinationTable)),statistics/query/referencedTables),nextPageToken")
)

var (
	anonTableRe       = regexp.MustCompile(`^anon(?:_|[0-9a-f]{8,})`)
	principalSASubjRe = regexp.MustCompile(`^serviceAccount:([^\s]+@[^\s]+)$`)

	// companionAPIHostMap aliases GCP companion API hosts to their canonical service host
	// (salvaged from base_catalog.yaml).
	companionAPIHostMap = map[string]string{
		"storage-component.googleapis.com":   "storage.googleapis.com",
		"storage-api.googleapis.com":         "storage.googleapis.com",
		"bigquerystorage.googleapis.com":     "bigquery.googleapis.com",
		"bigquerymigration.googleapis.com":   "bigquery.googleapis.com",
		"containerfilesystem.googleapis.com": "container.googleapis.com",
		"bigtable.googleapis.com":            "bigtableadmin.googleapis.com",
	}

	transversalControlAPIs = map[string]bool{
		"modelarmor.googleapis.com":       true,
		"cloudtrace.googleapis.com":       true,
		"logging.googleapis.com":          true,
		"monitoring.googleapis.com":       true,
		"secretmanager.googleapis.com":    true,
		"cloudkms.googleapis.com":         true,
		"dlp.googleapis.com":              true,
		"artifactregistry.googleapis.com": true,
		"cloudbuild.googleapis.com":       true,
		"clouddeploy.googleapis.com":      true,
	}

	consumedAPITargetTypes = map[string][]string{
		"storage.googleapis.com":         {"storage.googleapis.com/Bucket"},
		"bigquery.googleapis.com":        {"bigquery.googleapis.com/Dataset"},
		"sqladmin.googleapis.com":        {"sqladmin.googleapis.com/Instance"},
		"alloydb.googleapis.com":         {"alloydb.googleapis.com/Cluster"},
		"spanner.googleapis.com":         {"spanner.googleapis.com/Instance"},
		"firestore.googleapis.com":       {"firestore.googleapis.com/Database"},
		"bigtableadmin.googleapis.com":   {"bigtableadmin.googleapis.com/Instance"},
		"redis.googleapis.com":           {"redis.googleapis.com/Instance", "redis.googleapis.com/Cluster"},
		"memcache.googleapis.com":        {"memcache.googleapis.com/Instance"},
		"pubsub.googleapis.com":          {"pubsub.googleapis.com/Topic"},
		"cloudtasks.googleapis.com":      {"cloudtasks.googleapis.com/Queue"},
		"workflows.googleapis.com":       {"workflows.googleapis.com/Workflow"},
		"run.googleapis.com":             {"run.googleapis.com/Service"},
		"cloudfunctions.googleapis.com":  {"cloudfunctions.googleapis.com/CloudFunction"},
		"discoveryengine.googleapis.com": {"discoveryengine.googleapis.com/Engine", "discoveryengine.googleapis.com/DataStore"},
		"dialogflow.googleapis.com":      {"dialogflow.googleapis.com/Agent"},
		"documentai.googleapis.com":      {"documentai.googleapis.com/Processor"},
	}
)

type bqJobLink struct {
	projectID        string
	jobID            string
	userEmail        string
	principalSubject string
	referencedTables []string
	destinationTable string
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
func IsReadOnlyDiscoveryMethod(method string) bool {
	s := strings.TrimSpace(method)
	if s == "" {
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
		leaf == "poll" || strings.HasPrefix(leaf, "poll") ||
		leaf == "listen"
}

func (st *extractState) runEnrichers(ctx context.Context, clients *Clients) error {
	var (
		bqLinks []*bqJobLink
		dfLinks []*dataflowJobLink
		monRes  *monitoringResult
	)

	g, gctx := errgroup.WithContext(ctx)
	if clients.BigQuery != nil {
		g.Go(func() error {
			var err error
			bqLinks, err = st.fetchBigQueryJobs(gctx, clients)
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
	st.applyBigQueryLinks(bqLinks)
	st.applyMonitoringResult(monRes)
	return nil
}

func (st *extractState) fetchBigQueryJobs(ctx context.Context, clients *Clients) ([]*bqJobLink, error) {
	allProjects := st.allDiscoveredProjects()
	projects := make([]string, 0, len(allProjects))
	for _, proj := range allProjects {
		if st.isServiceMaybeEnabled(proj, "bigquery.googleapis.com") {
			projects = append(projects, proj)
		}
	}
	if len(projects) == 0 {
		return nil, nil
	}

	endTime := st.effectiveReadTime()
	minCreationMs := uint64(endTime.Add(-enrichmentLookback).UnixMilli())

	var mu sync.Mutex
	var links []*bqJobLink
	var completed atomic.Int32
	total := len(projects)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxEnrichWorkers)

	for _, proj := range projects {
		proj := proj
		g.Go(func() error {
			projLinks, err := st.fetchProjectBigQueryJobs(gctx, clients, proj, minCreationMs)
			if err != nil {
				return err
			}
			if len(projLinks) > 0 {
				mu.Lock()
				links = append(links, projLinks...)
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

	sort.Slice(links, func(i, j int) bool {
		if links[i].projectID != links[j].projectID {
			return links[i].projectID < links[j].projectID
		}
		return links[i].jobID < links[j].jobID
	})
	return links, nil
}

func (st *extractState) fetchProjectBigQueryJobs(ctx context.Context, clients *Clients, proj string, minCreationMs uint64) ([]*bqJobLink, error) {
	scope := "projects/" + proj
	projNum := st.projectNumberFor(proj)
	pageToken := ""
	isDegraded := false
	truncated := false
	var projLinks []*bqJobLink

	for page := 1; page <= maxBQJobPages; page++ {
		resp, err := listBigQueryJobsPage(ctx, clients, proj, minCreationMs, pageToken)
		if err != nil {
			if deg, fatalErr := st.handleEnrichError(err, clients, proj, projNum, scope, "bq_jobs"); fatalErr != nil {
				return nil, fatalErr
			} else if deg {
				isDegraded = true
				break
			}
			return nil, fmt.Errorf("bigquery jobs.list(%s): %w", proj, err)
		}

		for _, job := range resp.Jobs {
			if link := extractBQJobLink(proj, job, st.projIdx); link != nil {
				projLinks = append(projLinks, link)
			}
		}

		pageToken = resp.NextPageToken
		if pageToken == "" {
			break
		}
		if page == maxBQJobPages {
			truncated = true
			st.addCoverage(CoverageEntry{
				Scope:  scope,
				Source: "bq_jobs",
				State:  CoverageTruncatingCap,
				Detail: fmt.Sprintf("reached %d-page BigQuery jobs.list cap within 7-day window", maxBQJobPages),
			})
		}
	}
	if !isDegraded && !truncated {
		st.addCoverage(CoverageEntry{Scope: scope, Source: "bq_jobs", State: CoverageOK})
	}
	return projLinks, nil
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
			Fields(bqJobListFields).
			Context(ctx)
		if pageToken != "" {
			call.PageToken(pageToken)
		}
		var doErr error
		resp, doErr = call.Do()
		return doErr
	})
	return resp, err
}

func extractBQJobLink(projectID string, job *bigquery.JobListJobs, projIdx map[string]string) *bqJobLink {
	if job == nil {
		return nil
	}
	var refTables []string
	if job.Statistics != nil && job.Statistics.Query != nil {
		for _, tr := range job.Statistics.Query.ReferencedTables {
			if uri := canonicalBQTableRef(tr, projIdx); uri != "" {
				appendUnique(&refTables, uri)
			}
		}
	}

	var destTable string
	if job.Configuration != nil {
		switch {
		case job.Configuration.Query != nil && job.Configuration.Query.DestinationTable != nil:
			destTable = canonicalBQTableRef(job.Configuration.Query.DestinationTable, projIdx)
		case job.Configuration.Load != nil && job.Configuration.Load.DestinationTable != nil:
			destTable = canonicalBQTableRef(job.Configuration.Load.DestinationTable, projIdx)
		case job.Configuration.Copy != nil:
			if job.Configuration.Copy.DestinationTable != nil {
				destTable = canonicalBQTableRef(job.Configuration.Copy.DestinationTable, projIdx)
			}
			if uri := canonicalBQTableRef(job.Configuration.Copy.SourceTable, projIdx); uri != "" {
				appendUnique(&refTables, uri)
			}
			for _, tr := range job.Configuration.Copy.SourceTables {
				if uri := canonicalBQTableRef(tr, projIdx); uri != "" {
					appendUnique(&refTables, uri)
				}
			}
		}
	}
	if len(refTables) == 0 && destTable == "" {
		return nil
	}
	sort.Strings(refTables)
	jobID := job.Id
	if job.JobReference != nil && job.JobReference.JobId != "" {
		jobID = job.JobReference.JobId
	}
	return &bqJobLink{
		projectID:        projectID,
		jobID:            jobID,
		userEmail:        job.UserEmail,
		principalSubject: job.PrincipalSubject,
		referencedTables: refTables,
		destinationTable: destTable,
	}
}

func canonicalBQTableRef(tr *bigquery.TableReference, projIdx map[string]string) string {
	if tr == nil || tr.ProjectId == "" || tr.DatasetId == "" || tr.TableId == "" {
		return ""
	}
	if strings.HasPrefix(tr.DatasetId, "_") || strings.HasPrefix(tr.TableId, "_script") || anonTableRe.MatchString(tr.TableId) {
		return ""
	}
	proj := mapProject(tr.ProjectId, projIdx)
	return "//bigquery.googleapis.com/projects/" + proj + "/datasets/" + tr.DatasetId + "/tables/" + tr.TableId
}

func (st *extractState) applyBigQueryLinks(links []*bqJobLink) {
	if len(links) == 0 {
		return
	}
	saWorkloads := st.indexSAEmailToWorkloads()

	for _, job := range links {
		destDS := normalizeBQDatasetURI(job.destinationTable, job.projectID, st.projIdx)
		var refDatasets []string
		for _, tbl := range job.referencedTables {
			if ds := normalizeBQDatasetURI(tbl, job.projectID, st.projIdx); ds != "" {
				appendUnique(&refDatasets, ds)
			}
		}

		if destDS != "" {
			for _, refDS := range refDatasets {
				if refDS != destDS {
					st.emitTargetEdge(refDS, destDS, EdgeBQLineage, "sql_elt", "config_ref_cross_project")
				}
			}
		}

		touched := appendUniqueDataset(refDatasets, destDS)
		if len(touched) == 0 {
			continue
		}
		srcURI := st.resolveBQJobPrincipalSource(job, saWorkloads)
		if srcURI == "" {
			continue
		}
		for _, dsURI := range touched {
			st.emitTargetEdge(srcURI, dsURI, EdgeBQLineage, "bq_job", "config_ref_cross_project")
		}
	}
}

func appendUniqueDataset(refDatasets []string, destDS string) []string {
	touched := slices.Clone(refDatasets)
	if destDS != "" {
		appendUnique(&touched, destDS)
	}
	return touched
}

func (st *extractState) resolveBQJobPrincipalSource(job *bqJobLink, saWorkloads map[string][]string) string {
	email := extractPrincipalEmail(job.principalSubject, job.userEmail)
	if email == "" {
		return ""
	}
	if wls := saWorkloads[email]; len(wls) == 1 {
		return wls[0]
	}
	if sa := st.saByEmail[email]; sa != nil && !sa.suppressed {
		return sa.uri
	}
	return ""
}

func extractPrincipalEmail(principalSubject, userEmail string) string {
	s := strings.TrimSpace(principalSubject)
	if m := principalSASubjRe.FindStringSubmatch(s); len(m) == 2 {
		return strings.ToLower(m[1])
	}
	if strings.HasPrefix(strings.ToLower(s), "serviceaccount:") {
		return strings.ToLower(strings.TrimSpace(s[len("serviceAccount:"):]))
	}
	return strings.ToLower(strings.TrimSpace(userEmail))
}

func (st *extractState) fetchDataflowJobs(ctx context.Context, clients *Clients) ([]*dataflowJobLink, error) {
	allProjects := st.allDiscoveredProjects()
	projects := make([]string, 0, len(allProjects))
	for _, proj := range allProjects {
		if st.isServiceMaybeEnabled(proj, "dataflow.googleapis.com") {
			projects = append(projects, proj)
		}
	}
	if len(projects) == 0 {
		return nil, nil
	}

	var mu sync.Mutex
	var links []*dataflowJobLink
	var completed atomic.Int32
	total := len(projects)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxEnrichWorkers)

	for _, proj := range projects {
		proj := proj
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

	sort.Slice(links, func(i, j int) bool {
		if links[i].projectID != links[j].projectID {
			return links[i].projectID < links[j].projectID
		}
		return links[i].jobID < links[j].jobID
	})
	return links, nil
}

func (st *extractState) fetchProjectDataflowJobs(ctx context.Context, clients *Clients, proj string) ([]*dataflowJobLink, error) {
	scope := "projects/" + proj
	projNum := st.projectNumberFor(proj)
	pageToken := ""
	degraded := false
	var projLinks []*dataflowJobLink

	for {
		resp, err := listDataflowJobsPage(ctx, clients, proj, pageToken)
		if err != nil {
			if deg, fatalErr := st.handleEnrichError(err, clients, proj, projNum, scope, "dataflow_jobs"); fatalErr != nil {
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
		st.addCoverage(CoverageEntry{Scope: scope, Source: "dataflow_jobs", State: CoverageOK})
	}
	return projLinks, nil
}

func listDataflowJobsPage(ctx context.Context, clients *Clients, proj, pageToken string) (*dataflow.ListJobsResponse, error) {
	var resp *dataflow.ListJobsResponse
	err := retryOnRateLimit(ctx, clients.retryBackoff, func() error {
		if err := waitLimiter(ctx, clients.DataflowLimiter); err != nil {
			return err
		}
		call := clients.Dataflow.Projects.Locations.Jobs.List(proj, "-").Filter("ACTIVE").Context(ctx)
		if pageToken != "" {
			call.PageToken(pageToken)
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
		var detail *dataflow.Job
		err := retryOnRateLimit(ctx, clients.retryBackoff, func() error {
			if err := waitLimiter(ctx, clients.DataflowLimiter); err != nil {
				return err
			}
			var doErr error
			detail, doErr = clients.Dataflow.Projects.Locations.Jobs.Get(proj, loc, summary.Id).
				View("JOB_VIEW_DESCRIPTION").
				Context(ctx).
				Do()
			return doErr
		})
		if err != nil {
			if deg, fatalErr := st.handleEnrichError(err, clients, proj, projNum, scope, "dataflow_jobs"); fatalErr != nil {
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
	collectRef := func(raw string) {
		eRefs, _ := ExtractSanitizedRefs(raw, projIdx)
		for _, r := range eRefs {
			appendUnique(&refs, r)
		}
	}

	if job.Environment != nil {
		link.serviceAccount = job.Environment.ServiceAccountEmail
		if job.Environment.TempStoragePrefix != "" {
			collectRef(job.Environment.TempStoragePrefix)
		}
		if job.Environment.Dataset != "" {
			collectRef(job.Environment.Dataset)
		}
		for _, wp := range job.Environment.WorkerPools {
			if wp.Network != "" && link.networkURI == "" {
				link.networkURI = normalizeComputeNetworkURI(wp.Network, proj, projIdx)
			}
			if wp.Subnetwork != "" && link.subnetURI == "" {
				link.subnetURI = CanonicalizeAIP122WithService(wp.Subnetwork, "compute.googleapis.com", projIdx)
			}
		}
	}
	if job.PipelineDescription != nil {
		for _, dd := range job.PipelineDescription.DisplayData {
			collectRef(dd.StrValue)
			collectRef(dd.Url)
		}
		for _, tr := range job.PipelineDescription.OriginalPipelineTransform {
			for _, dd := range tr.DisplayData {
				collectRef(dd.StrValue)
				collectRef(dd.Url)
			}
		}
	}
	sort.Strings(refs)
	link.resourceURIs = refs
	return link
}

func (st *extractState) applyDataflowLinks(links []*dataflowJobLink) {
	for _, dl := range links {
		uri := "//dataflow.googleapis.com/projects/" + dl.projectID + "/locations/" + dl.location + "/jobs/" + dl.jobID
		a := st.assetsByURI[uri]
		if a == nil {
			dn := dl.jobName
			if dn == "" {
				dn = dl.jobID
			}
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
			parentURI := dl.subnetURI
			if parentURI == "" {
				parentURI = dl.networkURI
			}
			a = &ingestedAsset{
				uri:         uri,
				assetType:   "dataflow.googleapis.com/Job",
				projectID:   dl.projectID,
				location:    dl.location,
				displayName: dn,
				parentURI:   parentURI,
				attrs:       attrs,
			}
			st.assets = append(st.assets, a)
			st.assetsByURI[uri] = a
		}
		if dl.serviceAccount != "" {
			st.markServiceAccountRef(a.uri, dl.projectID, dl.serviceAccount)
		}
		for _, ref := range dl.resourceURIs {
			st.resolveSingleConfigRef(a.uri, dl.projectID, dl.networkURI, ref, EdgeConfigRef, "config_ref_cross_project")
		}
	}
}

func (st *extractState) fetchMonitoring(ctx context.Context, clients *Clients) (*monitoringResult, error) {
	candidates := st.nonEmptyProjects()
	projects := make([]string, 0, len(candidates))
	for _, proj := range candidates {
		if st.isServiceMaybeEnabled(proj, "monitoring.googleapis.com") {
			projects = append(projects, proj)
		}
	}
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
		proj := proj
		g.Go(func() error {
			localConsumed, localPub, err := st.fetchProjectMonitoring(gctx, clients, proj, interval, dailyPeriod)
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
	proj string,
	interval *monitoringpb.TimeInterval,
	dailyPeriod *durationpb.Duration,
) (map[consumedAPIKey]int64, map[publisherModelKey]*publisherModelStat, error) {
	scope := "projects/" + proj
	projNum := st.projectNumberFor(proj)
	localConsumed := make(map[consumedAPIKey]int64)
	localPub := make(map[publisherModelKey]*publisherModelStat)
	degraded := false

	runQuery := func(label string, fn func() error) error {
		err := fn()
		if err == nil {
			return nil
		}
		if deg, fatalErr := st.handleEnrichError(err, clients, proj, projNum, scope, "monitoring"); fatalErr != nil {
			return fatalErr
		} else if deg {
			degraded = true
			return nil
		}
		return fmt.Errorf("monitoring %s(%s): %w", label, proj, err)
	}

	if err := runQuery("consumed_api", func() error {
		return queryConsumedAPI(ctx, clients, proj, interval, dailyPeriod, localConsumed)
	}); err != nil {
		return nil, nil, err
	}

	if !degraded && st.isServiceMaybeEnabled(proj, "aiplatform.googleapis.com") {
		if err := runQuery("PublisherModel invocations", func() error {
			return queryPublisherInvocations(ctx, clients, proj, interval, dailyPeriod, localPub)
		}); err != nil {
			return nil, nil, err
		}
		if !degraded && len(localPub) > 0 {
			if err := runQuery("PublisherModel throughput", func() error {
				return queryPublisherThroughput(ctx, clients, proj, interval, dailyPeriod, localPub)
			}); err != nil {
				return nil, nil, err
			}
		}
	}

	if !degraded {
		st.addCoverage(CoverageEntry{Scope: scope, Source: "monitoring", State: CoverageOK})
	}
	return localConsumed, localPub, nil
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
		publisherModels: make([]*publisherModelStat, 0, len(pubAgg)),
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
	sort.Slice(res.consumed, func(i, j int) bool {
		a, b := res.consumed[i], res.consumed[j]
		if a.projectID != b.projectID {
			return a.projectID < b.projectID
		}
		if a.serviceHost != b.serviceHost {
			return a.serviceHost < b.serviceHost
		}
		if a.saUniqueID != b.saUniqueID {
			return a.saUniqueID < b.saUniqueID
		}
		return a.method < b.method
	})
	for _, ps := range pubAgg {
		res.publisherModels = append(res.publisherModels, ps)
	}
	sort.Slice(res.publisherModels, func(i, j int) bool {
		a, b := res.publisherModels[i], res.publisherModels[j]
		if a.projectID != b.projectID {
			return a.projectID < b.projectID
		}
		if a.location != b.location {
			return a.location < b.location
		}
		return a.modelID < b.modelID
	})
	return res
}

func queryConsumedAPI(
	ctx context.Context,
	clients *Clients,
	proj string,
	interval *monitoringpb.TimeInterval,
	dailyPeriod *durationpb.Duration,
	agg map[consumedAPIKey]int64,
) error {
	return listAllTimeSeries(ctx, clients, &monitoringpb.ListTimeSeriesRequest{
		Name:     "projects/" + proj,
		Filter:   `metric.type = "serviceruntime.googleapis.com/api/request_count" AND resource.type = "consumed_api"`,
		Interval: interval,
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    dailyPeriod,
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_SUM,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
			GroupByFields: []string{
				"resource.labels.project_id",
				"resource.labels.service",
				"resource.labels.credential_id",
				"resource.labels.method",
			},
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
			p = proj
		}
		kind, saUID := parseCredentialID(rLabels["credential_id"])
		k := consumedAPIKey{
			proj:   p,
			saID:   saUID,
			svc:    strings.ToLower(rLabels["service"]),
			method: rLabels["method"],
			kind:   kind,
		}
		agg[k] += total
	})
}

func queryPublisherInvocations(
	ctx context.Context,
	clients *Clients,
	proj string,
	interval *monitoringpb.TimeInterval,
	dailyPeriod *durationpb.Duration,
	agg map[publisherModelKey]*publisherModelStat,
) error {
	err := listAllTimeSeries(ctx, clients, &monitoringpb.ListTimeSeriesRequest{
		Name:     "projects/" + proj,
		Filter:   `metric.type = "aiplatform.googleapis.com/publisher/online_serving/model_invocation_count" AND resource.type = "aiplatform.googleapis.com/PublisherModel"`,
		Interval: interval,
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    dailyPeriod,
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_SUM,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
			GroupByFields: []string{
				"resource.labels.project_id",
				"resource.labels.location",
				"resource.labels.publisher",
				"resource.labels.model_user_id",
				"metric.labels.request_type",
			},
		},
		View: monitoringpb.ListTimeSeriesRequest_FULL,
	}, func(ts *monitoringpb.TimeSeries) {
		total := sumPointsInt64(ts.GetPoints())
		if total <= 0 {
			return
		}
		rLabels := ts.GetResource().GetLabels()
		mLabels := ts.GetMetric().GetLabels()
		p := rLabels["project_id"]
		if p == "" {
			p = proj
		}
		k := publisherModelKey{
			proj:  p,
			loc:   rLabels["location"],
			pub:   rLabels["publisher"],
			model: rLabels["model_user_id"],
		}
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
		if mLabels["request_type"] == "dedicated" {
			stat.dedicatedThroughput = true
		}
	})
	if isGRPCNotFound(err) {
		return nil
	}
	return err
}

func queryPublisherThroughput(
	ctx context.Context,
	clients *Clients,
	proj string,
	interval *monitoringpb.TimeInterval,
	dailyPeriod *durationpb.Duration,
	agg map[publisherModelKey]*publisherModelStat,
) error {
	err := listAllTimeSeries(ctx, clients, &monitoringpb.ListTimeSeriesRequest{
		Name:     "projects/" + proj,
		Filter:   `metric.type = "aiplatform.googleapis.com/publisher/online_serving/consumed_token_throughput" AND resource.type = "aiplatform.googleapis.com/PublisherModel"`,
		Interval: interval,
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    dailyPeriod,
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_SUM,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
			GroupByFields: []string{
				"resource.labels.project_id",
				"resource.labels.location",
				"resource.labels.publisher",
				"resource.labels.model_user_id",
				"metric.labels.request_type",
			},
		},
		View: monitoringpb.ListTimeSeriesRequest_FULL,
	}, func(ts *monitoringpb.TimeSeries) {
		total := sumPointsInt64(ts.GetPoints())
		if total <= 0 {
			return
		}
		rLabels := ts.GetResource().GetLabels()
		mLabels := ts.GetMetric().GetLabels()
		if mLabels["request_type"] != "dedicated" {
			return
		}
		p := rLabels["project_id"]
		if p == "" {
			p = proj
		}
		k := publisherModelKey{
			proj:  p,
			loc:   rLabels["location"],
			pub:   rLabels["publisher"],
			model: rLabels["model_user_id"],
		}
		if stat, ok := agg[k]; ok {
			stat.dedicatedThroughput = true
		}
	})
	if isGRPCNotFound(err) {
		return nil
	}
	return err
}

type monitoringAggKey struct {
	proj  string
	host  string
	saUID string
}

func (st *extractState) applyMonitoringResult(res *monitoringResult) {
	if res == nil {
		return
	}
	pubByProj := st.materializePublisherModels(res.publisherModels)
	targetsByProjType := st.indexTargetsByProjType()
	saWorkloads := st.indexSAEmailToWorkloads()
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
		list := pubByProj[proj]
		appendUnique(&list, uri)
		pubByProj[proj] = list
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
		if s.callerKind != "serviceaccount" || s.saUniqueID == "" {
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
	if s.callerKind != "serviceaccount" || s.saUniqueID == "" {
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
	if email == "" {
		return ""
	}
	if wls := saWorkloads[email]; len(wls) == 1 {
		return wls[0]
	}
	if len(saWorkloads[email]) > 1 {
		return ""
	}
	if sa := st.saByEmail[email]; sa != nil && !sa.suppressed {
		return sa.uri
	}
	return ""
}

func (st *extractState) emitConsumedAPIEdges(totals map[monitoringAggKey]int64, targetsByProjType map[projAssetTypeKey][]string, saWorkloads map[string][]string) {
	keys := make([]monitoringAggKey, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].proj != keys[j].proj {
			return keys[i].proj < keys[j].proj
		}
		if keys[i].host != keys[j].host {
			return keys[i].host < keys[j].host
		}
		return keys[i].saUID < keys[j].saUID
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
		st.addEdge(srcURI, targetURIs[0], EdgeMonitoringTraffic, fmt.Sprintf("%s (%d reqs/7d)", k.host, totals[k]))
	}
}

func (st *extractState) emitPublisherModelEdges(genTotals map[monitoringAggKey]int64, hasNonSAGenAI map[string]bool, pubByProj map[string][]string, saWorkloads map[string][]string) {
	genKeys := make([]monitoringAggKey, 0, len(genTotals))
	for k := range genTotals {
		genKeys = append(genKeys, k)
	}
	sort.Slice(genKeys, func(i, j int) bool {
		if genKeys[i].proj != genKeys[j].proj {
			return genKeys[i].proj < genKeys[j].proj
		}
		return genKeys[i].saUID < genKeys[j].saUID
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
		st.addEdge(srcURI, models[0], EdgeMonitoringTraffic, fmt.Sprintf("aiplatform.googleapis.com (%d reqs/7d)", genTotals[k]))
	}
}

func (st *extractState) indexSAEmailToWorkloads() map[string][]string {
	out := make(map[string][]string)
	for _, a := range st.assets {
		if a.suppressed || a.assetType == "iam.googleapis.com/ServiceAccount" {
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
		list := out[sa]
		appendUnique(&list, a.uri)
		out[sa] = list
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
	if strings.HasPrefix(credID, "serviceaccount:") {
		return "serviceaccount", strings.TrimPrefix(credID, "serviceaccount:")
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
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
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
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
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
