package extract

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/asset/apiv1/assetpb"
	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"golang.org/x/time/rate"
	"google.golang.org/api/bigquery/v2"
	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/dataflow/v1b3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	gtransport "google.golang.org/api/transport/grpc"
	htransport "google.golang.org/api/transport/http"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// DefaultCAIRPM caps Cloud Asset Inventory ListAssets below GCP's 100 RPM ceiling.
	DefaultCAIRPM = 80
	// DefaultMonitoringQPS caps Cloud Monitoring ListTimeSeries requests per second.
	DefaultMonitoringQPS = 10
	// DefaultEnricherQPS caps per-enricher BigQuery and Dataflow requests per second.
	DefaultEnricherQPS = 10

	// DefaultVertexLocation is the default Vertex AI endpoint location for Gemini 4.
	DefaultVertexLocation = "global"
	// DefaultVertexModel is the default Vertex AI model identifier.
	DefaultVertexModel = "gemini-4-carbon"

	maxRateLimitRetries = 3
	defaultRetryBackoff = 100 * time.Millisecond
	defaultHTTPTimeout  = 60 * time.Second
	maxGRPCRecvMsgBytes = 32 << 20
	maxDrainBodyBytes   = 64 << 10

	cloudPlatformReadOnlyScope = "https://www.googleapis.com/auth/cloud-platform.read-only"
	cloudPlatformScope         = "https://www.googleapis.com/auth/cloud-platform"
	defaultAssetEndpoint       = "cloudasset.googleapis.com:443"
	defaultMonitoringEndpoint  = "monitoring.googleapis.com:443"
	defaultGlobalVertexBase    = "https://aiplatform.googleapis.com"
)

var optionalEnricherServices = map[string]bool{
	svcBigQuery:                 true,
	"dataflow.googleapis.com":   true,
	"monitoring.googleapis.com": true,
}

// ErrorKind categorizes GCP API errors into fatal quota/auth failures vs target-scope degradations.
type ErrorKind string

// Supported ErrorKind classifications.
const (
	ErrorKindNone                  ErrorKind = ""
	ErrorKindQuotaFatal            ErrorKind = "QUOTA_FATAL"
	ErrorKindIAMDenied             ErrorKind = "IAM_DENIED"
	ErrorKindVPCSCBlocked          ErrorKind = "VPC_SC_BLOCKED"
	ErrorKindTargetServiceDisabled ErrorKind = "TARGET_SERVICE_DISABLED"
	ErrorKindUnauthenticated       ErrorKind = "UNAUTHENTICATED"
	ErrorKindOther                 ErrorKind = "OTHER"
)

// ClassifiedError captures structured Google RPC ErrorInfo unpacked from gRPC or REST errors.
type ClassifiedError struct {
	Kind      ErrorKind
	Reason    string
	Domain    string
	VPCSCUUID string
	Message   string
	Metadata  map[string]string
	Err       error
}

// IsFatalQuota reports whether the error indicates a fatal caller quota-project failure.
func (c ClassifiedError) IsFatalQuota() bool {
	return c.Kind == ErrorKindQuotaFatal
}

// IsDegraded reports whether the error indicates a non-fatal target-scope permission,
// VPC-SC block, or disabled target child-project API.
func (c ClassifiedError) IsDegraded() bool {
	return c.Kind == ErrorKindIAMDenied ||
		c.Kind == ErrorKindVPCSCBlocked ||
		c.Kind == ErrorKindTargetServiceDisabled
}

// CoverageState maps a degraded error kind into the corresponding CoverageState.
func (c ClassifiedError) CoverageState() CoverageState {
	switch c.Kind {
	case ErrorKindVPCSCBlocked:
		return CoverageVPCSCBlocked
	case ErrorKindIAMDenied:
		return CoverageIAMDenied
	case ErrorKindTargetServiceDisabled:
		return CoverageServiceDisabled
	default:
		return ""
	}
}

// QuotaProjectError is returned immediately when the caller's quota project cannot serve API requests.
type QuotaProjectError struct {
	Classified ClassifiedError
}

func (e *QuotaProjectError) Error() string {
	return fmt.Sprintf("quota project error (%s): %s", e.Classified.Reason, e.Classified.Message)
}

func (e *QuotaProjectError) Unwrap() error { return e.Classified.Err }

// ClassifyGCPError inspects both REST *googleapi.Error and gRPC status details (*errdetails.ErrorInfo)
// to distinguish fatal caller quota-project failures from target-scope degradations.
func ClassifyGCPError(err error, callerQuotaProject, targetProjectID, targetProjectNumber string) ClassifiedError {
	if err == nil {
		return ClassifiedError{Kind: ErrorKindNone}
	}

	out := ClassifiedError{
		Kind:    ErrorKindOther,
		Message: err.Error(),
		Err:     err,
	}

	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		if gerr.Message != "" {
			out.Message = gerr.Message
		}
		extractRESTErrorInfo(gerr, &out)
		classifyFromReasonAndCode(&out, codes.OK, gerr.Code, callerQuotaProject, targetProjectID, targetProjectNumber)
		return out
	}

	if st, ok := status.FromError(err); ok && st.Code() != codes.OK {
		out.Message = st.Message()
		for _, detail := range st.Details() {
			if ei, ok := detail.(*errdetails.ErrorInfo); ok {
				out.Reason = ei.GetReason()
				out.Domain = ei.GetDomain()
				out.Metadata = ei.GetMetadata()
				break
			}
		}
		classifyFromReasonAndCode(&out, st.Code(), 0, callerQuotaProject, targetProjectID, targetProjectNumber)
		return out
	}

	return out
}

func extractRESTErrorInfo(gerr *googleapi.Error, out *ClassifiedError) {
	if extractErrorInfoFromDetails(gerr.Details, out) {
		return
	}
	if extractErrorInfoFromBody(gerr.Body, out) {
		return
	}
	if out.Reason == "" && len(gerr.Errors) > 0 {
		out.Reason = gerr.Errors[0].Reason
	}
}

func extractErrorInfoFromDetails(details []any, out *ClassifiedError) bool {
	for _, raw := range details {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typeURL, _ := m["@type"].(string)
		if !strings.HasSuffix(typeURL, "google.rpc.ErrorInfo") {
			continue
		}
		applyErrorInfoMap(m, out)
		return true
	}
	return false
}

func applyErrorInfoMap(m map[string]any, out *ClassifiedError) {
	if r, ok := m["reason"].(string); ok {
		out.Reason = r
	}
	if d, ok := m["domain"].(string); ok {
		out.Domain = d
	}
	if meta, ok := m["metadata"].(map[string]any); ok {
		out.Metadata = extractStringAnyMap(meta)
	}
}

func extractStringAnyMap(meta map[string]any) map[string]string {
	out := make(map[string]string, len(meta))
	for k, v := range meta {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func extractErrorInfoFromBody(body string, out *ClassifiedError) bool {
	if body == "" {
		return false
	}
	var payload struct {
		Error struct {
			Details []struct {
				Type     string         `json:"@type"`
				Reason   string         `json:"reason"`
				Domain   string         `json:"domain"`
				Metadata map[string]any `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &payload) != nil {
		return false
	}
	for _, d := range payload.Error.Details {
		if !strings.HasSuffix(d.Type, "google.rpc.ErrorInfo") {
			continue
		}
		out.Reason = d.Reason
		out.Domain = d.Domain
		if len(d.Metadata) > 0 {
			out.Metadata = extractStringAnyMap(d.Metadata)
		}
		return true
	}
	return false
}

func trimProjectPrefix(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), prefixProjects)
}

func matchesTargetProject(candidate, targetID, targetNum string) bool {
	if candidate == "" {
		return false
	}
	return (targetID != "" && candidate == targetID) || (targetNum != "" && candidate == targetNum)
}

func classifyDisabledServiceKind(reason, domain string, meta map[string]string, callerQuotaProject, targetProjectID, targetProjectNumber string) ErrorKind {
	c := trimProjectPrefix(meta["consumer"])
	callerQP := trimProjectPrefix(callerQuotaProject)
	targetID := trimProjectPrefix(targetProjectID)
	targetNum := trimProjectPrefix(targetProjectNumber)

	if targetID == "" && targetNum == "" {
		return ErrorKindQuotaFatal
	}
	if !matchesTargetProject(c, targetID, targetNum) {
		return ErrorKindQuotaFatal
	}
	if matchesTargetProject(callerQP, targetID, targetNum) || (callerQP != "" && c == callerQP) {
		svc := strings.ToLower(strings.TrimSpace(meta["service"]))
		if svc == "" {
			svc = strings.ToLower(strings.TrimSpace(domain))
		}
		if reason == "SERVICE_DISABLED" && optionalEnricherServices[svc] {
			return ErrorKindTargetServiceDisabled
		}
		return ErrorKindQuotaFatal
	}
	return ErrorKindTargetServiceDisabled
}

func classifyFromReasonAndCode(
	out *ClassifiedError,
	grpcCode codes.Code,
	httpCode int,
	callerQuotaProject, targetProjectID, targetProjectNumber string,
) {
	if uuid := out.Metadata["vpcServiceControlsUniqueIdentifier"]; uuid != "" {
		out.VPCSCUUID = uuid
	}

	switch out.Reason {
	case "USER_PROJECT_DENIED":
		out.Kind = ErrorKindQuotaFatal
		return
	case "SERVICE_DISABLED", "BILLING_DISABLED":
		out.Kind = classifyDisabledServiceKind(out.Reason, out.Domain, out.Metadata, callerQuotaProject, targetProjectID, targetProjectNumber)
		return
	case "SECURITY_POLICY_VIOLATED", "VPC_SERVICE_CONTROLS":
		out.Kind = ErrorKindVPCSCBlocked
		return
	case "IAM_PERMISSION_DENIED":
		out.Kind = ErrorKindIAMDenied
		return
	}

	switch {
	case out.VPCSCUUID != "":
		out.Kind = ErrorKindVPCSCBlocked
	case grpcCode == codes.Unauthenticated || httpCode == http.StatusUnauthorized:
		out.Kind = ErrorKindUnauthenticated
	case grpcCode == codes.PermissionDenied || httpCode == http.StatusForbidden:
		out.Kind = ErrorKindIAMDenied
	}
}

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok {
		if st.Code() == codes.ResourceExhausted {
			return true
		}
		for _, detail := range st.Details() {
			if ei, ok := detail.(*errdetails.ErrorInfo); ok && ei.GetReason() == "RATE_LIMIT_EXCEEDED" {
				return true
			}
		}
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		if gerr.Code == http.StatusTooManyRequests {
			return true
		}
		var ce ClassifiedError
		extractRESTErrorInfo(gerr, &ce)
		if ce.Reason == "RATE_LIMIT_EXCEEDED" || ce.Reason == "rateLimitExceeded" {
			return true
		}
	}
	return false
}

func retryOnRateLimit(ctx context.Context, baseBackoff time.Duration, fn func() error) error {
	if baseBackoff <= 0 {
		baseBackoff = defaultRetryBackoff
	}
	backoff := baseBackoff
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || attempt >= maxRateLimitRetries || !isRateLimitError(err) {
			return err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
	}
}

func waitLimiter(ctx context.Context, lim *rate.Limiter) error {
	if lim == nil {
		return nil
	}
	return lim.Wait(ctx)
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// ClientOptions configures read-only GCP API clients, quota project handling, rate limiters,
// and optional test connection overrides.
type ClientOptions struct {
	QuotaProject        string
	ProjectAliases      map[string]string
	AssetConn           *grpc.ClientConn
	MonitoringConn      *grpc.ClientConn
	HTTPClient          *http.Client
	BigQueryEndpoint    string
	DataflowEndpoint    string
	CAIRateLimit        rate.Limit
	MonitoringRateLimit rate.Limit
	BQLimiter           *rate.Limiter
	DataflowLimiter     *rate.Limiter
	ProgressWriter      io.Writer
	RetryBackoff        time.Duration
}

// Clients bundles read-only GCP API clients, shared rate limiters, and progress reporting.
type Clients struct {
	QuotaProject      string
	ProjectAliases    map[string]string
	Asset             assetpb.AssetServiceClient
	Monitoring        monitoringpb.MetricServiceClient
	BigQuery          *bigquery.Service
	Dataflow          *dataflow.Service
	HTTPClient        *http.Client
	CAILimiter        *rate.Limiter
	MonitoringLimiter *rate.Limiter
	BQLimiter         *rate.Limiter
	DataflowLimiter   *rate.Limiter
	ProgressWriter    io.Writer

	retryBackoff time.Duration
	progressMu   sync.Mutex
	lastProgress time.Time
	closers      []io.Closer
}

// ReportProgress writes a formatted progress line to ProgressWriter, throttled to at most once
// per second unless force is true. Safe for concurrent calls.
func (c *Clients) ReportProgress(force bool, format string, args ...any) {
	if c == nil || c.ProgressWriter == nil {
		return
	}
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	now := time.Now()
	if !force && !c.lastProgress.IsZero() && now.Sub(c.lastProgress) < time.Second {
		return
	}
	c.lastProgress = now
	if !strings.HasSuffix(format, "\n") {
		format += "\n"
	}
	_, _ = fmt.Fprintf(c.ProgressWriter, format, args...)
}

// Close releases all gRPC connections and idle HTTP connections opened by NewClients.
func (c *Clients) Close() error {
	var firstErr error
	for _, cl := range c.closers {
		if err := cl.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ResolveQuotaProject returns explicitQuotaProject if non-empty, otherwise preserves the
// quota_project_id embedded in ADC JSON. It never falls back to a target workload project.
func ResolveQuotaProject(creds *auth.Credentials, explicitQuotaProject string) string {
	if qp := strings.TrimSpace(explicitQuotaProject); qp != "" {
		return qp
	}
	if creds == nil {
		return ""
	}
	if qp, err := creds.QuotaProjectID(context.Background()); err == nil {
		return strings.TrimSpace(qp)
	}
	return ""
}

// NewClients initializes strictly read-only GCP clients (cloud-platform.read-only) with
// shared CAI (<= 80 RPM) and Monitoring/BigQuery/Dataflow (<= 10 QPS) rate limiters.
func NewClients(ctx context.Context, opts ClientOptions) (*Clients, error) {
	c := newClientsWithLimiters(opts)

	baseOpts, qp, err := resolveReadOnlyBaseOptions(opts)
	if err != nil {
		return nil, err
	}
	c.QuotaProject = qp

	assetConn, err := dialOrReuse(ctx, opts.AssetConn, defaultAssetEndpoint, baseOpts, &c.closers)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("dial Cloud Asset Inventory: %w", err)
	}
	c.Asset = assetpb.NewAssetServiceClient(assetConn)

	monConn, err := dialOrReuse(ctx, opts.MonitoringConn, defaultMonitoringEndpoint, baseOpts, &c.closers)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("dial Cloud Monitoring: %w", err)
	}
	c.Monitoring = monitoringpb.NewMetricServiceClient(monConn)

	if err := initRESTServices(ctx, c, opts, baseOpts); err != nil {
		_ = c.Close()
		return nil, err
	}

	return c, nil
}

func newClientsWithLimiters(opts ClientOptions) *Clients {
	caiRate := opts.CAIRateLimit
	if caiRate <= 0 {
		caiRate = rate.Every(time.Minute / DefaultCAIRPM)
	}
	monRate := opts.MonitoringRateLimit
	if monRate <= 0 {
		monRate = rate.Limit(DefaultMonitoringQPS)
	}
	bqLimiter := opts.BQLimiter
	if bqLimiter == nil {
		bqLimiter = rate.NewLimiter(rate.Limit(DefaultEnricherQPS), DefaultEnricherQPS)
	}
	dfLimiter := opts.DataflowLimiter
	if dfLimiter == nil {
		dfLimiter = rate.NewLimiter(rate.Limit(DefaultEnricherQPS), DefaultEnricherQPS)
	}

	return &Clients{
		ProjectAliases:    opts.ProjectAliases,
		CAILimiter:        rate.NewLimiter(caiRate, 1),
		MonitoringLimiter: rate.NewLimiter(monRate, 1),
		BQLimiter:         bqLimiter,
		DataflowLimiter:   dfLimiter,
		ProgressWriter:    opts.ProgressWriter,
		retryBackoff:      opts.RetryBackoff,
	}
}

func resolveReadOnlyBaseOptions(opts ClientOptions) ([]option.ClientOption, string, error) {
	if opts.AssetConn != nil && opts.MonitoringConn != nil && opts.HTTPClient != nil {
		return nil, strings.TrimSpace(opts.QuotaProject), nil
	}
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{cloudPlatformReadOnlyScope},
	})
	if err != nil {
		return nil, "", fmt.Errorf("load Application Default Credentials: %w", err)
	}
	qp := ResolveQuotaProject(creds, opts.QuotaProject)
	baseOpts := []option.ClientOption{
		option.WithAuthCredentials(creds),
		option.WithTelemetryDisabled(),
	}
	if qp != "" {
		baseOpts = append(baseOpts, option.WithQuotaProject(qp))
	}
	return baseOpts, qp, nil
}

func initRESTServices(ctx context.Context, c *Clients, opts ClientOptions, baseOpts []option.ClientOption) error {
	if opts.HTTPClient != nil {
		c.HTTPClient = opts.HTTPClient
	} else {
		hc, _, err := htransport.NewClient(ctx, baseOpts...)
		if err != nil {
			return fmt.Errorf("create oauth2 HTTP client: %w", err)
		}
		hc.Timeout = defaultHTTPTimeout
		c.HTTPClient = hc
		c.closers = append(c.closers, closerFunc(func() error {
			hc.CloseIdleConnections()
			return nil
		}))
	}

	bqSvc, err := bigquery.NewService(ctx, restClientOptions(c.HTTPClient, opts.BigQueryEndpoint)...)
	if err != nil {
		return fmt.Errorf("init BigQuery client: %w", err)
	}
	c.BigQuery = bqSvc

	dfSvc, err := dataflow.NewService(ctx, restClientOptions(c.HTTPClient, opts.DataflowEndpoint)...)
	if err != nil {
		return fmt.Errorf("init Dataflow client: %w", err)
	}
	c.Dataflow = dfSvc

	return nil
}

func restClientOptions(hc *http.Client, endpoint string) []option.ClientOption {
	opts := []option.ClientOption{option.WithHTTPClient(hc)}
	if ep := strings.TrimSpace(endpoint); ep != "" {
		opts = append(opts, option.WithEndpoint(ep))
	}
	return opts
}

func dialOrReuse(
	ctx context.Context,
	existing *grpc.ClientConn,
	endpoint string,
	baseOpts []option.ClientOption,
	closers *[]io.Closer,
) (*grpc.ClientConn, error) {
	if existing != nil {
		return existing, nil
	}
	dialOpts := slices.Concat(baseOpts, []option.ClientOption{
		option.WithEndpoint(endpoint),
		option.WithGRPCDialOption(grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxGRPCRecvMsgBytes))),
	})
	conn, err := gtransport.Dial(ctx, dialOpts...)
	if err != nil {
		return nil, err
	}
	*closers = append(*closers, conn)
	return conn, nil
}

// ProbeStatus indicates the outcome of a lightweight preflight permission probe.
type ProbeStatus string

// Supported ProbeStatus outcomes.
const (
	ProbeGranted         ProbeStatus = "GRANTED"
	ProbeDenied          ProbeStatus = "DENIED"
	ProbeVPCSC           ProbeStatus = "VPC_SC_BLOCKED"
	ProbeServiceDisabled ProbeStatus = "SERVICE_DISABLED"
	ProbeQuotaError      ProbeStatus = "QUOTA_PROJECT_ERROR"
	ProbeError           ProbeStatus = "ERROR"
)

// PreflightProbe records the outcome and remediation guidance for one API/scope probe.
type PreflightProbe struct {
	Scope        string      `json:"scope"`
	Source       string      `json:"source"`
	RequiredRole string      `json:"required_role,omitempty"`
	Status       ProbeStatus `json:"status"`
	Detail       string      `json:"detail,omitempty"`
	VPCSCUUID    string      `json:"vpc_sc_uuid,omitempty"`
	Remediation  string      `json:"remediation,omitempty"`
}

// PreflightReport summarizes all permission and quota probes executed by RunPreflight.
type PreflightReport struct {
	QuotaProject string           `json:"quota_project,omitempty"`
	Scopes       []string         `json:"scopes"`
	Probes       []PreflightProbe `json:"probes"`
	FatalError   string           `json:"fatal_error,omitempty"`
}

// VertexPreflightOptions configures the optional zero-token Vertex AI readiness probe
// against --vertex-project using separate cloud-platform OAuth credentials.
type VertexPreflightOptions struct {
	Project        string
	Location       string
	Model          string
	HTTPClient     *http.Client
	CRMEndpoint    string
	VertexEndpoint string
	ModelProbe     func(ctx context.Context, project, location, model string) error
}

type preflightState struct {
	clients   *Clients
	report    *PreflightReport
	fatalErr  *QuotaProjectError
	unauthErr error
	probeErr  error
}

func (ps *preflightState) recordProbe(scope, targetProj, source, role string, err error) {
	p := PreflightProbe{
		Scope:        scope,
		Source:       source,
		RequiredRole: role,
		Status:       ProbeGranted,
	}
	if err != nil {
		ce := ClassifyGCPError(err, ps.clients.QuotaProject, targetProj, "")
		p.Detail = ce.Message
		p.VPCSCUUID = ce.VPCSCUUID
		ps.applyErrorClassification(&p, ce, scope, source, role, err)
	}
	ps.report.Probes = append(ps.report.Probes, p)
}

func (ps *preflightState) applyErrorClassification(p *PreflightProbe, ce ClassifiedError, scope, source, role string, rawErr error) {
	switch {
	case ce.IsFatalQuota():
		p.Status = ProbeQuotaError
		p.Remediation = quotaRemediation(ps.clients.QuotaProject, ce.Reason)
		if ps.fatalErr == nil {
			ps.fatalErr = &QuotaProjectError{Classified: ce}
			ps.report.FatalError = ps.fatalErr.Error()
		}
	case ce.Kind == ErrorKindUnauthenticated:
		p.Status = ProbeError
		p.Remediation = "Run `gcloud auth application-default login` to refresh Application Default Credentials"
		if ps.unauthErr == nil {
			ps.unauthErr = fmt.Errorf("authentication failed (%s on %s): %w", source, scope, rawErr)
			if ps.report.FatalError == "" {
				ps.report.FatalError = ps.unauthErr.Error()
			}
		}
	case ce.Kind == ErrorKindTargetServiceDisabled:
		p.Status = ProbeServiceDisabled
	case ce.Kind == ErrorKindVPCSCBlocked:
		p.Status = ProbeVPCSC
		p.Remediation = fmt.Sprintf("Allow caller egress/ingress in VPC-SC perimeter (audit UUID: %s)", ce.VPCSCUUID)
	case ce.Kind == ErrorKindIAMDenied:
		p.Status = ProbeDenied
		p.Remediation = iamRemediation(scope, role)
	default:
		p.Status = ProbeError
		if ps.probeErr == nil {
			ps.probeErr = fmt.Errorf("probe failed (%s on %s): %w", source, scope, rawErr)
			if ps.report.FatalError == "" {
				ps.report.FatalError = ps.probeErr.Error()
			}
		}
	}
}

func (ps *preflightState) probeCAIScopes(ctx context.Context, sortedScopes []string) ([]string, error) {
	var projectIDs []string
	var sampleProject string
	for _, scope := range sortedScopes {
		targetProj := ""
		if proj, ok := strings.CutPrefix(scope, prefixProjects); ok {
			projectIDs = append(projectIDs, proj)
			targetProj = proj
		}
		sampled, err := ps.probeCAIScope(ctx, scope, targetProj, sampleProject == "")
		if err != nil {
			return nil, err
		}
		if sampled != "" {
			sampleProject = sampled
		}
	}
	if len(projectIDs) == 0 && sampleProject != "" {
		projectIDs = append(projectIDs, sampleProject)
	}
	return projectIDs, nil
}

func (ps *preflightState) probeCAIScope(ctx context.Context, scope, targetProj string, needSample bool) (string, error) {
	isOrgOrFolder := strings.HasPrefix(scope, prefixOrganizations) || strings.HasPrefix(scope, prefixFolders)
	if err := waitLimiter(ctx, ps.clients.CAILimiter); err != nil {
		return "", err
	}
	req := &assetpb.ListAssetsRequest{
		Parent:      scope,
		ContentType: assetpb.ContentType_RESOURCE,
		PageSize:    1,
	}
	if isOrgOrFolder {
		req.AssetTypes = []string{assetTypeProject}
	}
	resp, err := ps.clients.Asset.ListAssets(ctx, req)
	ps.recordProbe(scope, targetProj, "cloudasset.googleapis.com", "roles/cloudasset.viewer", err)

	var sampleProject string
	if err == nil && needSample {
		sampleProject, err = ps.sampleProjectFromScope(ctx, scope, isOrgOrFolder, resp.GetAssets())
		if err != nil {
			return "", err
		}
	}
	if isOrgOrFolder {
		if err := ps.probeCAIFolderAccess(ctx, scope); err != nil {
			return "", err
		}
	}
	return sampleProject, nil
}

func (ps *preflightState) sampleProjectFromScope(ctx context.Context, scope string, isOrgOrFolder bool, assets []*assetpb.Asset) (string, error) {
	if proj := firstProjectFromAssets(assets, ps.clients.ProjectAliases); proj != "" || !isOrgOrFolder {
		return proj, nil
	}
	if err := waitLimiter(ctx, ps.clients.CAILimiter); err != nil {
		return "", err
	}
	fallbackResp, fbErr := ps.clients.Asset.ListAssets(ctx, &assetpb.ListAssetsRequest{
		Parent:      scope,
		ContentType: assetpb.ContentType_RESOURCE,
		PageSize:    1,
	})
	if fbErr != nil {
		return "", nil
	}
	return firstProjectFromAssets(fallbackResp.GetAssets(), ps.clients.ProjectAliases), nil
}

func firstProjectFromAssets(assets []*assetpb.Asset, aliases map[string]string) string {
	for _, a := range assets {
		if proj := projectFromAsset(a, aliases); proj != "" {
			return proj
		}
	}
	return ""
}

func projectFromAsset(a *assetpb.Asset, aliases map[string]string) string {
	if a.GetAssetType() == assetTypeProject {
		if pid := strings.TrimSpace(getStringField(a.GetResource().GetData(), "projectId")); pid != "" {
			return pid
		}
	}
	for _, anc := range a.GetAncestors() {
		if proj, ok := strings.CutPrefix(anc, prefixProjects); ok {
			return mapProject(proj, aliases)
		}
	}
	if a.GetAssetType() == assetTypeProject {
		if last := shortResourceName(a.GetName()); last != "" {
			return mapProject(last, aliases)
		}
	}
	return ""
}

func (ps *preflightState) probeCAIFolderAccess(ctx context.Context, scope string) error {
	if err := waitLimiter(ctx, ps.clients.CAILimiter); err != nil {
		return err
	}
	folderResp, folderErr := ps.clients.Asset.ListAssets(ctx, &assetpb.ListAssetsRequest{
		Parent:      scope,
		AssetTypes:  []string{assetTypeFolder},
		ContentType: assetpb.ContentType_RESOURCE,
		PageSize:    1,
	})
	if folderErr != nil {
		ps.recordProbe(scope, "", sourceCAIFolders, roleFolderViewer, folderErr)
		return nil
	}
	assets := folderResp.GetAssets()
	if len(assets) > 0 && strings.TrimSpace(getStringField(assets[0].GetResource().GetData(), "displayName")) == "" {
		ps.report.Probes = append(ps.report.Probes, PreflightProbe{
			Scope:        scope,
			Source:       sourceCAIFolders,
			RequiredRole: roleFolderViewer,
			Status:       ProbeDenied,
			Detail:       "Folder displayName is empty; grant roles/resourcemanager.folderViewer to resolve folder names",
			Remediation:  iamRemediation(scope, roleFolderViewer),
		})
		return nil
	}
	ps.report.Probes = append(ps.report.Probes, PreflightProbe{
		Scope:        scope,
		Source:       sourceCAIFolders,
		RequiredRole: roleFolderViewer,
		Status:       ProbeGranted,
	})
	return nil
}

func (ps *preflightState) probeProjectEnrichers(ctx context.Context, projectIDs []string) error {
	now := time.Now().UTC()
	for _, proj := range projectIDs {
		scope := prefixProjects + proj
		if err := waitLimiter(ctx, ps.clients.MonitoringLimiter); err != nil {
			return err
		}
		_, err := ps.clients.Monitoring.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{
			Name:   scope,
			Filter: `metric.type = "serviceruntime.googleapis.com/api/request_count"`,
			Interval: &monitoringpb.TimeInterval{
				StartTime: timestamppb.New(now.Add(-time.Hour)),
				EndTime:   timestamppb.New(now),
			},
			View:     monitoringpb.ListTimeSeriesRequest_HEADERS,
			PageSize: 1,
		})
		ps.recordProbe(scope, proj, "monitoring.googleapis.com", "roles/monitoring.viewer", err)

		if err := waitLimiter(ctx, ps.clients.BQLimiter); err != nil {
			return err
		}
		_, err = ps.clients.BigQuery.Jobs.List(proj).AllUsers(true).MaxResults(1).Context(ctx).Do()
		ps.recordProbe(scope, proj, svcBigQuery, "roles/bigquery.resourceViewer", err)

		if err := waitLimiter(ctx, ps.clients.DataflowLimiter); err != nil {
			return err
		}
		_, err = ps.clients.Dataflow.Projects.Locations.Jobs.List(proj, "-").Filter("ACTIVE").PageSize(1).Context(ctx).Do()
		ps.recordProbe(scope, proj, "dataflow.googleapis.com", "roles/dataflow.viewer", err)
	}
	return nil
}

func (ps *preflightState) probeVertexReadiness(ctx context.Context, vertexOpts VertexPreflightOptions) {
	vertexProj := strings.TrimSpace(vertexOpts.Project)
	if vertexProj == "" {
		return
	}
	vertexScope := prefixProjects + vertexProj
	location := strings.TrimSpace(vertexOpts.Location)
	if location == "" {
		location = DefaultVertexLocation
	}
	model := strings.TrimSpace(vertexOpts.Model)
	if model == "" {
		model = DefaultVertexModel
	}

	vhc := vertexOpts.HTTPClient
	if vhc == nil {
		var authErr error
		vhc, authErr = newCloudPlatformHTTPClient(ctx, vertexProj)
		if authErr != nil {
			ps.recordProbe(vertexScope, vertexProj, sourceVertexIAM, roleAIPlatformUser, authErr)
			return
		}
		defer vhc.CloseIdleConnections()
	}

	granted, err := probeVertexIAMPermission(ctx, vhc, vertexOpts.CRMEndpoint, vertexProj)
	switch {
	case err != nil:
		ps.recordProbe(vertexScope, vertexProj, sourceVertexIAM, roleAIPlatformUser, err)
	case !granted:
		ps.report.Probes = append(ps.report.Probes, PreflightProbe{
			Scope:        vertexScope,
			Source:       sourceVertexIAM,
			RequiredRole: roleAIPlatformUser,
			Status:       ProbeDenied,
			Detail:       "missing aiplatform.endpoints.predict permission on Vertex AI project",
			Remediation:  iamRemediation(vertexScope, roleAIPlatformUser),
		})
	default:
		ps.report.Probes = append(ps.report.Probes, PreflightProbe{
			Scope:        vertexScope,
			Source:       sourceVertexIAM,
			RequiredRole: roleAIPlatformUser,
			Status:       ProbeGranted,
		})
	}

	var modelErr error
	if vertexOpts.ModelProbe != nil {
		modelErr = vertexOpts.ModelProbe(ctx, vertexProj, location, model)
	} else {
		modelErr = probeVertexModelREST(ctx, vhc, vertexOpts.VertexEndpoint, vertexProj, location, model)
	}
	ps.recordProbe(vertexScope, vertexProj, "vertex.model", roleAIPlatformUser, modelErr)
}

func (ps *preflightState) firstError() error {
	if ps.fatalErr != nil {
		return ps.fatalErr
	}
	if ps.unauthErr != nil {
		return ps.unauthErr
	}
	return ps.probeErr
}

// RunPreflight executes lightweight read-only probes on target scopes (CAI, Folder displayName,
// Cloud Monitoring, BigQuery jobs.list, Dataflow jobs.list) and, when vertexOpts.Project is set,
// runs a zero-token Vertex AI IAM + Model readiness check using cloud-platform scope.
func RunPreflight(ctx context.Context, clients *Clients, scopes []string, vertexOpts VertexPreflightOptions) (*PreflightReport, error) {
	sortedScopes := uniqueSortedStrings(scopes)
	ps := &preflightState{
		clients: clients,
		report: &PreflightReport{
			QuotaProject: clients.QuotaProject,
			Scopes:       sortedScopes,
		},
	}

	projectIDs, err := ps.probeCAIScopes(ctx, sortedScopes)
	if err != nil {
		return ps.report, err
	}
	if err := ps.probeProjectEnrichers(ctx, projectIDs); err != nil {
		return ps.report, err
	}
	ps.probeVertexReadiness(ctx, vertexOpts)

	slices.SortFunc(ps.report.Probes, func(a, b PreflightProbe) int {
		return cmp.Or(cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.Source, b.Source))
	})
	return ps.report, ps.firstError()
}

func newCloudPlatformHTTPClient(ctx context.Context, quotaProject string) (*http.Client, error) {
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{cloudPlatformScope},
	})
	if err != nil {
		return nil, fmt.Errorf("load Vertex AI cloud-platform credentials: %w", err)
	}
	opts := []option.ClientOption{
		option.WithAuthCredentials(creds),
		option.WithTelemetryDisabled(),
	}
	if qp := ResolveQuotaProject(creds, quotaProject); qp != "" {
		opts = append(opts, option.WithQuotaProject(qp))
	}
	hc, _, err := htransport.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create Vertex AI oauth2 HTTP client: %w", err)
	}
	hc.Timeout = defaultHTTPTimeout
	return hc, nil
}

func probeVertexIAMPermission(ctx context.Context, hc *http.Client, crmEndpoint, project string) (bool, error) {
	svc, err := cloudresourcemanager.NewService(ctx, restClientOptions(hc, crmEndpoint)...)
	if err != nil {
		return false, fmt.Errorf("init Cloud Resource Manager client: %w", err)
	}
	resp, err := svc.Projects.TestIamPermissions(project, &cloudresourcemanager.TestIamPermissionsRequest{
		Permissions: []string{"aiplatform.endpoints.predict"},
	}).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	return slices.Contains(resp.Permissions, "aiplatform.endpoints.predict"), nil
}

func probeVertexModelREST(ctx context.Context, hc *http.Client, vertexEndpoint, project, location, model string) error {
	base := strings.TrimRight(strings.TrimSpace(vertexEndpoint), "/")
	if base == "" {
		if location == "global" {
			base = defaultGlobalVertexBase
		} else {
			base = fmt.Sprintf("https://%s-aiplatform.googleapis.com", location)
		}
	}
	endpoint := fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s", base, project, location, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBodyBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxDrainBodyBytes))
	return googleapi.CheckResponseWithBody(resp, body)
}

func iamRemediation(scope, role string) string {
	switch {
	case strings.HasPrefix(scope, prefixProjects):
		id := strings.TrimPrefix(scope, prefixProjects)
		return fmt.Sprintf("gcloud projects add-iam-policy-binding %s --member=<PRINCIPAL> --role=%s", id, role)
	case strings.HasPrefix(scope, prefixFolders):
		id := strings.TrimPrefix(scope, prefixFolders)
		return fmt.Sprintf("gcloud resource-manager folders add-iam-policy-binding %s --member=<PRINCIPAL> --role=%s", id, role)
	case strings.HasPrefix(scope, prefixOrganizations):
		id := strings.TrimPrefix(scope, prefixOrganizations)
		return fmt.Sprintf("gcloud organizations add-iam-policy-binding %s --member=<PRINCIPAL> --role=%s", id, role)
	default:
		return fmt.Sprintf("Grant %s on %s", role, scope)
	}
}

func quotaRemediation(quotaProject, reason string) string {
	qp := quotaProject
	if qp == "" {
		qp = "<QUOTA_PROJECT>"
	}
	switch reason {
	case "SERVICE_DISABLED":
		return fmt.Sprintf("gcloud services enable cloudasset.googleapis.com monitoring.googleapis.com --project=%s", qp)
	case "USER_PROJECT_DENIED":
		return fmt.Sprintf("gcloud projects add-iam-policy-binding %s --member=<PRINCIPAL> --role=roles/serviceusage.serviceUsageConsumer", qp)
	case "BILLING_DISABLED":
		return fmt.Sprintf("Link an active billing account to quota project %s or pass --quota-project <PROJECT>", qp)
	default:
		return fmt.Sprintf("Verify quota project %s permissions and enabled APIs", qp)
	}
}

func uniqueSortedStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
