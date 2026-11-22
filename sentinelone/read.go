package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The read tools follow the same Management API of SentinelOne. The reference
// needs a console login, so the shapes come from public sources:
//   - GET /web/api/v2.1/threats  (query ids, agentIds, incidentStatuses,
//     mitigationStatuses, createdAt__gte, createdAt__lte, limit, cursor)
//     answer {"data":[{"id", "threatInfo", "agentRealtimeInfo",
//     "agentDetectionInfo"}], "pagination":{"nextCursor","totalItems"}}
//     (the response fields: https://docs.blinkops.com/docs/integrations/sentinelone/actions/get-threats
//     and https://xsoar.pan.dev/docs/reference/integrations/sentinel-one-v2).
//   - GET /web/api/v2.1/agents  (query ids, computerName, networkStatuses,
//     isActive, limit, cursor)
//     answer {"data":[{"id", "computerName", "networkStatus", "isActive", ...}],
//     "pagination":{...}} (the response fields: the same XSOAR page).
// The names of the query parameters are not checked against the official
// reference. A wrong name makes SentinelOne ignore or refuse the filter. The
// README states this gap.

var (
	idListFormat = regexp.MustCompile(`^\d{10,24}$`)
	cursorFormat = regexp.MustCompile(`^[A-Za-z0-9_=+/.-]{1,512}$`)
	nameFormat   = regexp.MustCompile(`^[A-Za-z0-9._ -]{1,255}$`)
	enumFormat   = regexp.MustCompile(`^[a-z_]{1,32}$`)
)

const (
	defaultLimit = 25
	maxLimit     = 100
)

func (v *service) registerRead(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_threats",
		Description: "Read SentinelOne threats. Read only. All inputs are optional. " +
			"Filter by threat_id, agent_id, incident_status (unresolved, in_progress or resolved), " +
			"mitigation_status (for example mitigated, active, blocked, suspicious or pending) " +
			"and by created_after and created_before (RFC 3339). Returns at most limit threats. " +
			"Give next_cursor from a result as cursor to read the next page.",
		Annotations: kit.ReadOnly("List SentinelOne threats"),
	}, v.listThreats)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_agents",
		Description: "Read the state of SentinelOne agents: network status, activity, active threat count, " +
			"version and last activity. Read only. All inputs are optional. " +
			"Filter by agent_id, computer_name (exact), network_status (connected, connecting, disconnected or " +
			"disconnecting) and is_active. Returns at most limit agents. " +
			"Give next_cursor from a result as cursor to read the next page.",
		Annotations: kit.ReadOnly("List SentinelOne agents"),
	}, v.listAgents)
}

type page struct {
	NextCursor string `json:"next_cursor,omitempty"`
	TotalItems int    `json:"total_items"`
}

type pagination struct {
	NextCursor string `json:"nextCursor"`
	TotalItems int    `json:"totalItems"`
}

type threatsInput struct {
	ThreatID         string `json:"threat_id,omitempty" jsonschema:"one threat id: a decimal number"`
	AgentID          string `json:"agent_id,omitempty" jsonschema:"one agent id: a decimal number"`
	IncidentStatus   string `json:"incident_status,omitempty" jsonschema:"unresolved, in_progress or resolved"`
	MitigationStatus string `json:"mitigation_status,omitempty" jsonschema:"for example mitigated, active, blocked, suspicious or pending"`
	CreatedAfter     string `json:"created_after,omitempty" jsonschema:"RFC 3339 time, for example 2026-11-20T10:00:00Z"`
	CreatedBefore    string `json:"created_before,omitempty" jsonschema:"RFC 3339 time"`
	Limit            int    `json:"limit,omitempty" jsonschema:"number of threats, 1 to 100. The default is 25."`
	Cursor           string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous result"`
}

type threat struct {
	ID         string `json:"id"`
	ThreatInfo struct {
		ThreatName       string `json:"threatName,omitempty"`
		Classification   string `json:"classification,omitempty"`
		ConfidenceLevel  string `json:"confidenceLevel,omitempty"`
		AnalystVerdict   string `json:"analystVerdict,omitempty"`
		IncidentStatus   string `json:"incidentStatus,omitempty"`
		MitigationStatus string `json:"mitigationStatus,omitempty"`
		CreatedAt        string `json:"createdAt,omitempty"`
		FilePath         string `json:"filePath,omitempty"`
		SHA256           string `json:"sha256,omitempty"`
	} `json:"threatInfo"`
	AgentRealtimeInfo struct {
		AgentID           string `json:"agentId,omitempty"`
		AgentComputerName string `json:"agentComputerName,omitempty"`
		AgentOsType       string `json:"agentOsType,omitempty"`
	} `json:"agentRealtimeInfo"`
}

type threatsOutput struct {
	Threats []threat `json:"threats"`
	page
}

func (v *service) listThreats(ctx context.Context, _ *mcp.CallToolRequest, in threatsInput) (*mcp.CallToolResult, threatsOutput, error) {
	out := threatsOutput{Threats: []threat{}}
	q := url.Values{}
	if err := setID(q, "ids", "threat_id", in.ThreatID); err != nil {
		return nil, out, err
	}
	if err := setID(q, "agentIds", "agent_id", in.AgentID); err != nil {
		return nil, out, err
	}
	if err := setEnum(q, "incidentStatuses", "incident_status", in.IncidentStatus); err != nil {
		return nil, out, err
	}
	if err := setEnum(q, "mitigationStatuses", "mitigation_status", in.MitigationStatus); err != nil {
		return nil, out, err
	}
	if err := setTime(q, "createdAt__gte", "created_after", in.CreatedAfter); err != nil {
		return nil, out, err
	}
	if err := setTime(q, "createdAt__lte", "created_before", in.CreatedBefore); err != nil {
		return nil, out, err
	}
	if err := setPaging(q, in.Limit, in.Cursor); err != nil {
		return nil, out, err
	}
	var resp struct {
		Data       []threat   `json:"data"`
		Pagination pagination `json:"pagination"`
	}
	if err := v.get(ctx, "threats", q, &resp); err != nil {
		return nil, out, fmt.Errorf("list threats: %w", err)
	}
	if resp.Data != nil {
		out.Threats = resp.Data
	}
	out.NextCursor, out.TotalItems = resp.Pagination.NextCursor, resp.Pagination.TotalItems
	return nil, out, nil
}

type agentsInput struct {
	AgentID       string `json:"agent_id,omitempty" jsonschema:"one agent id: a decimal number"`
	ComputerName  string `json:"computer_name,omitempty" jsonschema:"exact computer name"`
	NetworkStatus string `json:"network_status,omitempty" jsonschema:"connected, connecting, disconnected or disconnecting"`
	IsActive      *bool  `json:"is_active,omitempty" jsonschema:"true for agents that were active lately"`
	Limit         int    `json:"limit,omitempty" jsonschema:"number of agents, 1 to 100. The default is 25."`
	Cursor        string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous result"`
}

type agentInfo struct {
	ID               string `json:"id"`
	ComputerName     string `json:"computerName,omitempty"`
	NetworkStatus    string `json:"networkStatus,omitempty"`
	IsActive         bool   `json:"isActive"`
	IsDecommissioned bool   `json:"isDecommissioned"`
	ActiveThreats    int    `json:"activeThreats"`
	AgentVersion     string `json:"agentVersion,omitempty"`
	OSName           string `json:"osName,omitempty"`
	MachineType      string `json:"machineType,omitempty"`
	ExternalIP       string `json:"externalIp,omitempty"`
	LastActiveDate   string `json:"lastActiveDate,omitempty"`
	SiteName         string `json:"siteName,omitempty"`
}

type agentsOutput struct {
	Agents []agentInfo `json:"agents"`
	page
}

func (v *service) listAgents(ctx context.Context, _ *mcp.CallToolRequest, in agentsInput) (*mcp.CallToolResult, agentsOutput, error) {
	out := agentsOutput{Agents: []agentInfo{}}
	q := url.Values{}
	if err := setID(q, "ids", "agent_id", in.AgentID); err != nil {
		return nil, out, err
	}
	if in.ComputerName != "" {
		if err := kit.CheckString("computer_name", in.ComputerName, 255, nameFormat); err != nil {
			return nil, out, err
		}
		q.Set("computerName", in.ComputerName)
	}
	if err := setEnum(q, "networkStatuses", "network_status", in.NetworkStatus); err != nil {
		return nil, out, err
	}
	if in.IsActive != nil {
		q.Set("isActive", strconv.FormatBool(*in.IsActive))
	}
	if err := setPaging(q, in.Limit, in.Cursor); err != nil {
		return nil, out, err
	}
	var resp struct {
		Data       []agentInfo `json:"data"`
		Pagination pagination  `json:"pagination"`
	}
	if err := v.get(ctx, "agents", q, &resp); err != nil {
		return nil, out, fmt.Errorf("list agents: %w", err)
	}
	if resp.Data != nil {
		out.Agents = resp.Data
	}
	out.NextCursor, out.TotalItems = resp.Pagination.NextCursor, resp.Pagination.TotalItems
	return nil, out, nil
}

func (v *service) get(ctx context.Context, resource string, q url.Values, out any) error {
	err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("web", "api", "v2.1", resource), Query: q}, out)
	if err != nil {
		return fmt.Errorf("get %s: %w", resource, err)
	}
	return nil
}

func setID(q url.Values, key, field, val string) error {
	if val == "" {
		return nil
	}
	if err := kit.CheckString(field, val, 24, idListFormat); err != nil {
		return fmt.Errorf("check %s: %w", field, err)
	}
	q.Set(key, val)
	return nil
}

func setEnum(q url.Values, key, field, val string) error {
	if val == "" {
		return nil
	}
	if err := kit.CheckString(field, val, 32, enumFormat); err != nil {
		return fmt.Errorf("check %s: %w", field, err)
	}
	q.Set(key, val)
	return nil
}

func setTime(q url.Values, key, field, val string) error {
	if val == "" {
		return nil
	}
	if err := kit.CheckString(field, val, 40, nil); err != nil {
		return fmt.Errorf("check %s: %w", field, err)
	}
	if _, err := time.Parse(time.RFC3339, val); err != nil {
		return fmt.Errorf("%w: %s is not an RFC 3339 time", kit.ErrInvalidInput, field)
	}
	q.Set(key, val)
	return nil
}

func setPaging(q url.Values, limit int, cursor string) error {
	if limit == 0 {
		limit = defaultLimit
	}
	if err := kit.CheckRange("limit", limit, 1, maxLimit); err != nil {
		return fmt.Errorf("check limit: %w", err)
	}
	q.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		if err := kit.CheckString("cursor", cursor, 512, cursorFormat); err != nil {
			return fmt.Errorf("check cursor: %w", err)
		}
		q.Set("cursor", cursor)
	}
	return nil
}
