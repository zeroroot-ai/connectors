// Package ticket holds the tool contract of the ticket servers (class
// ticket): the tool names, the argument names and the result fields. The
// servers for ServiceNow and Jira use it, so the ticket mirror of the
// intake agent needs one adapter shape. A server maps the contract to its
// vendor API, and it adds vendor tools only when the contract has no tool
// for the action. PagerDuty (class paging) has its own tools. See the
// README of the repo, section "Ticket contract".
package ticket

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tool names of the contract.
const (
	ToolCreate  = "create_ticket"
	ToolUpdate  = "update_ticket"
	ToolComment = "add_comment"
	ToolClose   = "close_ticket"
	ToolGet     = "get_ticket"
	ToolSearch  = "search_tickets"
)

// The priorities of the contract. A server maps each one to a value of its
// vendor.
const (
	Low      = "low"
	Medium   = "medium"
	High     = "high"
	Critical = "critical"
)

// The statuses of the search filter.
const (
	StatusOpen   = "open"
	StatusClosed = "closed"
	StatusAny    = "any"
)

// Limits. A model gives the input, so each field has a limit.
const (
	MaxTitle       = 255
	MaxDescription = 32000
	MaxComment     = 32000
	MaxQueue       = 64
	MaxText        = 200
	MaxLabel       = 50
	MaxLabels      = 10
	MaxLimit       = 50
	DefaultLimit   = 20
)

var labelFormat = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,50}$`)

// CreateInput is the input of create_ticket.
type CreateInput struct {
	Title       string   `json:"title" jsonschema:"one line that names the problem, at most 255 characters"`
	Description string   `json:"description" jsonschema:"the text of the ticket"`
	Queue       string   `json:"queue,omitempty" jsonschema:"where the ticket goes: the project key on Jira, the id of the assignment group on ServiceNow"`
	Priority    string   `json:"priority,omitempty" jsonschema:"low, medium, high or critical. The default is medium."`
	Labels      []string `json:"labels,omitempty" jsonschema:"up to 10 labels of letters, digits and . _ : -"`
}

// UpdateInput is the input of update_ticket. Give at least one of title,
// description and priority.
type UpdateInput struct {
	TicketID    string `json:"ticket_id" jsonschema:"the ticket_id that create_ticket or search_tickets returned"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Priority    string `json:"priority,omitempty" jsonschema:"low, medium, high or critical"`
}

// CommentInput is the input of add_comment.
type CommentInput struct {
	TicketID string `json:"ticket_id"`
	Body     string `json:"body" jsonschema:"the text of the comment"`
}

// CloseInput is the input of close_ticket.
type CloseInput struct {
	TicketID   string `json:"ticket_id"`
	Resolution string `json:"resolution" jsonschema:"what was done, as a short text. The server records it on the ticket."`
}

// GetInput is the input of get_ticket.
type GetInput struct {
	TicketID string `json:"ticket_id"`
}

// SearchInput is the input of search_tickets. Each field is a typed filter.
// A server builds its query from these fields, and never from free text that
// it passes on as query syntax.
type SearchInput struct {
	Queue         string `json:"queue,omitempty"`
	Status        string `json:"status,omitempty" jsonschema:"open, closed or any. The default is open."`
	TitleContains string `json:"title_contains,omitempty" jsonschema:"text that the title holds, at most 200 characters"`
	Label         string `json:"label,omitempty"`
	CreatedAfter  string `json:"created_after,omitempty" jsonschema:"RFC 3339 time"`
	Limit         int    `json:"limit,omitempty" jsonschema:"number of tickets, 1 to 50. The default is 20."`
}

// Ticket is the result of get_ticket and the item of search_tickets.
type Ticket struct {
	TicketID    string `json:"ticket_id"`
	Number      string `json:"number,omitempty"`
	URL         string `json:"url,omitempty"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	Priority    string `json:"priority,omitempty"`
	Closed      bool   `json:"closed"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// Created is the result of create_ticket.
type Created struct {
	TicketID string `json:"ticket_id"`
	Number   string `json:"number,omitempty"`
	URL      string `json:"url,omitempty"`
}

// Changed is the result of update_ticket, add_comment and close_ticket.
type Changed struct {
	TicketID string `json:"ticket_id"`
	Action   string `json:"action"`
	Status   string `json:"status,omitempty"`
}

// SearchOutput is the result of search_tickets.
type SearchOutput struct {
	Tickets []Ticket `json:"tickets"`
	Count   int      `json:"count"`
}

// CheckPriority refuses a priority that is not in the contract. An empty
// value is allowed and means no value.
func CheckPriority(p string) error {
	switch p {
	case "", Low, Medium, High, Critical:
		return nil
	}
	return fmt.Errorf("%w: priority must be low, medium, high or critical", kit.ErrInvalidInput)
}

// CheckCreate checks the fields that do not depend on the vendor. The server
// checks queue with the format of its vendor.
func CheckCreate(in CreateInput) error {
	if err := kit.CheckString("title", in.Title, MaxTitle, nil); err != nil {
		return fmt.Errorf("check title: %w", err)
	}
	if err := checkLong("description", in.Description, MaxDescription, true); err != nil {
		return err
	}
	if err := CheckPriority(in.Priority); err != nil {
		return err
	}
	if len(in.Labels) > MaxLabels {
		return fmt.Errorf("%w: more than %d labels", kit.ErrInvalidInput, MaxLabels)
	}
	for _, l := range in.Labels {
		if !labelFormat.MatchString(l) {
			return fmt.Errorf("%w: a label has the wrong format", kit.ErrInvalidInput)
		}
	}
	return nil
}

// CheckUpdate checks the fields of update_ticket. A server checks ticket_id.
func CheckUpdate(in UpdateInput) error {
	if in.Title == "" && in.Description == "" && in.Priority == "" {
		return fmt.Errorf("%w: give title, description or priority", kit.ErrInvalidInput)
	}
	if in.Title != "" {
		if err := kit.CheckString("title", in.Title, MaxTitle, nil); err != nil {
			return fmt.Errorf("check title: %w", err)
		}
	}
	if in.Description != "" {
		if err := checkLong("description", in.Description, MaxDescription, true); err != nil {
			return err
		}
	}
	return CheckPriority(in.Priority)
}

// CheckText checks a long text, for example a comment or a resolution. It
// allows new lines and tabs, and refuses other control characters.
func CheckText(field, v string, limit int) error {
	return checkLong(field, v, limit, true)
}

func checkLong(field, v string, limit int, required bool) error {
	switch {
	case required && strings.TrimSpace(v) == "":
		return fmt.Errorf("%w: %s is required", kit.ErrInvalidInput, field)
	case len(v) > limit:
		return fmt.Errorf("%w: %s is longer than %d bytes", kit.ErrInvalidInput, field, limit)
	}
	for _, r := range v {
		if (r < 0x20 && r != '\n' && r != '\t' && r != '\r') || r == 0x7f {
			return fmt.Errorf("%w: %s holds a control character", kit.ErrInvalidInput, field)
		}
	}
	return nil
}

// CheckSearch checks the filters of search_tickets and returns the limit
// with its default. A server checks queue and the text with its own rules.
func CheckSearch(in SearchInput) (limit int, err error) {
	switch in.Status {
	case "", StatusOpen, StatusClosed, StatusAny:
	default:
		return 0, fmt.Errorf("%w: status must be open, closed or any", kit.ErrInvalidInput)
	}
	if in.TitleContains != "" {
		if err := kit.CheckString("title_contains", in.TitleContains, MaxText, nil); err != nil {
			return 0, fmt.Errorf("check title_contains: %w", err)
		}
	}
	if in.Label != "" && !labelFormat.MatchString(in.Label) {
		return 0, fmt.Errorf("%w: label has the wrong format", kit.ErrInvalidInput)
	}
	if in.CreatedAfter != "" {
		if _, perr := time.Parse(time.RFC3339, in.CreatedAfter); perr != nil {
			return 0, fmt.Errorf("%w: created_after is not an RFC 3339 time", kit.ErrInvalidInput)
		}
	}
	limit = in.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	if err := kit.CheckRange("limit", limit, 1, MaxLimit); err != nil {
		return 0, fmt.Errorf("check limit: %w", err)
	}
	return limit, nil
}

// StatusOf returns the status filter with its default.
func StatusOf(in SearchInput) string {
	if in.Status == "" {
		return StatusOpen
	}
	return in.Status
}
