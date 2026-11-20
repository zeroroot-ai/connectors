package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tool follows the Host List VM Detection API of Qualys:
// "Qualys VM/PC API User Guide", chapter Assets, "Host List Detection"
// https://cdn2.qualys.com/docs/qualys-api-vmpc-user-guide.pdf
//   - GET /api/2.0/fo/asset/host/vm/detection/?action=list
//     with the parameters ids, ips, status, severities, show_results,
//     truncation_limit
//   - HTTP basic authentication, and the header X-Requested-With is required
//   - answer XML: HOST_LIST_VM_DETECTION_OUTPUT / RESPONSE / HOST_LIST / HOST
//     (ID, IP, DNS, OS, LAST_VM_SCANNED_DATE, DETECTION_LIST / DETECTION with
//     QID, TYPE, SEVERITY, STATUS, FIRST_FOUND_DATETIME, LAST_FOUND_DATETIME,
//     TIMES_FOUND, RESULTS) and WARNING / URL when the list is truncated
//   - an error answer is <SIMPLE_RETURN><RESPONSE><CODE/><TEXT/>

const (
	detectionPath = "/api/2.0/fo/asset/host/vm/detection/"
	defaultLimit  = 50
	maxLimit      = 500
	maxResults    = 1000 // characters of the scan output of one detection
)

var (
	hostIDFormat = regexp.MustCompile(`^\d{1,12}$`)
	statuses     = map[string]string{"new": "New", "active": "Active", "re-opened": "Re-Opened", "fixed": "Fixed"}
)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	base := env.URL("QUALYS_API_URL")
	user := env.Require("QUALYS_USERNAME")
	pass := env.Secret("QUALYS_PASSWORD")
	timeout := env.Duration("QUALYS_TIMEOUT", 60*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if base.Scheme != "https" {
		return nil, errors.New("QUALYS_API_URL must use https")
	}
	basic := kit.BasicAuth(user, pass)
	auth := func(ctx context.Context, req *http.Request) error {
		req.Header.Set("X-Requested-With", "zeroroot-connectors")
		return basic(ctx, req)
	}
	svc := &service{client: kit.NewClient(base, kit.ClientOptions{Auth: auth, Logger: log, Timeout: timeout, HTTPClient: hc, MaxBody: 32 << 20})}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct{ client *kit.Client }

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "get_host_detections",
		Description: "Read the latest vulnerability detections of the Qualys hosts that match one host id or one IP address. " +
			"Read only. A detection is a flaw on a host: a QID, its severity from 1 to 5, its status (New, Active, " +
			"Re-Opened, Fixed) and the times that Qualys first and last found it.",
		Annotations: kit.ReadOnly("Qualys host detections"),
	}, v.getHostDetections)
}

type detectionsInput struct {
	HostID         string   `json:"host_id,omitempty" jsonschema:"Qualys host id, a number. Give host_id or ip."`
	IP             string   `json:"ip,omitempty" jsonschema:"one IPv4 or IPv6 address. Give host_id or ip."`
	Status         []string `json:"status,omitempty" jsonschema:"keep detections with these statuses: New, Active, Re-Opened, Fixed. The default is New, Active and Re-Opened."`
	MinSeverity    int      `json:"min_severity,omitempty" jsonschema:"keep detections with this severity or higher, 1 to 5"`
	IncludeResults bool     `json:"include_results,omitempty" jsonschema:"include the scan output of each detection, cut to 1000 characters. The default is false."`
	Limit          int      `json:"limit,omitempty" jsonschema:"maximum number of detections to return, 1 to 500. The default is 50."`
}

type detection struct {
	QID        string `json:"qid"`
	UniqueID   string `json:"unique_vuln_id,omitempty"`
	Type       string `json:"type,omitempty"`
	Severity   int    `json:"severity"`
	Status     string `json:"status"`
	FirstFound string `json:"first_found,omitempty"`
	LastFound  string `json:"last_found,omitempty"`
	TimesFound int    `json:"times_found,omitempty"`
	Results    string `json:"results,omitempty"`
}

type hostOut struct {
	HostID      string      `json:"host_id"`
	IP          string      `json:"ip"`
	DNS         string      `json:"dns,omitempty"`
	OS          string      `json:"os,omitempty"`
	LastScanned string      `json:"last_vm_scanned,omitempty"`
	Detections  []detection `json:"detections"`
}

type detectionsOutput struct {
	Hosts     []hostOut `json:"hosts"`
	Total     int       `json:"total_detections"`
	Truncated bool      `json:"truncated"`
}

// ---- the XML of Qualys ----

type xmlDetection struct {
	QID        string `xml:"QID"`
	UniqueID   string `xml:"UNIQUE_VULN_ID"`
	Type       string `xml:"TYPE"`
	Severity   string `xml:"SEVERITY"`
	Status     string `xml:"STATUS"`
	FirstFound string `xml:"FIRST_FOUND_DATETIME"`
	LastFound  string `xml:"LAST_FOUND_DATETIME"`
	TimesFound string `xml:"TIMES_FOUND"`
	Results    string `xml:"RESULTS"`
}

type xmlHost struct {
	ID          string         `xml:"ID"`
	IP          string         `xml:"IP"`
	DNS         string         `xml:"DNS"`
	OS          string         `xml:"OS"`
	LastScanned string         `xml:"LAST_VM_SCANNED_DATE"`
	Detections  []xmlDetection `xml:"DETECTION_LIST>DETECTION"`
}

type xmlOutput struct {
	XMLName  xml.Name  `xml:"HOST_LIST_VM_DETECTION_OUTPUT"`
	Hosts    []xmlHost `xml:"RESPONSE>HOST_LIST>HOST"`
	WarnCode string    `xml:"RESPONSE>WARNING>CODE"`
}

type xmlSimpleReturn struct {
	XMLName xml.Name `xml:"SIMPLE_RETURN"`
	Code    string   `xml:"RESPONSE>CODE"`
	Text    string   `xml:"RESPONSE>TEXT"`
}

func (v *service) getHostDetections(ctx context.Context, _ *mcp.CallToolRequest, in detectionsInput) (*mcp.CallToolResult, detectionsOutput, error) {
	out := detectionsOutput{Hosts: []hostOut{}}
	q, limit, err := buildQuery(in)
	if err != nil {
		return nil, out, err
	}
	data, err := v.client.DoBytes(ctx, kit.Request{Method: http.MethodGet, Path: detectionPath, Query: q})
	if err != nil {
		return nil, out, qualysError(err)
	}
	var doc xmlOutput
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, out, errors.New("get host detections: the Qualys answer is not the expected XML")
	}
	returned := 0
	for _, h := range doc.Hosts {
		ho := hostOut{HostID: h.ID, IP: h.IP, DNS: strings.TrimSpace(h.DNS), OS: strings.TrimSpace(h.OS),
			LastScanned: h.LastScanned, Detections: []detection{}}
		for _, d := range h.Detections {
			out.Total++
			if returned >= limit {
				out.Truncated = true
				continue
			}
			returned++
			sev, _ := strconv.Atoi(strings.TrimSpace(d.Severity))
			times, _ := strconv.Atoi(strings.TrimSpace(d.TimesFound))
			det := detection{QID: d.QID, UniqueID: d.UniqueID, Type: d.Type, Severity: sev, Status: d.Status,
				FirstFound: d.FirstFound, LastFound: d.LastFound, TimesFound: times}
			if in.IncludeResults {
				det.Results = cut(strings.TrimSpace(d.Results), maxResults)
			}
			ho.Detections = append(ho.Detections, det)
		}
		out.Hosts = append(out.Hosts, ho)
	}
	if doc.WarnCode != "" {
		out.Truncated = true
	}
	return nil, out, nil
}

func buildQuery(in detectionsInput) (url.Values, int, error) {
	if (in.HostID == "") == (in.IP == "") {
		return nil, 0, fmt.Errorf("%w: give host_id or ip, not both and not neither", kit.ErrInvalidInput)
	}
	q := url.Values{"action": {"list"}, "output_format": {"XML"}, "truncation_limit": {"10"}}
	if in.HostID != "" {
		if err := kit.CheckString("host_id", in.HostID, 12, hostIDFormat); err != nil {
			return nil, 0, err
		}
		q.Set("ids", in.HostID)
	} else {
		if err := kit.CheckString("ip", in.IP, 45, nil); err != nil {
			return nil, 0, err
		}
		if net.ParseIP(in.IP) == nil {
			return nil, 0, fmt.Errorf("%w: ip must be one IPv4 or IPv6 address", kit.ErrInvalidInput)
		}
		q.Set("ips", in.IP)
	}
	if len(in.Status) > 0 {
		list := make([]string, 0, len(in.Status))
		for _, s := range in.Status {
			canon, ok := statuses[strings.ToLower(s)]
			if !ok {
				return nil, 0, fmt.Errorf("%w: status must be New, Active, Re-Opened or Fixed", kit.ErrInvalidInput)
			}
			list = append(list, canon)
		}
		q.Set("status", strings.Join(list, ","))
	}
	if in.MinSeverity != 0 {
		if err := kit.CheckRange("min_severity", in.MinSeverity, 1, 5); err != nil {
			return nil, 0, err
		}
		q.Set("severities", fmt.Sprintf("%d-5", in.MinSeverity))
	}
	if !in.IncludeResults {
		q.Set("show_results", "0")
	}
	limit := defaultLimit
	if in.Limit != 0 {
		if err := kit.CheckRange("limit", in.Limit, 1, maxLimit); err != nil {
			return nil, 0, err
		}
		limit = in.Limit
	}
	return q, limit, nil
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// qualysError shows the code and the text of a SIMPLE_RETURN error.
func qualysError(err error) error {
	var ae *kit.APIError
	if errors.As(err, &ae) {
		var sr xmlSimpleReturn
		if xml.Unmarshal([]byte(ae.Body), &sr) == nil && sr.Text != "" {
			return fmt.Errorf("get host detections: Qualys answered HTTP %d, code %s: %s", ae.Status, strings.TrimSpace(sr.Code), strings.TrimSpace(sr.Text))
		}
	}
	return fmt.Errorf("get host detections: %w", err)
}
