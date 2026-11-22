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

// The read tools follow the same Okta Management API (OpenAPI spec
// https://github.com/okta/okta-management-openapi-spec, file
// dist/current/management-minimal.yaml, and the reference at
// https://developer.okta.com/docs/api/openapi/okta-management/management/tag/SystemLog/,
// .../tag/User/ and .../tag/Session/):
//   - GET /api/v1/logs                (listLogEvents, scope okta.logs.read)
//   - GET /api/v1/users               (listUsers, scope okta.users.read)
//   - GET /api/v1/users/{userId}      (getUser, scope okta.users.read)
//   - GET /api/v1/sessions/{sessionId} (getSession, scope okta.sessions.read)
// Okta has no call that lists the sessions of one user. A log event of a
// sign-in holds the session id in authenticationContext.externalSessionId.

// sessionIDFormat is the format of an Okta session id. Okta documents it as a
// string with no fixed length, so the pattern limits the characters only.
var sessionIDFormat = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const (
	defaultLimit = 50
	maxLogLimit  = 200
	maxUserLimit = 50
	maxExpr      = 400 // bytes of a filter, search or keyword input
)

func (v *service) registerRead(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "list_log_events",
		Description: "Read system log events of the Okta org. Read only. All inputs are optional. " +
			"since and until are RFC 3339 times, for example 2026-11-20T10:00:00Z. " +
			"filter is an Okta SCIM filter, for example eventType eq \"user.session.start\". " +
			"q is a list of up to 10 keywords. Okta returns one page of at most limit events, " +
			"oldest first unless sort_order is DESCENDING.",
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
	kit.AddTool(s, log, &mcp.Tool{
		Name: "get_session",
		Description: "Read one Okta session by its id: user id, login, status, creation and expiry time. " +
			"Read only. Okta has no call that lists the sessions of a user. Get the session id from " +
			"authenticationContext.externalSessionId of a log event.",
		Annotations: kit.ReadOnly("Get Okta session"),
	}, v.getSession)
}

type logInput struct {
	Since     string `json:"since,omitempty" jsonschema:"lower time bound of the event time, RFC 3339. Okta default: 7 days before until."`
	Until     string `json:"until,omitempty" jsonschema:"upper time bound of the event time, RFC 3339. Okta default: now."`
	Filter    string `json:"filter,omitempty" jsonschema:"Okta SCIM filter expression, at most 400 bytes"`
	Q         string `json:"q,omitempty" jsonschema:"keywords, separated by a space. At most 10 keywords of 40 characters."`
	Limit     int    `json:"limit,omitempty" jsonschema:"number of events, 1 to 200. The default is 50."`
	SortOrder string `json:"sort_order,omitempty" jsonschema:"ASCENDING or DESCENDING by event time. The default is ASCENDING."`
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
	for _, t := range []struct{ field, key, val string }{{"filter", "filter", in.Filter}, {"q", "q", in.Q}} {
		if t.val == "" {
			continue
		}
		if err := kit.CheckString(t.field, t.val, maxExpr, nil); err != nil {
			return nil, out, err
		}
		q.Set(t.key, t.val)
	}
	limit, err := limitOf(in.Limit, maxLogLimit)
	if err != nil {
		return nil, out, err
	}
	q.Set("limit", strconv.Itoa(limit))
	switch in.SortOrder {
	case "":
	case "ASCENDING", "DESCENDING":
		q.Set("sortOrder", in.SortOrder)
	default:
		return nil, out, fmt.Errorf("%w: sort_order must be ASCENDING or DESCENDING", kit.ErrInvalidInput)
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("api", "v1", "logs"), Query: q}, &out.Events); err != nil {
		return nil, out, fmt.Errorf("list log events: %w", err)
	}
	out.Count = len(out.Events)
	return nil, out, nil
}

func limitOf(n, max int) (int, error) {
	if n == 0 {
		return defaultLimit, nil
	}
	if err := kit.CheckRange("limit", n, 1, max); err != nil {
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

type sessionInput struct {
	SessionID string `json:"session_id" jsonschema:"Okta session id, from authenticationContext.externalSessionId of a log event"`
}

type sessionInfo struct {
	ID                       string   `json:"id"`
	UserID                   string   `json:"userId"`
	Login                    string   `json:"login,omitempty"`
	Status                   string   `json:"status"`
	CreatedAt                string   `json:"createdAt,omitempty"`
	ExpiresAt                string   `json:"expiresAt,omitempty"`
	LastPasswordVerification string   `json:"lastPasswordVerification,omitempty"`
	LastFactorVerification   string   `json:"lastFactorVerification,omitempty"`
	Amr                      []string `json:"amr,omitempty"`
}

func (v *service) getSession(ctx context.Context, _ *mcp.CallToolRequest, in sessionInput) (*mcp.CallToolResult, sessionInfo, error) {
	var out sessionInfo
	if err := kit.CheckString("session_id", in.SessionID, 64, sessionIDFormat); err != nil {
		return nil, out, err
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("api", "v1", "sessions", in.SessionID)}, &out); err != nil {
		return nil, out, fmt.Errorf("get session: %w", err)
	}
	return nil, out, nil
}
