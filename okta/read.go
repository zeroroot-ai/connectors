package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The read tools follow the same Okta Management API (OpenAPI spec
// https://github.com/okta/okta-management-openapi-spec, file
// dist/current/management-minimal.yaml, and the reference at
// https://developer.okta.com/docs/api/openapi/okta-management/management/tag/SystemLog/,
// .../tag/User/):
//   - GET /api/v1/logs                (listLogEvents, scope okta.logs.read)
//   - GET /api/v1/users               (listUsers, scope okta.users.read)
//   - GET /api/v1/users/{userId}      (getUser, scope okta.users.read)
// There is no session read tool. See the README.

const (
	defaultLimit  = 50
	maxLogLimit   = 200
	maxUserLimit  = 50
	maxExpr       = 400 // bytes of a filter, search or keyword input
	maxCursor     = 512 // bytes of a page cursor
	maxKeywords   = 10  // keywords of the q parameter of the log
	maxKeywordLen = 40  // characters of one keyword
)

func (v *service) registerRead(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_log_events",
		Description: "Read system log events of the Okta org. Read only. All inputs are optional. " +
			"since and until are RFC 3339 times, for example 2026-11-20T10:00:00Z. " +
			"filter is an Okta SCIM filter, for example eventType eq \"user.session.start\". " +
			"q is a list of up to 10 keywords of at most 40 characters each. Okta returns one page of at most limit events, " +
			"newest first unless sort_order is ASCENDING. When more events exist, the result has truncated true and " +
			"an after cursor. Give that cursor as after to read the next page.",
		Annotations: kit.ReadOnly("List Okta system log events"),
	}, v.listLogEvents)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_users",
		Description: "Find Okta users. Read only. Give search, an Okta search expression such as " +
			"profile.login eq \"a@example.com\", or q, a prefix of the name or e-mail address. " +
			"Give at least one of the two. Returns at most limit users.",
		Annotations: kit.ReadOnly("List Okta users"),
	}, v.listUsers)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "get_user",
		Description: "Read one Okta user: status, lifecycle times and the main profile fields. Read only. " +
			"To act on the user, use suspend_user or revoke_user_sessions.",
		Annotations: kit.ReadOnly("Get Okta user"),
	}, v.getUser)
}

type logInput struct {
	Since     string `json:"since,omitempty" jsonschema:"lower time bound of the event time, RFC 3339. Okta default: 7 days before until."`
	Until     string `json:"until,omitempty" jsonschema:"upper time bound of the event time, RFC 3339. Okta default: now."`
	Filter    string `json:"filter,omitempty" jsonschema:"Okta SCIM filter expression, at most 400 bytes"`
	Q         string `json:"q,omitempty" jsonschema:"keywords, separated by a space. At most 10 keywords of 40 characters."`
	Limit     int    `json:"limit,omitempty" jsonschema:"number of events, 1 to 200. The default is 50."`
	SortOrder string `json:"sort_order,omitempty" jsonschema:"ASCENDING or DESCENDING by event time. The default is DESCENDING, newest first."`
	After     string `json:"after,omitempty" jsonschema:"the after value of the previous result, to read the next page"`
}

// logActor is the actor or a target of a log event.
type logActor struct {
	ID          string `json:"id,omitempty"`
	Type        string `json:"type,omitempty"`
	AlternateID string `json:"alternateId,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// logEvent is the part of a LogEvent that a defender needs.
type logEvent struct {
	UUID           string `json:"uuid"`
	Published      string `json:"published"`
	EventType      string `json:"eventType"`
	DisplayMessage string `json:"displayMessage,omitempty"`
	Severity       string `json:"severity,omitempty"`
	Outcome        *struct {
		Result string `json:"result,omitempty"`
		Reason string `json:"reason,omitempty"`
	} `json:"outcome,omitempty"`
	Actor  *logActor  `json:"actor,omitempty"`
	Target []logActor `json:"target,omitempty"`
	Client *struct {
		IPAddress string `json:"ipAddress,omitempty"`
		UserAgent *struct {
			RawUserAgent string `json:"rawUserAgent,omitempty"`
		} `json:"userAgent,omitempty"`
	} `json:"client,omitempty"`
	AuthenticationContext *struct {
		ExternalSessionID string `json:"externalSessionId,omitempty"`
	} `json:"authenticationContext,omitempty"`
	Transaction *struct {
		ID string `json:"id,omitempty"`
	} `json:"transaction,omitempty"`
}

type logOutput struct {
	Events []logEvent `json:"events"`
	Count  int        `json:"count"`
	// Truncated is true when Okta has more events for the same query. After
	// is the cursor of the next page.
	Truncated bool   `json:"truncated"`
	After     string `json:"after,omitempty"`
}

func (v *service) listLogEvents(ctx context.Context, _ *mcp.CallToolRequest, in logInput) (*mcp.CallToolResult, logOutput, error) {
	out := logOutput{Events: []logEvent{}}
	q := url.Values{}
	for _, t := range []struct{ field, key, val string }{{"since", "since", in.Since}, {"until", "until", in.Until}} {
		if t.val == "" {
			continue
		}
		if err := kit.CheckString(t.field, t.val, 40, nil); err != nil {
			return nil, out, err
		}
		if _, err := time.Parse(time.RFC3339, t.val); err != nil {
			return nil, out, fmt.Errorf("%w: %s is not an RFC 3339 time", kit.ErrInvalidInput, t.field)
		}
		q.Set(t.key, t.val)
	}
	if in.Filter != "" {
		if err := kit.CheckString("filter", in.Filter, maxExpr, nil); err != nil {
			return nil, out, err
		}
		q.Set("filter", in.Filter)
	}
	if in.Q != "" {
		if err := checkKeywords(in.Q); err != nil {
			return nil, out, err
		}
		q.Set("q", in.Q)
	}
	if in.After != "" {
		if err := kit.CheckString("after", in.After, maxCursor, nil); err != nil {
			return nil, out, err
		}
		q.Set("after", in.After)
	}
	limit, err := limitOf(in.Limit, maxLogLimit)
	if err != nil {
		return nil, out, err
	}
	q.Set("limit", strconv.Itoa(limit))
	switch in.SortOrder {
	case "":
		q.Set("sortOrder", "DESCENDING")
	case "ASCENDING", "DESCENDING":
		q.Set("sortOrder", in.SortOrder)
	default:
		return nil, out, fmt.Errorf("%w: sort_order must be ASCENDING or DESCENDING", kit.ErrInvalidInput)
	}
	var hdr http.Header
	err = v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("api", "v1", "logs"), Query: q, ResponseHeader: &hdr}, &out.Events)
	if err != nil {
		return nil, out, fmt.Errorf("list log events: %w", err)
	}
	out.Count = len(out.Events)
	out.After = nextCursor(hdr)
	out.Truncated = out.After != ""
	return nil, out, nil
}

// checkKeywords enforces the limits of the Okta q parameter: at most 10
// keywords, each at most 40 characters.
func checkKeywords(q string) error {
	if err := kit.CheckString("q", q, maxExpr, nil); err != nil {
		return fmt.Errorf("check q: %w", err)
	}
	words := strings.Fields(q)
	if len(words) > maxKeywords {
		return fmt.Errorf("%w: q has more than %d keywords", kit.ErrInvalidInput, maxKeywords)
	}
	for _, w := range words {
		if utf8.RuneCountInString(w) > maxKeywordLen {
			return fmt.Errorf("%w: a keyword of q is longer than %d characters", kit.ErrInvalidInput, maxKeywordLen)
		}
	}
	return nil
}

// nextCursor returns the after value of the Link header with rel="next", or
// an empty string. Okta sends the self link and the next link in two Link
// headers. The tool never follows the link. It takes the cursor only, so the
// credential goes to the fixed base URL.
func nextCursor(h http.Header) string {
	for _, value := range h.Values("Link") {
		for _, part := range strings.Split(value, ",") {
			target, params, ok := strings.Cut(strings.TrimSpace(part), ">;")
			if !ok || !strings.HasPrefix(target, "<") || !strings.Contains(strings.ReplaceAll(params, " ", ""), `rel="next"`) {
				continue
			}
			u, err := url.Parse(target[1:])
			if err != nil {
				continue
			}
			return u.Query().Get("after")
		}
	}
	return ""
}

func limitOf(n, maxN int) (int, error) {
	if n == 0 {
		return defaultLimit, nil
	}
	if err := kit.CheckRange("limit", n, 1, maxN); err != nil {
		return 0, fmt.Errorf("check limit: %w", err)
	}
	return n, nil
}

type userInfo struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	Created         string `json:"created,omitempty"`
	Activated       string `json:"activated,omitempty"`
	StatusChanged   string `json:"statusChanged,omitempty"`
	LastLogin       string `json:"lastLogin,omitempty"`
	LastUpdated     string `json:"lastUpdated,omitempty"`
	PasswordChanged string `json:"passwordChanged,omitempty"`
	Profile         struct {
		Login     string `json:"login,omitempty"`
		Email     string `json:"email,omitempty"`
		FirstName string `json:"firstName,omitempty"`
		LastName  string `json:"lastName,omitempty"`
	} `json:"profile"`
}

type listUsersInput struct {
	Search string `json:"search,omitempty" jsonschema:"Okta search expression, for example profile.login eq \"a@example.com\". At most 400 bytes."`
	Q      string `json:"q,omitempty" jsonschema:"prefix of first name, last name or e-mail address"`
	Limit  int    `json:"limit,omitempty" jsonschema:"number of users, 1 to 50. The default is 50."`
}

type usersOutput struct {
	Users []userInfo `json:"users"`
	Count int        `json:"count"`
}

func (v *service) listUsers(ctx context.Context, _ *mcp.CallToolRequest, in listUsersInput) (*mcp.CallToolResult, usersOutput, error) {
	out := usersOutput{Users: []userInfo{}}
	if in.Search == "" && in.Q == "" {
		return nil, out, fmt.Errorf("%w: give search or q", kit.ErrInvalidInput)
	}
	q := url.Values{}
	if in.Search != "" {
		if err := kit.CheckString("search", in.Search, maxExpr, nil); err != nil {
			return nil, out, err
		}
		q.Set("search", in.Search)
	}
	if in.Q != "" {
		if err := kit.CheckString("q", in.Q, 100, nil); err != nil {
			return nil, out, err
		}
		q.Set("q", in.Q)
	}
	limit, err := limitOf(in.Limit, maxUserLimit)
	if err != nil {
		return nil, out, err
	}
	q.Set("limit", strconv.Itoa(limit))
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("api", "v1", "users"), Query: q}, &out.Users); err != nil {
		return nil, out, fmt.Errorf("list users: %w", err)
	}
	out.Count = len(out.Users)
	return nil, out, nil
}

func (v *service) getUser(ctx context.Context, _ *mcp.CallToolRequest, in userInput) (*mcp.CallToolResult, userInfo, error) {
	var out userInfo
	if err := kit.CheckString("user_id", in.UserID, 20, userIDFormat); err != nil {
		return nil, out, err
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("api", "v1", "users", in.UserID)}, &out); err != nil {
		return nil, out, fmt.Errorf("get user: %w", err)
	}
	return nil, out, nil
}
