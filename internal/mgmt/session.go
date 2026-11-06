package mgmt

import (
	"context"
	"net/url"
	"strings"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// Terminal session paths.
//
// Verified 2026-11-06 on a live v4: the collection answered HTTP 200 with
// 6,314 records, the detail (/<id>/) answered 200, and the nested
// replay-files view does not exist (HTTP 404) — replay is reported through
// the session's own has_replay / replay_size fields, which is what this
// package exposes.
const (
	PathSessions = "/api/v1/terminal/sessions/"
	PathCommands = "/api/v1/terminal/commands/"
	PathTasks    = "/api/v1/terminal/tasks/"
)

// Session is one terminal session as the server lists it.
//
// Only the fields a reader acts on are kept. The server returns more, and
// forwarding all of it would bury the answer: the point of this tool is
// "who was connected to what, when, and is it still open".
type Session struct {
	ID            string     `json:"id"`
	User          string     `json:"user"`
	Asset         string     `json:"asset"`
	Account       string     `json:"account,omitempty"`
	Protocol      string     `json:"protocol"`
	Type          FlexString `json:"type,omitempty"`
	LoginFrom     FlexString `json:"login_from,omitempty"`
	RemoteAddr    string     `json:"remote_addr,omitempty"`
	DateStart     string     `json:"date_start"`
	DateEnd       string     `json:"date_end,omitempty"`
	Duration      string     `json:"duration,omitempty"`
	IsFinished    bool       `json:"is_finished"`
	IsSuccess     bool       `json:"is_success,omitempty"`
	HasReplay     bool       `json:"has_replay"`
	ReplaySize    int64      `json:"replay_size,omitempty"`
	CommandAmount int        `json:"command_amount,omitempty"`
	CanTerminate  bool       `json:"can_terminate"`
	Terminal      string     `json:"terminal_display,omitempty"`
	ErrorReason   FlexString `json:"error_reason,omitempty"`
}

// SessionQuery is one session search.
type SessionQuery struct {
	Since  string
	Until  string
	User   string
	Asset  string
	Active bool
	Limit  int
	Offset int
}

// ListSessions returns one page of terminal sessions.
func ListSessions(ctx context.Context, c *api.Client, q SessionQuery) (Page[Session], error) {
	hasLocal := q.User != "" || q.Asset != "" || q.Active
	page, err := listPage[Session](ctx, c, PathSessions, ListOptions{
		Limit:   Window(q.Limit, hasLocal),
		Offset:  q.Offset,
		Filters: DateRange(url.Values{}, q.Since, q.Until),
	})
	if err != nil {
		return Page[Session]{}, err
	}
	kept, _ := FilterRows(page.Results, q.Limit, func(s Session) bool {
		if q.Active && s.IsFinished {
			return false
		}
		return MatchKeyword(q.User, s.User) &&
			MatchKeyword(q.Asset, s.Asset)
	})
	return Page[Session]{Count: page.Count, Results: kept}, nil
}

// GetSession returns one session by id.
func GetSession(ctx context.Context, c *api.Client, id string) (Session, error) {
	var out Session
	if err := c.Get(ctx, PathSessions+strings.Trim(strings.TrimSpace(id), "/")+"/", nil, &out); err != nil {
		return Session{}, err
	}
	return out, nil
}

// Command is one recorded command.
//
// The shape below is the one JumpServer has documented for the view; the
// instance this was written against had zero command records, so the field
// names are unverified and are reported as such. It is a read-only view, so
// an unverified field costs an empty column rather than a wrong decision.
type Command struct {
	ID        string `json:"id"`
	User      string `json:"user"`
	Asset     string `json:"asset"`
	Account   string `json:"account,omitempty"`
	Input     string `json:"input,omitempty"`
	Output    string `json:"output,omitempty"`
	RiskLevel string `json:"risk_level,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	SessionID string `json:"session,omitempty"`
}

// CommandQuery is one command search.
type CommandQuery struct {
	Since  string
	Until  string
	User   string
	Asset  string
	Input  string
	Limit  int
	Offset int
}

// ListCommands returns one page of recorded commands.
func ListCommands(ctx context.Context, c *api.Client, q CommandQuery) (Page[Command], error) {
	hasLocal := q.User != "" || q.Asset != "" || q.Input != ""
	page, err := listPage[Command](ctx, c, PathCommands, ListOptions{
		Limit:   Window(q.Limit, hasLocal),
		Offset:  q.Offset,
		Filters: DateRange(url.Values{}, q.Since, q.Until),
	})
	if err != nil {
		return Page[Command]{}, err
	}
	kept, _ := FilterRows(page.Results, q.Limit, func(c Command) bool {
		return MatchKeyword(q.User, c.User) &&
			MatchKeyword(q.Asset, c.Asset) &&
			MatchKeyword(q.Input, c.Input)
	})
	return Page[Command]{Count: page.Count, Results: kept}, nil
}
