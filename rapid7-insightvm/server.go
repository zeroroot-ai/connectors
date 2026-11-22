package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tools use the InsightVM Console API v3. The OpenAPI spec is
// https://help.rapid7.com/insightvm/en-us/api/api-v3.json and the reference is
// https://help.rapid7.com/insightvm/en-us/api/index.html
//   - POST /api/3/assets/search          (query page, size, sort; body {"filters":[{"field","operator","value"}],"match":"all"|"any"})
//   - GET  /api/3/assets/{id}
//   - GET  /api/3/assets/{id}/vulnerabilities  (query page, size, sort)
//   - GET  /api/3/vulnerabilities/{id}
// A page answer is {"resources":[...],"page":{"number","size","totalPages","totalResources"}}.
// size has at most 500. Authentication: HTTP basic with the user and the
// password of the console. The search fields and operators are in the overview
// of the spec: ip-address (is) and host-name (is, starts-with, contains).

var (
	vulnIDFormat = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,199}$`)
	ipFormat     = regexp.MustCompile(`^[0-9A-Fa-f:.]{3,45}$`)
	hostFormat   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
)

const (
	defaultSize = 25
	maxSize     = 100
	maxPage     = 10000
)

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	return buildWith(env, log, nil)
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	base := env.URL("INSIGHTVM_URL")
	user := env.Require("INSIGHTVM_USER")
	pass := env.Secret("INSIGHTVM_PASSWORD")
	caFile := env.Optional("INSIGHTVM_CA_FILE", "")
	timeout := env.Duration("INSIGHTVM_TIMEOUT", 30*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if base.Scheme != "https" {
		return nil, errors.New("INSIGHTVM_URL must use https")
	}
	if hc == nil && caFile != "" {
		var err error
		if hc, err = clientWithCA(caFile); err != nil {
			return nil, err
		}
	}
	client := kit.NewClient(base, kit.ClientOptions{Auth: kit.BasicAuth(user, pass), Logger: log, Timeout: timeout, HTTPClient: hc})
	svc := &service{client: client}
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

// clientWithCA returns an HTTP client that trusts the certificates in a PEM
// file, in addition to the system roots. A console often has a certificate
// from a private authority.
func clientWithCA(path string) (*http.Client, error) {
	pem, err := os.ReadFile(path) //nolint:gosec // the path is set by the operator in the environment
	if err != nil {
		return nil, errors.New("INSIGHTVM_CA_FILE cannot be read")
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("INSIGHTVM_CA_FILE holds no certificate")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}

type service struct{ client *kit.Client }

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "find_assets",
		Description: "Find InsightVM assets. Read only. Give ip_address (exact) or host_name (the start of the name), or both. " +
			"Both must match. Returns one page of at most size assets with the count of vulnerabilities of each.",
		Annotations: kit.ReadOnly("Find InsightVM assets"),
	}, v.findAssets)
	kit.AddTool(s, log, &mcp.Tool{
		Name:        "get_asset",
		Description: "Read one InsightVM asset by its id: address, host name, operating system, risk score and the count of vulnerabilities. Read only.",
		Annotations: kit.ReadOnly("Get InsightVM asset"),
	}, v.getAsset)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_asset_vulnerabilities",
		Description: "List the vulnerability findings of one InsightVM asset: the id of the vulnerability, its status, the time since when it is found and the " +
			"number of instances. Read only. One page of at most size findings. Use get_vulnerability for the detail.",
		Annotations: kit.ReadOnly("List InsightVM asset vulnerabilities"),
	}, v.listFindings)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "get_vulnerability",
		Description: "Read one InsightVM vulnerability by its id, for example ssh-cve-2020-14145: title, severity, CVSS scores, CVE ids, " +
			"the dates and whether a fix exists. Read only.",
		Annotations: kit.ReadOnly("Get InsightVM vulnerability"),
	}, v.getVulnerability)
}

type pageIn struct {
	Page int `json:"page,omitempty" jsonschema:"the page, starting at 0. The default is 0."`
	Size int `json:"size,omitempty" jsonschema:"records on a page, 1 to 100. The default is 25."`
}

func (p pageIn) query() (url.Values, error) {
	size := p.Size
	if size == 0 {
		size = defaultSize
	}
	if err := kit.CheckRange("size", size, 1, maxSize); err != nil {
		return nil, fmt.Errorf("check size: %w", err)
	}
	if err := kit.CheckRange("page", p.Page, 0, maxPage); err != nil {
		return nil, fmt.Errorf("check page: %w", err)
	}
	return url.Values{"page": {strconv.Itoa(p.Page)}, "size": {strconv.Itoa(size)}}, nil
}

type pageOut struct {
	Number       int `json:"page"`
	TotalPages   int `json:"total_pages"`
	TotalRecords int `json:"total_records"`
}

type pageMeta struct {
	Number         int `json:"number"`
	TotalPages     int `json:"totalPages"`
	TotalResources int `json:"totalResources"`
}

func (m pageMeta) out() pageOut {
	return pageOut{Number: m.Number, TotalPages: m.TotalPages, TotalRecords: m.TotalResources}
}

// ---- assets ------------------------------------------------------------

type asset struct {
	ID              int64   `json:"id"`
	IP              string  `json:"ip,omitempty"`
	HostName        string  `json:"hostName,omitempty"`
	OS              string  `json:"os,omitempty"`
	Type            string  `json:"type,omitempty"`
	RiskScore       float64 `json:"riskScore"`
	Vulnerabilities struct {
		Critical    int `json:"critical"`
		Severe      int `json:"severe"`
		Moderate    int `json:"moderate"`
		Total       int `json:"total"`
		Exploits    int `json:"exploits"`
		MalwareKits int `json:"malwareKits"`
	} `json:"vulnerabilities"`
}

type vulnCounts struct {
	Critical    int `json:"critical"`
	Severe      int `json:"severe"`
	Moderate    int `json:"moderate"`
	Total       int `json:"total"`
	Exploits    int `json:"exploits"`
	MalwareKits int `json:"malware_kits"`
}

type assetOut struct {
	ID              int64      `json:"asset_id"`
	IP              string     `json:"ip,omitempty"`
	HostName        string     `json:"host_name,omitempty"`
	OS              string     `json:"os,omitempty"`
	Type            string     `json:"type,omitempty"`
	RiskScore       float64    `json:"risk_score"`
	Vulnerabilities vulnCounts `json:"vulnerabilities"`
}

func (a asset) out() assetOut {
	v := a.Vulnerabilities
	return assetOut{
		ID: a.ID, IP: a.IP, HostName: a.HostName, OS: a.OS, Type: a.Type, RiskScore: a.RiskScore,
		Vulnerabilities: vulnCounts{Critical: v.Critical, Severe: v.Severe, Moderate: v.Moderate, Total: v.Total, Exploits: v.Exploits, MalwareKits: v.MalwareKits},
	}
}

type findAssetsIn struct {
	IPAddress string `json:"ip_address,omitempty" jsonschema:"an IPv4 or IPv6 address, exact"`
	HostName  string `json:"host_name,omitempty" jsonschema:"the start of the host name"`
	pageIn
}

type findAssetsOut struct {
	Assets []assetOut `json:"assets"`
	pageOut
}

func (v *service) findAssets(ctx context.Context, _ *mcp.CallToolRequest, in findAssetsIn) (*mcp.CallToolResult, findAssetsOut, error) {
	out := findAssetsOut{Assets: []assetOut{}}
	var filters []map[string]any
	if in.IPAddress != "" {
		if err := kit.CheckString("ip_address", in.IPAddress, 45, ipFormat); err != nil {
			return nil, out, fmt.Errorf("check ip_address: %w", err)
		}
		filters = append(filters, map[string]any{"field": "ip-address", "operator": "is", "value": in.IPAddress})
	}
	if in.HostName != "" {
		if err := kit.CheckString("host_name", in.HostName, 255, hostFormat); err != nil {
			return nil, out, fmt.Errorf("check host_name: %w", err)
		}
		filters = append(filters, map[string]any{"field": "host-name", "operator": "starts-with", "value": in.HostName})
	}
	if len(filters) == 0 {
		return nil, out, fmt.Errorf("%w: give ip_address or host_name", kit.ErrInvalidInput)
	}
	q, err := in.query()
	if err != nil {
		return nil, out, err
	}
	var resp struct {
		Resources []asset  `json:"resources"`
		Page      pageMeta `json:"page"`
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodPost, Path: kit.Path("api", "3", "assets", "search"), Query: q,
		JSON: map[string]any{"filters": filters, "match": "all"}, Retry: true, // a search changes nothing
	}, &resp)
	if err != nil {
		return nil, out, fmt.Errorf("find assets: %w", err)
	}
	for _, a := range resp.Resources {
		out.Assets = append(out.Assets, a.out())
	}
	out.pageOut = resp.Page.out()
	return nil, out, nil
}

// checkAssetID refuses an asset id that is not positive. The spec types the id
// as int64, so a console with many assets can have an id above 2^31.
func checkAssetID(id int64) error {
	if id < 1 {
		return fmt.Errorf("%w: asset_id must be a positive integer", kit.ErrInvalidInput)
	}
	return nil
}

type assetIn struct {
	AssetID int64 `json:"asset_id" jsonschema:"the id of the asset: a positive integer"`
}

func (v *service) getAsset(ctx context.Context, _ *mcp.CallToolRequest, in assetIn) (*mcp.CallToolResult, assetOut, error) {
	var out assetOut
	if err := checkAssetID(in.AssetID); err != nil {
		return nil, out, err
	}
	var a asset
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("api", "3", "assets", strconv.FormatInt(in.AssetID, 10))}, &a); err != nil {
		return nil, out, fmt.Errorf("get asset: %w", err)
	}
	if a.ID != in.AssetID {
		return nil, out, errors.New("get asset: InsightVM did not return the asset")
	}
	return nil, a.out(), nil
}

// ---- findings and vulnerabilities --------------------------------------

type listFindingsIn struct {
	AssetID int64 `json:"asset_id" jsonschema:"the id of the asset: a positive integer"`
	pageIn
}

type finding struct {
	VulnerabilityID string `json:"vulnerability_id"`
	Status          string `json:"status,omitempty"`
	Since           string `json:"since,omitempty"`
	Instances       int    `json:"instances"`
}

type findingsOut struct {
	Findings []finding `json:"findings"`
	pageOut
}

func (v *service) listFindings(ctx context.Context, _ *mcp.CallToolRequest, in listFindingsIn) (*mcp.CallToolResult, findingsOut, error) {
	out := findingsOut{Findings: []finding{}}
	if err := checkAssetID(in.AssetID); err != nil {
		return nil, out, err
	}
	q, err := in.query()
	if err != nil {
		return nil, out, err
	}
	var resp struct {
		Resources []struct {
			ID        any    `json:"id"`
			Status    string `json:"status"`
			Since     string `json:"since"`
			Instances int    `json:"instances"`
		} `json:"resources"`
		Page pageMeta `json:"page"`
	}
	err = v.client.Do(ctx, kit.Request{
		Method: http.MethodGet, Path: kit.Path("api", "3", "assets", strconv.FormatInt(in.AssetID, 10), "vulnerabilities"), Query: q,
	}, &resp)
	if err != nil {
		return nil, out, fmt.Errorf("list asset vulnerabilities: %w", err)
	}
	for _, r := range resp.Resources {
		out.Findings = append(out.Findings, finding{VulnerabilityID: fmt.Sprint(r.ID), Status: r.Status, Since: r.Since, Instances: r.Instances})
	}
	out.pageOut = resp.Page.out()
	return nil, out, nil
}

type vulnIn struct {
	VulnerabilityID string `json:"vulnerability_id" jsonschema:"the id of the vulnerability, for example ssh-cve-2020-14145"`
}

type cvssScore struct {
	Score  float64 `json:"score"`
	Vector string  `json:"vector,omitempty"`
}

type vulnOut struct {
	ID            string     `json:"vulnerability_id"`
	Title         string     `json:"title"`
	Severity      string     `json:"severity,omitempty"`
	SeverityScore int        `json:"severity_score"`
	RiskScore     float64    `json:"risk_score"`
	CVSSv2        *cvssScore `json:"cvss_v2,omitempty"`
	CVSSv3        *cvssScore `json:"cvss_v3,omitempty"`
	CVEs          []string   `json:"cves,omitempty"`
	Published     string     `json:"published,omitempty"`
	Modified      string     `json:"modified,omitempty"`
	FixAvailable  bool       `json:"fix_available"`
	Exploits      int        `json:"exploits"`
	MalwareKits   int        `json:"malware_kits"`
	Description   string     `json:"description,omitempty"`
}

func (v *service) getVulnerability(ctx context.Context, _ *mcp.CallToolRequest, in vulnIn) (*mcp.CallToolResult, vulnOut, error) {
	var out vulnOut
	if err := kit.CheckString("vulnerability_id", in.VulnerabilityID, 200, vulnIDFormat); err != nil {
		return nil, out, fmt.Errorf("check vulnerability_id: %w", err)
	}
	var r struct {
		ID            any      `json:"id"`
		Title         string   `json:"title"`
		Severity      string   `json:"severity"`
		SeverityScore int      `json:"severityScore"`
		RiskScore     float64  `json:"riskScore"`
		CVEs          []string `json:"cves"`
		Published     string   `json:"published"`
		Modified      string   `json:"modified"`
		FixAvailable  bool     `json:"fixAvailable"`
		Exploits      int      `json:"exploits"`
		MalwareKits   int      `json:"malwareKits"`
		Description   struct {
			Text string `json:"text"`
		} `json:"description"`
		CVSS struct {
			V2 *cvssScore `json:"v2"`
			V3 *cvssScore `json:"v3"`
		} `json:"cvss"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("api", "3", "vulnerabilities", in.VulnerabilityID)}, &r); err != nil {
		return nil, out, fmt.Errorf("get vulnerability: %w", err)
	}
	if fmt.Sprint(r.ID) != in.VulnerabilityID {
		return nil, out, errors.New("get vulnerability: InsightVM did not return the vulnerability")
	}
	return nil, vulnOut{
		ID: in.VulnerabilityID, Title: r.Title, Severity: r.Severity, SeverityScore: r.SeverityScore, RiskScore: r.RiskScore,
		CVSSv2: r.CVSS.V2, CVSSv3: r.CVSS.V3, CVEs: r.CVEs, Published: r.Published, Modified: r.Modified, FixAvailable: r.FixAvailable,
		Exploits: r.Exploits, MalwareKits: r.MalwareKits, Description: r.Description.Text,
	}, nil
}
