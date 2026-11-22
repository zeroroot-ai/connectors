package main

import (
	"context"
	"errors"
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

// The tools use the Trellix EDR API. The pages are on docs.trellix.com:
//   - GET /edr/v3/alerts (query from, to in epoch milliseconds, sort, filter,
//     page[offset], page[limit]):
//     https://docs.trellix.com/docs/get-alerts-1.md
//   - GET /edr/v2/remediation/host-info (query aGuid, hostName as a prefix,
//     page[offset], page[limit]):
//     https://docs.trellix.com/docs/get-host-info-1.md
// Headers of each call: Authorization: Bearer <token>, x-api-key: <key>,
// Content-Type: application/vnd.api+json. The answer is JSON:API:
// {"data":[{"type","id","attributes":{...}}],"meta":{"totalResourceCount"},"links":{...}}.
// Authentication: OAuth 2.0 client credentials, the token URL
// https://iam.mcafee-cloud.com/iam/v1.1/token, the client id and secret in the
// authorization header ("Auth Style: In Header"), and the scopes listed in
// https://docs.trellix.com/network-and-platform/docs/trellix-edr.md
// The page of GET /edr/v3/alerts does not state the syntax of filter and sort,
// so the tool does not send them.

const (
	defaultAPI    = "https://api.manage.trellix.com"
	defaultToken  = "https://iam.mcafee-cloud.com/iam/v1.1/token"
	defaultScopes = "epo.evt.r,epo.device.r,mi.user.investigate,soc.rts.c,soc.rts.r,soc.hts.r"
	maxLimit      = 100
	defaultLimit  = 25
	maxOffset     = 100000
)

var (
	guidFormat = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
	hostFormat = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
	scopeList  = regexp.MustCompile(`^[A-Za-z0-9._,:-]{1,500}$`)
)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	clientID := env.Require("TRELLIX_CLIENT_ID")
	secret := env.Secret("TRELLIX_CLIENT_SECRET")
	apiKey := env.Secret("TRELLIX_API_KEY")
	api := env.OptionalURL("TRELLIX_API_URL", defaultAPI)
	tokenURL := env.OptionalURL("TRELLIX_TOKEN_URL", defaultToken)
	scopes := env.Optional("TRELLIX_SCOPES", defaultScopes)
	timeout := env.Duration("TRELLIX_TIMEOUT", 20*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if api.Scheme != "https" || tokenURL.Scheme != "https" {
		return nil, errors.New("TRELLIX_API_URL and TRELLIX_TOKEN_URL must use https")
	}
	if !scopeList.MatchString(scopes) {
		return nil, errors.New("TRELLIX_SCOPES has the wrong format")
	}
	cc := &kit.ClientCredentials{
		TokenURL: tokenURL.String(), ID: clientID, Secret: secret,
		Extra: url.Values{"scope": {scopes}}, HTTPClient: hc,
	}
	bearer := cc.Authenticator()
	client := kit.NewClient(api, kit.ClientOptions{
		Auth: func(ctx context.Context, req *http.Request) error {
			req.Header.Set("x-api-key", apiKey.Reveal())
			req.Header.Set("Content-Type", "application/vnd.api+json")
			return bearer(ctx, req)
		},
		Logger: log, Timeout: timeout, HTTPClient: hc,
	})
	svc := &service{client: client}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct{ client *kit.Client }

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_alerts",
		Description: "Read Trellix EDR alerts (detections): rule, severity (s0 to s5), process, command line, user, MITRE tags and host. " +
			"Read only. All inputs are optional. since and until are RFC 3339 times on the detection time. " +
			"Returns at most limit alerts from offset. has_more is true when more exist.",
		Annotations: kit.ReadOnly("List Trellix EDR alerts"),
	}, v.listAlerts)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_hosts",
		Description: "Read the state of Trellix hosts: agent GUID, host name, operating system, last boot time and network interfaces. " +
			"Read only. Give agent_guid for one host, or host_name for a prefix search. Returns at most limit hosts from offset. " +
			"This server has no isolate or release tool. The release call is not confirmed.",
		Annotations: kit.ReadOnly("List Trellix hosts"),
	}, v.listHosts)
}

type page struct {
	Limit  int `json:"limit,omitempty" jsonschema:"number of records, 1 to 100. The default is 25."`
	Offset int `json:"offset,omitempty" jsonschema:"number of records to skip. The default is 0."`
}

func (p page) query(q url.Values) error {
	limit := p.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	if err := kit.CheckRange("limit", limit, 1, maxLimit); err != nil {
		return fmt.Errorf("check limit: %w", err)
	}
	if err := kit.CheckRange("offset", p.Offset, 0, maxOffset); err != nil {
		return fmt.Errorf("check offset: %w", err)
	}
	q.Set("page[limit]", strconv.Itoa(limit))
	q.Set("page[offset]", strconv.Itoa(p.Offset))
	return nil
}

type envelope struct {
	Meta struct {
		TotalResourceCount int `json:"totalResourceCount"`
	} `json:"meta"`
}

// hasMore tells if more records exist. The links of the answer show a next
// link on the last page too, so the tool does not use them. When the answer has
// a total, more exist when the total is past this page. The alerts answer of
// the example has no total. Then a full page means that more can exist.
func hasMore(p page, count, total int) bool {
	if total > 0 {
		return p.Offset+count < total
	}
	limit := p.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	return count >= limit
}

// ---- alerts ------------------------------------------------------------

type alertsInput struct {
	Since string `json:"since,omitempty" jsonschema:"RFC 3339 time, for example 2026-11-20T10:00:00Z"`
	Until string `json:"until,omitempty" jsonschema:"RFC 3339 time"`
	page
}

type alert struct {
	ID          string   `json:"id"`
	TraceID     string   `json:"trace_id,omitempty"`
	Detected    string   `json:"detection_date,omitempty"`
	EventDate   string   `json:"event_date,omitempty"`
	Activity    string   `json:"activity,omitempty"`
	Severity    string   `json:"severity,omitempty"`
	Score       int      `json:"score,omitempty"`
	Rank        int      `json:"rank,omitempty"`
	RuleID      string   `json:"rule_id,omitempty"`
	Artifact    string   `json:"artifact,omitempty"`
	HostName    string   `json:"host_name,omitempty"`
	HostOS      string   `json:"host_os,omitempty"`
	AgentGUID   string   `json:"agent_guid,omitempty"`
	ProcessName string   `json:"process_name,omitempty"`
	CommandLine string   `json:"command_line,omitempty"`
	User        string   `json:"user,omitempty"`
	Tags        []string `json:"detection_tags,omitempty"`
}

type alertsOutput struct {
	Alerts  []alert `json:"alerts"`
	Count   int     `json:"count"`
	Total   int     `json:"total"`
	HasMore bool    `json:"has_more"`
}

func (v *service) listAlerts(ctx context.Context, _ *mcp.CallToolRequest, in alertsInput) (*mcp.CallToolResult, alertsOutput, error) {
	out := alertsOutput{Alerts: []alert{}}
	q := url.Values{}
	for _, t := range []struct{ field, key, val string }{{"since", "from", in.Since}, {"until", "to", in.Until}} {
		if t.val == "" {
			continue
		}
		if err := kit.CheckString(t.field, t.val, 40, nil); err != nil {
			return nil, out, fmt.Errorf("check %s: %w", t.field, err)
		}
		ts, err := time.Parse(time.RFC3339, t.val)
		if err != nil {
			return nil, out, fmt.Errorf("%w: %s is not an RFC 3339 time", kit.ErrInvalidInput, t.field)
		}
		q.Set(t.key, strconv.FormatInt(ts.UnixMilli(), 10))
	}
	if err := in.query(q); err != nil {
		return nil, out, err
	}
	var resp struct {
		envelope
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				TraceID     string   `json:"Trace_Id"`
				Detected    string   `json:"DetectionDate"`
				EventDate   string   `json:"Event_Date"`
				Activity    string   `json:"Activity"`
				Severity    string   `json:"Severity"`
				Score       int      `json:"Score"`
				Rank        int      `json:"Rank"`
				RuleID      string   `json:"RuleId"`
				Artifact    string   `json:"Artifact"`
				HostName    string   `json:"Host_Name"`
				HostOS      string   `json:"Host_OS"`
				MAGUID      string   `json:"MAGUID"`
				ProcessName string   `json:"ProcessName"`
				CommandLine string   `json:"CommandLine"`
				Tags        []string `json:"Detection_Tags"`
				User        struct {
					Domain string `json:"domain"`
					Name   string `json:"name"`
				} `json:"User"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("edr", "v3", "alerts"), Query: q}, &resp); err != nil {
		return nil, out, fmt.Errorf("list alerts: %w", err)
	}
	for _, d := range resp.Data {
		a := d.Attributes
		user := a.User.Name
		if a.User.Domain != "" {
			user = a.User.Domain + `\` + a.User.Name
		}
		out.Alerts = append(out.Alerts, alert{
			ID: d.ID, TraceID: a.TraceID, Detected: a.Detected, EventDate: a.EventDate, Activity: a.Activity, Severity: a.Severity,
			Score: a.Score, Rank: a.Rank, RuleID: a.RuleID, Artifact: a.Artifact, HostName: a.HostName, HostOS: a.HostOS,
			AgentGUID: a.MAGUID, ProcessName: a.ProcessName, CommandLine: a.CommandLine, User: user, Tags: a.Tags,
		})
	}
	out.Count, out.Total = len(out.Alerts), resp.Meta.TotalResourceCount
	out.HasMore = hasMore(in.page, out.Count, out.Total)
	return nil, out, nil
}

// ---- hosts -------------------------------------------------------------

type hostsInput struct {
	AgentGUID string `json:"agent_guid,omitempty" jsonschema:"the GUID of the agent of one host"`
	HostName  string `json:"host_name,omitempty" jsonschema:"the start of the host name"`
	page
}

type iface struct {
	Name string `json:"name,omitempty"`
	MAC  string `json:"mac,omitempty"`
	IP   string `json:"ip,omitempty"`
}

type host struct {
	AgentGUID    string  `json:"agent_guid"`
	HostName     string  `json:"host_name"`
	HostOS       string  `json:"host_os,omitempty"`
	OSDesc       string  `json:"os,omitempty"`
	LastBootTime string  `json:"last_boot_time,omitempty"`
	Interfaces   []iface `json:"interfaces,omitempty"`
}

type hostsOutput struct {
	Hosts   []host `json:"hosts"`
	Count   int    `json:"count"`
	Total   int    `json:"total"`
	HasMore bool   `json:"has_more"`
}

func (v *service) listHosts(ctx context.Context, _ *mcp.CallToolRequest, in hostsInput) (*mcp.CallToolResult, hostsOutput, error) {
	out := hostsOutput{Hosts: []host{}}
	q := url.Values{}
	if in.AgentGUID != "" {
		if err := kit.CheckString("agent_guid", in.AgentGUID, 36, guidFormat); err != nil {
			return nil, out, fmt.Errorf("check agent_guid: %w", err)
		}
		q.Set("aGuid", in.AgentGUID)
	}
	if in.HostName != "" {
		if err := kit.CheckString("host_name", in.HostName, 255, hostFormat); err != nil {
			return nil, out, fmt.Errorf("check host_name: %w", err)
		}
		q.Set("hostName", in.HostName)
	}
	if err := in.query(q); err != nil {
		return nil, out, err
	}
	var resp struct {
		envelope
		Data []struct {
			Attributes struct {
				AGuid        string `json:"aGuid"`
				Hostname     string `json:"hostname"`
				HostOS       string `json:"hostOs"`
				LastBootTime string `json:"lastBootTime"`
				OS           struct {
					Desc string `json:"desc"`
				} `json:"os"`
				NetInterfaces []iface `json:"netInterfaces"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("edr", "v2", "remediation", "host-info"), Query: q}, &resp); err != nil {
		return nil, out, fmt.Errorf("list hosts: %w", err)
	}
	for _, d := range resp.Data {
		a := d.Attributes
		out.Hosts = append(out.Hosts, host{
			AgentGUID: a.AGuid, HostName: a.Hostname, HostOS: a.HostOS, OSDesc: a.OS.Desc, LastBootTime: a.LastBootTime, Interfaces: a.NetInterfaces,
		})
	}
	out.Count, out.Total = len(out.Hosts), resp.Meta.TotalResourceCount
	out.HasMore = hasMore(in.page, out.Count, out.Total)
	return nil, out, nil
}
