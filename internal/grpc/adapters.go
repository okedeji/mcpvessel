package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	pb "github.com/okedeji/agentcage/api/proto"
	"github.com/okedeji/agentcage/internal/assessment"
	"github.com/okedeji/agentcage/internal/audit"
	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/findings"
	"github.com/okedeji/agentcage/internal/fleet"
	"github.com/okedeji/agentcage/internal/identity"
	"github.com/okedeji/agentcage/internal/intervention"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Services holds references to all domain servers needed by gRPC adapters.
type Services struct {
	Cages            *cage.Service
	Assessments      *assessment.Service
	Interventions    *intervention.Service
	Fleet            *fleet.Service
	Findings         *findings.PGStore
	Audit            *audit.PGStore
	Pack             PackConfig
	SecretReader     identity.SecretReader
	ConfigServer     *config.Server
	CageLogDir       string
	NATSConn         *nats.Conn
	ServiceLogDir    string
	ConfigYAML       []byte
	CACert           []byte
	ServiceEndpoints *pb.ServiceEndpoints
	Cancel           context.CancelFunc
	Version          string
}

// Register wires all gRPC service adapters onto the server.
func Register(srv *grpc.Server, svc Services) {
	pb.RegisterControlServiceServer(srv, &controlAdapter{
		cancelFunc:       svc.Cancel,
		version:          svc.Version,
		configYAML:       svc.ConfigYAML,
		caCert:           svc.CACert,
		serviceEndpoints: svc.ServiceEndpoints,
		logDir:           svc.ServiceLogDir,
	})
	pb.RegisterCageServiceServer(srv, &cageAdapter{server: svc.Cages, logDir: svc.CageLogDir, natsConn: svc.NATSConn})
	pb.RegisterAssessmentServiceServer(srv, &assessmentAdapter{server: svc.Assessments})
	pb.RegisterInterventionServiceServer(srv, &interventionAdapter{server: svc.Interventions})
	pb.RegisterFleetServiceServer(srv, &fleetAdapter{server: svc.Fleet})
	if svc.Findings != nil {
		pb.RegisterFindingsServiceServer(srv, &findingsAdapter{store: svc.Findings})
	}
	if svc.Audit != nil {
		pb.RegisterAuditServiceServer(srv, &auditAdapter{store: svc.Audit})
	}
	pb.RegisterPackServiceServer(srv, &packAdapter{config: svc.Pack})
	if svc.SecretReader != nil {
		pb.RegisterVaultServiceServer(srv, &vaultAdapter{reader: svc.SecretReader})
	}
	if svc.ConfigServer != nil {
		pb.RegisterConfigServiceServer(srv, &configAdapter{server: svc.ConfigServer})
	}
}

type controlAdapter struct {
	pb.UnimplementedControlServiceServer
	cancelFunc       context.CancelFunc
	version          string
	configYAML       []byte
	caCert           []byte
	serviceEndpoints *pb.ServiceEndpoints
	logDir           string
}

func (a *controlAdapter) Ping(_ context.Context, _ *pb.PingRequest) (*pb.PingResponse, error) {
	return &pb.PingResponse{Version: a.version, Status: "running"}, nil
}

func (a *controlAdapter) Stop(_ context.Context, _ *pb.StopRequest) (*pb.StopResponse, error) {
	a.cancelFunc()
	return &pb.StopResponse{}, nil
}

func (a *controlAdapter) Health(_ context.Context, _ *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{Services: map[string]string{"status": "ok"}}, nil
}

func (a *controlAdapter) GetServiceLog(_ context.Context, req *pb.GetServiceLogRequest) (*pb.GetServiceLogResponse, error) {
	service := req.GetService()
	if service == "" {
		return nil, status.Error(codes.InvalidArgument, "service is required")
	}
	// Prevent path traversal.
	if strings.Contains(service, "/") || strings.Contains(service, "\\") || strings.Contains(service, "..") {
		return nil, status.Error(codes.InvalidArgument, "invalid service name")
	}

	logFile := filepath.Join(a.logDir, service+".log")
	tailLines := int(req.GetTailLines())
	if tailLines <= 0 {
		tailLines = 200
	}

	lines, err := readLogFile(logFile, tailLines)
	if err != nil {
		return &pb.GetServiceLogResponse{}, nil
	}
	return &pb.GetServiceLogResponse{Lines: lines}, nil
}

func (a *controlAdapter) StreamServiceLog(req *pb.StreamServiceLogRequest, stream pb.ControlService_StreamServiceLogServer) error {
	service := req.GetService()
	if service == "" {
		return status.Error(codes.InvalidArgument, "service is required")
	}
	if strings.Contains(service, "/") || strings.Contains(service, "\\") || strings.Contains(service, "..") {
		return status.Error(codes.InvalidArgument, "invalid service name")
	}

	logFile := filepath.Join(a.logDir, service+".log")

	// Send tail lines first.
	tailLines := int(req.GetTailLines())
	if tailLines <= 0 {
		tailLines = 20
	}
	if lines, err := readLogFile(logFile, tailLines); err == nil {
		for _, line := range lines {
			if err := stream.Send(&pb.StreamServiceLogResponse{Line: line}); err != nil {
				return err
			}
		}
	}

	// Follow: poll for new lines every second.
	f, err := os.Open(logFile)
	if err != nil {
		return status.Errorf(codes.NotFound, "log file not found: %v", err)
	}
	defer func() { _ = f.Close() }()

	// Seek to end.
	offset, _ := f.Seek(0, io.SeekEnd)

	buf := make([]byte, 64*1024)
	var partial string
	for {
		select {
		case <-stream.Context().Done():
			return nil
		default:
		}

		n, readErr := f.ReadAt(buf, offset)
		if n > 0 {
			chunk := partial + string(buf[:n])
			partial = ""
			lines := strings.Split(chunk, "\n")
			// Last element may be partial (no trailing newline yet).
			if !strings.HasSuffix(chunk, "\n") {
				partial = lines[len(lines)-1]
				lines = lines[:len(lines)-1]
			}
			for _, line := range lines {
				if line == "" {
					continue
				}
				if err := stream.Send(&pb.StreamServiceLogResponse{Line: line}); err != nil {
					return err
				}
			}
			offset += int64(n)
		}
		if readErr != nil && readErr != io.EOF {
			return nil
		}

		time.Sleep(1 * time.Second)
	}
}

func (a *controlAdapter) GetConfig(_ context.Context, _ *pb.GetConfigRequest) (*pb.GetConfigResponse, error) {
	return &pb.GetConfigResponse{
		ConfigYaml:       a.configYAML,
		CaCert:           a.caCert,
		ServiceEndpoints: a.serviceEndpoints,
	}, nil
}

type cageAdapter struct {
	pb.UnimplementedCageServiceServer
	server   *cage.Service
	logDir   string
	natsConn *nats.Conn
}

func (a *cageAdapter) CreateCage(ctx context.Context, req *pb.CreateCageRequest) (*pb.CreateCageResponse, error) {
	info, err := a.server.CreateCage(ctx, cageConfigFromProto(req.GetConfig()))
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.CreateCageResponse{Cage: cageInfoToProto(info)}, nil
}

func (a *cageAdapter) GetCage(ctx context.Context, req *pb.GetCageRequest) (*pb.GetCageResponse, error) {
	info, err := a.server.GetCage(ctx, req.GetCageId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.GetCageResponse{Cage: cageInfoToProto(info)}, nil
}

func (a *cageAdapter) ListCagesByAssessment(ctx context.Context, req *pb.ListCagesByAssessmentRequest) (*pb.ListCagesByAssessmentResponse, error) {
	if req.GetAssessmentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "assessment_id is required")
	}
	ids, err := a.server.ListByAssessment(ctx, req.GetAssessmentId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.ListCagesByAssessmentResponse{CageIds: ids}, nil
}

const maxLogLines = 10000

func (a *cageAdapter) GetCageLogs(ctx context.Context, req *pb.GetCageLogsRequest) (*pb.GetCageLogsResponse, error) {
	if req.GetCageId() == "" {
		return nil, status.Error(codes.InvalidArgument, "cage_id is required")
	}
	if strings.Contains(req.GetCageId(), "/") || strings.Contains(req.GetCageId(), "\\") || strings.Contains(req.GetCageId(), "..") {
		return nil, status.Error(codes.InvalidArgument, "invalid cage_id")
	}

	info, err := a.server.GetCage(ctx, req.GetCageId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	isRunning := info.State == cage.StateRunning || info.State == cage.StatePending || info.State == cage.StateProvisioning

	tailLines := int(req.GetTailLines())
	if tailLines <= 0 || tailLines > maxLogLines {
		tailLines = maxLogLines
	}

	suffix := ".log"
	if req.GetSerial() {
		suffix = ".serial.log"
	}
	logFile := filepath.Join(a.logDir, req.GetCageId()+suffix)
	lines, err := readLogFile(logFile, tailLines)
	if err != nil {
		return &pb.GetCageLogsResponse{IsRunning: isRunning}, nil
	}

	return &pb.GetCageLogsResponse{Lines: lines, IsRunning: isRunning}, nil
}

func readLogFile(path string, tailLines int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	if tailLines <= 0 {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		return strings.Split(strings.TrimRight(string(data), "\n"), "\n"), nil
	}

	// Read from the end for tail to avoid loading the entire file.
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	const chunkSize = 64 * 1024
	size := stat.Size()
	var buf []byte

	for offset := size; offset > 0; {
		readSize := int64(chunkSize)
		if readSize > offset {
			readSize = offset
		}
		offset -= readSize

		chunk := make([]byte, readSize)
		if _, err := f.ReadAt(chunk, offset); err != nil && err != io.EOF {
			return nil, err
		}
		buf = append(chunk, buf...)

		count := 0
		for _, b := range buf {
			if b == '\n' {
				count++
			}
		}
		if count > tailLines {
			break
		}
	}

	allLines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if len(allLines) > tailLines {
		allLines = allLines[len(allLines)-tailLines:]
	}
	return allLines, nil
}

func (a *cageAdapter) StreamCageLogs(req *pb.StreamCageLogsRequest, stream pb.CageService_StreamCageLogsServer) error {
	cageID := req.GetCageId()
	if cageID == "" {
		return status.Error(codes.InvalidArgument, "cage_id is required")
	}
	if strings.ContainsAny(cageID, "/\\..") {
		return status.Error(codes.InvalidArgument, "invalid cage_id")
	}

	info, err := a.server.GetCage(stream.Context(), cageID)
	if err != nil {
		return toGRPCError(err)
	}

	isActive := info.State == cage.StateRunning || info.State == cage.StatePending ||
		info.State == cage.StateProvisioning || info.State == cage.StatePaused

	// Send historical tail lines.
	tailLines := int(req.GetTailLines())
	if tailLines <= 0 {
		tailLines = 200
	}
	logFile := filepath.Join(a.logDir, cageID+".log")
	lines, _ := readLogFile(logFile, tailLines)
	sourceFilter := req.GetSourceFilter()
	for _, line := range lines {
		if sourceFilter != "" && !strings.Contains(line, `"source":"`+sourceFilter+`"`) {
			continue
		}
		if err := stream.Send(&pb.StreamCageLogsResponse{Line: line}); err != nil {
			return err
		}
	}

	if !isActive {
		_ = stream.Send(&pb.StreamCageLogsResponse{Completed: true, CageState: info.State.String()})
		return nil
	}

	// Live streaming via NATS subscription.
	if a.natsConn != nil {
		return a.streamViaNATS(cageID, sourceFilter, stream)
	}

	// Fallback: poll the log file.
	return a.streamViaPolling(cageID, sourceFilter, stream, int32(len(lines)))
}

func (a *cageAdapter) streamViaNATS(cageID, sourceFilter string, stream pb.CageService_StreamCageLogsServer) error {
	ch := make(chan string, 256)
	sub, err := a.natsConn.Subscribe(cage.LogSubject(cageID), func(msg *nats.Msg) {
		select {
		case ch <- string(msg.Data):
		default:
		}
	})
	if err != nil {
		return a.streamViaPolling(cageID, sourceFilter, stream, 0)
	}
	defer func() { _ = sub.Unsubscribe() }()

	stateTicker := time.NewTicker(5 * time.Second)
	defer stateTicker.Stop()

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case line := <-ch:
			if sourceFilter != "" && !strings.Contains(line, `"source":"`+sourceFilter+`"`) {
				continue
			}
			if err := stream.Send(&pb.StreamCageLogsResponse{Line: line}); err != nil {
				return err
			}
		case <-stateTicker.C:
			info, err := a.server.GetCage(stream.Context(), cageID)
			if err != nil {
				continue
			}
			if info.State == cage.StateCompleted || info.State == cage.StateFailed {
				_ = stream.Send(&pb.StreamCageLogsResponse{Completed: true, CageState: info.State.String()})
				return nil
			}
		}
	}
}

func (a *cageAdapter) streamViaPolling(cageID, sourceFilter string, stream pb.CageService_StreamCageLogsServer, lastCount int32) error {
	logFile := filepath.Join(a.logDir, cageID+".log")
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	stateTicker := time.NewTicker(5 * time.Second)
	defer stateTicker.Stop()

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-ticker.C:
			lines, err := readLogFile(logFile, 0)
			if err != nil {
				continue
			}
			for i := lastCount; i < int32(len(lines)); i++ {
				line := lines[i]
				if sourceFilter != "" && !strings.Contains(line, `"source":"`+sourceFilter+`"`) {
					continue
				}
				if err := stream.Send(&pb.StreamCageLogsResponse{Line: line}); err != nil {
					return err
				}
			}
			lastCount = int32(len(lines))
		case <-stateTicker.C:
			info, err := a.server.GetCage(stream.Context(), cageID)
			if err != nil {
				continue
			}
			if info.State == cage.StateCompleted || info.State == cage.StateFailed {
				_ = stream.Send(&pb.StreamCageLogsResponse{Completed: true, CageState: info.State.String()})
				return nil
			}
		}
	}
}

func (a *cageAdapter) DestroyCage(ctx context.Context, req *pb.DestroyCageRequest) (*pb.DestroyCageResponse, error) {
	if err := a.server.DestroyCage(ctx, req.GetCageId(), req.GetReason()); err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.DestroyCageResponse{}, nil
}

type assessmentAdapter struct {
	pb.UnimplementedAssessmentServiceServer
	server *assessment.Service
}

const (
	maxTagCount    = 50
	maxTagKeyLen   = 128
	maxTagValueLen = 1024
)

func validateTags(tags map[string]string) error {
	if len(tags) > maxTagCount {
		return fmt.Errorf("tags has %d entries, max %d", len(tags), maxTagCount)
	}
	for k, v := range tags {
		if len(k) > maxTagKeyLen {
			return fmt.Errorf("tag key %q exceeds %d characters", k, maxTagKeyLen)
		}
		if len(v) > maxTagValueLen {
			return fmt.Errorf("tag value for key %q exceeds %d characters", k, maxTagValueLen)
		}
	}
	return nil
}

func (a *assessmentAdapter) CreateAssessment(ctx context.Context, req *pb.CreateAssessmentRequest) (*pb.CreateAssessmentResponse, error) {
	if err := validateTags(req.GetConfig().GetTags()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tags: %v", err)
	}
	cfg := assessmentConfigFromProto(req.GetConfig())
	cfg.BundleRef = req.GetBundleRef()
	info, err := a.server.CreateAssessment(ctx, cfg)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.CreateAssessmentResponse{Assessment: assessmentInfoToProto(info)}, nil
}

func (a *assessmentAdapter) GetAssessment(ctx context.Context, req *pb.GetAssessmentRequest) (*pb.GetAssessmentResponse, error) {
	info, err := a.server.GetAssessment(ctx, req.GetAssessmentId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.GetAssessmentResponse{Assessment: assessmentInfoToProto(info)}, nil
}

func (a *assessmentAdapter) ListAssessments(ctx context.Context, req *pb.ListAssessmentsRequest) (*pb.ListAssessmentsResponse, error) {
	filters := assessment.ListFilters{
		Limit:     int(req.GetLimit()),
		PageToken: req.GetPageToken(),
	}
	if req.GetStatusFilter() != pb.AssessmentStatus_ASSESSMENT_STATUS_UNSPECIFIED {
		s := assessmentStatusFromProto(req.GetStatusFilter())
		filters.StatusFilter = &s
	}

	items, nextToken, err := a.server.ListAssessments(ctx, filters)
	if err != nil {
		return nil, toGRPCError(err)
	}

	pbItems := make([]*pb.AssessmentInfo, len(items))
	for i := range items {
		pbItems[i] = assessmentInfoToProto(&items[i])
	}
	return &pb.ListAssessmentsResponse{Assessments: pbItems, NextPageToken: nextToken}, nil
}

func (a *assessmentAdapter) CancelAssessment(ctx context.Context, req *pb.CancelAssessmentRequest) (*pb.CancelAssessmentResponse, error) {
	if req.GetAssessmentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "assessment_id is required")
	}
	if err := a.server.CancelAssessment(ctx, req.GetAssessmentId()); err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.CancelAssessmentResponse{Cancelled: true}, nil
}

func (a *assessmentAdapter) FinishAssessment(ctx context.Context, req *pb.FinishAssessmentRequest) (*pb.FinishAssessmentResponse, error) {
	if req.GetAssessmentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "assessment_id is required")
	}
	if err := a.server.FinishAssessment(ctx, req.GetAssessmentId()); err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.FinishAssessmentResponse{}, nil
}

func (a *assessmentAdapter) GetReport(ctx context.Context, req *pb.GetReportRequest) (*pb.GetReportResponse, error) {
	if req.GetAssessmentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "assessment_id is required")
	}
	data, err := a.server.LoadReport(ctx, req.GetAssessmentId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.GetReportResponse{ReportJson: data}, nil
}

type interventionAdapter struct {
	pb.UnimplementedInterventionServiceServer
	server *intervention.Service
}

func (a *interventionAdapter) GetIntervention(ctx context.Context, req *pb.GetInterventionRequest) (*pb.GetInterventionResponse, error) {
	if req.GetInterventionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "intervention_id is required")
	}
	r, err := a.server.GetIntervention(ctx, req.GetInterventionId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.GetInterventionResponse{Intervention: interventionToProto(r)}, nil
}

func (a *interventionAdapter) ListInterventions(ctx context.Context, req *pb.ListInterventionsRequest) (*pb.ListInterventionsResponse, error) {
	filters := intervention.ListFilters{
		AssessmentID: req.GetAssessmentIdFilter(),
		PageSize:     int(req.GetPageSize()),
		PageToken:    req.GetPageToken(),
	}
	if req.GetStatusFilter() != pb.InterventionStatus_INTERVENTION_STATUS_UNSPECIFIED {
		s := interventionStatusFromProto(req.GetStatusFilter())
		filters.StatusFilter = &s
	}
	if req.GetTypeFilter() != pb.InterventionType_INTERVENTION_TYPE_UNSPECIFIED {
		t := interventionTypeFromProto(req.GetTypeFilter())
		filters.TypeFilter = &t
	}

	items, nextToken, err := a.server.ListInterventions(ctx, filters)
	if err != nil {
		return nil, toGRPCError(err)
	}

	pbItems := make([]*pb.InterventionInfo, len(items))
	for i, item := range items {
		pbItems[i] = interventionToProto(&item)
	}
	return &pb.ListInterventionsResponse{Interventions: pbItems, NextPageToken: nextToken}, nil
}

func (a *interventionAdapter) ResolveCageIntervention(ctx context.Context, req *pb.ResolveCageInterventionRequest) (*pb.ResolveCageInterventionResponse, error) {
	action := interventionActionFromProto(req.GetAction())
	if err := a.server.ResolveCageIntervention(ctx, req.GetInterventionId(), action, req.GetRationale(), req.GetAdjustments(), "operator"); err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.ResolveCageInterventionResponse{}, nil
}

func (a *interventionAdapter) ResolveAssessmentReview(ctx context.Context, req *pb.ResolveAssessmentReviewRequest) (*pb.ResolveAssessmentReviewResponse, error) {
	decision := reviewDecisionFromProto(req.GetDecision())
	var adjustments []intervention.FindingAdjustment
	for _, adj := range req.GetAdjustments() {
		adjustments = append(adjustments, intervention.FindingAdjustment{
			FindingID:        adj.GetFindingId(),
			SeverityOverride: adj.GetSeverityOverride(),
			Rationale:        adj.GetRationale(),
		})
	}
	if err := a.server.ResolveAssessmentReview(ctx, req.GetInterventionId(), decision, req.GetRationale(), adjustments, "operator"); err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.ResolveAssessmentReviewResponse{}, nil
}

func (a *interventionAdapter) ResolvePlanApproval(ctx context.Context, req *pb.ResolvePlanApprovalRequest) (*pb.ResolvePlanApprovalResponse, error) {
	decision := planDecisionFromProto(req.GetDecision())
	if err := a.server.ResolvePlanApproval(ctx, req.GetInterventionId(), decision, req.GetRationale(), req.GetFeedback(), "operator"); err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.ResolvePlanApprovalResponse{}, nil
}

type fleetAdapter struct {
	pb.UnimplementedFleetServiceServer
	server *fleet.Service
}

func (a *fleetAdapter) GetFleetStatus(ctx context.Context, _ *pb.GetFleetStatusRequest) (*pb.GetFleetStatusResponse, error) {
	fs, err := a.server.GetFleetStatus(ctx)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.GetFleetStatusResponse{Status: fleetStatusToProto(fs)}, nil
}

func (a *fleetAdapter) ListHosts(ctx context.Context, req *pb.ListHostsRequest) (*pb.ListHostsResponse, error) {
	var poolFilter *fleet.HostPool
	if req.GetPoolFilter() != pb.HostPool_HOST_POOL_UNSPECIFIED {
		p := poolFromProto(req.GetPoolFilter())
		poolFilter = &p
	}
	hosts, err := a.server.ListHosts(ctx, poolFilter)
	if err != nil {
		return nil, toGRPCError(err)
	}
	pbHosts := make([]*pb.HostInfo, len(hosts))
	for i, h := range hosts {
		pbHosts[i] = hostToProto(h)
	}
	return &pb.ListHostsResponse{Hosts: pbHosts}, nil
}

func (a *fleetAdapter) DrainHost(ctx context.Context, req *pb.DrainHostRequest) (*pb.DrainHostResponse, error) {
	if err := a.server.DrainHost(ctx, req.GetHostId(), req.GetReason(), req.GetForce()); err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.DrainHostResponse{}, nil
}

func (a *fleetAdapter) GetCapacity(ctx context.Context, _ *pb.GetCapacityRequest) (*pb.GetCapacityResponse, error) {
	pools, available, err := a.server.GetCapacity(ctx)
	if err != nil {
		return nil, toGRPCError(err)
	}
	pbPools := make([]*pb.PoolStatus, len(pools))
	for i, p := range pools {
		pbPools[i] = poolStatusToProto(p)
	}
	return &pb.GetCapacityResponse{Pools: pbPools, AvailableCageSlots: available}, nil
}

type findingsAdapter struct {
	pb.UnimplementedFindingsServiceServer
	store *findings.PGStore
}

func (a *findingsAdapter) ListFindings(ctx context.Context, req *pb.ListFindingsRequest) (*pb.ListFindingsResponse, error) {
	if req.GetAssessmentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "assessment_id is required")
	}
	filters := findings.ListFilters{
		AssessmentID: req.GetAssessmentId(),
		Limit:        int(req.GetLimit()),
	}
	if req.GetStatusFilter() != pb.FindingStatus_FINDING_STATUS_UNSPECIFIED {
		s := findingStatusFromProto(req.GetStatusFilter())
		filters.StatusFilter = &s
	}
	if req.GetSeverityFilter() != pb.FindingSeverity_FINDING_SEVERITY_UNSPECIFIED {
		sev := findingSeverityFromProto(req.GetSeverityFilter())
		filters.SeverityFilter = &sev
	}

	items, err := a.store.ListFindings(ctx, filters)
	if err != nil {
		return nil, toGRPCError(err)
	}

	pbItems := make([]*pb.FindingInfo, len(items))
	for i := range items {
		pbItems[i] = findingToProto(&items[i])
	}
	return &pb.ListFindingsResponse{Findings: pbItems}, nil
}

func (a *findingsAdapter) GetFinding(ctx context.Context, req *pb.GetFindingRequest) (*pb.GetFindingResponse, error) {
	if req.GetFindingId() == "" {
		return nil, status.Error(codes.InvalidArgument, "finding_id is required")
	}
	f, err := a.store.GetByID(ctx, req.GetFindingId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.GetFindingResponse{Finding: findingToProto(&f)}, nil
}

func (a *findingsAdapter) DeleteFinding(ctx context.Context, req *pb.DeleteFindingRequest) (*pb.DeleteFindingResponse, error) {
	if req.GetFindingId() == "" {
		return nil, status.Error(codes.InvalidArgument, "finding_id is required")
	}
	if err := a.store.DeleteFinding(ctx, req.GetFindingId()); err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.DeleteFindingResponse{}, nil
}

func (a *findingsAdapter) DeleteByAssessment(ctx context.Context, req *pb.DeleteByAssessmentRequest) (*pb.DeleteByAssessmentResponse, error) {
	if req.GetAssessmentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "assessment_id is required")
	}
	n, err := a.store.DeleteByAssessment(ctx, req.GetAssessmentId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.DeleteByAssessmentResponse{Deleted: n}, nil
}

type auditAdapter struct {
	pb.UnimplementedAuditServiceServer
	store *audit.PGStore
}

func (a *auditAdapter) VerifyChain(ctx context.Context, req *pb.VerifyChainRequest) (*pb.VerifyChainResponse, error) {
	if req.GetCageId() == "" {
		return nil, status.Error(codes.InvalidArgument, "cage_id is required")
	}
	entries, err := a.store.GetEntries(ctx, req.GetCageId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	if len(entries) == 0 {
		return &pb.VerifyChainResponse{Valid: false, Error: "no audit entries found"}, nil
	}

	// Verify chain linkage and sequence continuity without HMAC
	// signature verification (which requires Vault key access).
	// This catches deleted/reordered entries and broken hash links.
	if err := audit.VerifyChainLinkage(entries); err != nil {
		return &pb.VerifyChainResponse{Valid: false, Error: err.Error(), EntryCount: int64(len(entries))}, nil
	}

	return &pb.VerifyChainResponse{Valid: true, EntryCount: int64(len(entries))}, nil
}

func (a *auditAdapter) GetEntries(ctx context.Context, req *pb.GetEntriesRequest) (*pb.GetEntriesResponse, error) {
	if req.GetCageId() == "" {
		return nil, status.Error(codes.InvalidArgument, "cage_id is required")
	}
	entries, err := a.store.GetEntriesFiltered(ctx, req.GetCageId(), req.GetTypeFilter(), int(req.GetLimit()))
	if err != nil {
		return nil, toGRPCError(err)
	}
	pbEntries := make([]*pb.AuditEntry, len(entries))
	for i, e := range entries {
		pbEntries[i] = auditEntryToProto(e)
	}
	return &pb.GetEntriesResponse{Entries: pbEntries}, nil
}

func (a *auditAdapter) GetEntry(ctx context.Context, req *pb.GetEntryRequest) (*pb.GetEntryResponse, error) {
	if req.GetEntryId() == "" {
		return nil, status.Error(codes.InvalidArgument, "entry_id is required")
	}
	e, err := a.store.GetEntryByID(ctx, req.GetEntryId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	if e == nil {
		return nil, status.Error(codes.NotFound, "audit entry not found")
	}
	return &pb.GetEntryResponse{Entry: auditEntryToProto(*e)}, nil
}

func (a *auditAdapter) GetDigest(ctx context.Context, req *pb.GetDigestRequest) (*pb.GetDigestResponse, error) {
	if req.GetCageId() == "" {
		return nil, status.Error(codes.InvalidArgument, "cage_id is required")
	}
	d, err := a.store.GetDigest(ctx, req.GetCageId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	if d == nil {
		return &pb.GetDigestResponse{}, nil
	}
	return &pb.GetDigestResponse{Digest: auditDigestToProto(*d)}, nil
}

func (a *auditAdapter) ExportCage(ctx context.Context, req *pb.ExportCageRequest) (*pb.ExportCageResponse, error) {
	if req.GetCageId() == "" {
		return nil, status.Error(codes.InvalidArgument, "cage_id is required")
	}
	entries, err := a.store.GetEntries(ctx, req.GetCageId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	digest, err := a.store.GetDigest(ctx, req.GetCageId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	if len(entries) == 0 {
		return nil, status.Error(codes.NotFound, "no audit entries for cage")
	}
	data, err := audit.Export(entries, digest)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.ExportCageResponse{ExportJson: data}, nil
}

func (a *auditAdapter) ListCagesWithAudit(ctx context.Context, req *pb.ListCagesWithAuditRequest) (*pb.ListCagesWithAuditResponse, error) {
	ids, err := a.store.ListCagesWithAudit(ctx, req.GetAssessmentId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.ListCagesWithAuditResponse{CageIds: ids}, nil
}

func (a *auditAdapter) GetKeyVersions(ctx context.Context, req *pb.GetKeyVersionsRequest) (*pb.GetKeyVersionsResponse, error) {
	if req.GetCageId() == "" {
		return nil, status.Error(codes.InvalidArgument, "cage_id is required")
	}
	versions, err := a.store.GetKeyVersions(ctx, req.GetCageId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.GetKeyVersionsResponse{KeyVersions: versions}, nil
}

func (a *auditAdapter) ChainStatus(ctx context.Context, req *pb.ChainStatusRequest) (*pb.ChainStatusResponse, error) {
	if req.GetCageId() == "" {
		return nil, status.Error(codes.InvalidArgument, "cage_id is required")
	}
	entries, err := a.store.GetEntries(ctx, req.GetCageId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	if len(entries) == 0 {
		return nil, status.Error(codes.NotFound, "no audit entries for cage")
	}
	digest, _ := a.store.GetDigest(ctx, req.GetCageId())
	versions, _ := a.store.GetKeyVersions(ctx, req.GetCageId())

	resp := &pb.ChainStatusResponse{
		CageId:          entries[0].CageID,
		AssessmentId:    entries[0].AssessmentID,
		EntryCount:      int64(len(entries)),
		FirstTimestamp:  timestamppb.New(entries[0].Timestamp),
		LatestTimestamp: timestamppb.New(entries[len(entries)-1].Timestamp),
		HasDigest:       digest != nil,
		KeyVersions:     versions,
	}
	return resp, nil
}

func auditEntryToProto(e audit.Entry) *pb.AuditEntry {
	return &pb.AuditEntry{
		Id:           e.ID,
		CageId:       e.CageID,
		AssessmentId: e.AssessmentID,
		Sequence:     e.Sequence,
		Type:         e.Type.String(),
		Timestamp:    timestamppb.New(e.Timestamp),
		Data:         e.Data,
		KeyVersion:   e.KeyVersion,
		Signature:    e.Signature,
		PreviousHash: e.PreviousHash,
	}
}

func auditDigestToProto(d audit.Digest) *pb.AuditDigest {
	return &pb.AuditDigest{
		AssessmentId:  d.AssessmentID,
		CageId:        d.CageID,
		ChainHeadHash: d.ChainHeadHash,
		EntryCount:    d.EntryCount,
		KeyVersion:    d.KeyVersion,
		Signature:     d.Signature,
		IssuedAt:      timestamppb.New(d.IssuedAt),
	}
}

func toGRPCError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, cage.ErrCageNotFound) || errors.Is(err, assessment.ErrAssessmentNotFound) || errors.Is(err, findings.ErrFindingNotFound) {
		return status.Error(codes.NotFound, err.Error())
	}
	if errors.Is(err, cage.ErrInvalidTransition) || errors.Is(err, intervention.ErrNotPending) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	msg := err.Error()
	for _, keyword := range []string{"validating", "invalid", "rejected"} {
		if strings.Contains(msg, keyword) {
			return status.Error(codes.InvalidArgument, msg)
		}
	}
	return status.Error(codes.Internal, msg)
}
