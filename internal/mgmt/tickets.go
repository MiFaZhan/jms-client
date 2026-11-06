package mgmt

import (
	"context"
	"fmt"
	"strings"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// Ticket paths.
//
// Verified 2026-11-06 on a live v4: the collection answered 200 with zero
// records, so the path is right and the record shape could not be sampled.
// The struct below therefore lists only the fields a ticket necessarily has
// on any v4 build; a missing name yields an empty column, never a decode
// failure.
const (
	PathTickets = "/api/v1/tickets/tickets/"
)

// Ticket is one work order.
type Ticket struct {
	ID          string     `json:"id"`
	SerialNum   string     `json:"serial_num,omitempty"`
	Title       string     `json:"title,omitempty"`
	Type        FlexString `json:"type,omitempty"`
	Status      FlexString `json:"status,omitempty"`
	Applicant   string     `json:"applicant,omitempty"`
	Assignee    string     `json:"assignee,omitempty"`
	DateCreated string     `json:"date_created,omitempty"`
	DateUpdated string     `json:"date_updated,omitempty"`
	Comment     string     `json:"comment,omitempty"`
}

// TicketQuery is one ticket search.
type TicketQuery struct {
	Status    string
	Type      string
	Applicant string
	Limit     int
	Offset    int
}

// ListTickets returns one page of tickets.
//
// Status and type are enum-ish values whose spelling differs between
// builds, so they are matched on the page rather than sent as filters: a
// silent "unknown enum returns everything" would make a filtered report
// look complete.
func ListTickets(ctx context.Context, c *api.Client, q TicketQuery) (Page[Ticket], error) {
	hasLocal := q.Status != "" || q.Type != "" || q.Applicant != ""
	page, err := listPage[Ticket](ctx, c, PathTickets, ListOptions{
		Limit:  Window(q.Limit, hasLocal),
		Offset: q.Offset,
	})
	if err != nil {
		return Page[Ticket]{}, err
	}
	kept, _ := FilterRows(page.Results, q.Limit, func(t Ticket) bool {
		return MatchKeyword(q.Status, string(t.Status)) &&
			MatchKeyword(q.Type, string(t.Type)) &&
			MatchKeyword(q.Applicant, t.Applicant, t.Assignee)
	})
	return Page[Ticket]{Count: page.Count, Results: kept}, nil
}

// GetTicket returns one ticket with its approval trail.
//
// The trail lives in a separate view on v4; it is fetched best-effort,
// because a ticket whose detail is readable but whose flow is not should
// still be answerable.
func GetTicket(ctx context.Context, c *api.Client, id string) (map[string]any, error) {
	slug := strings.Trim(strings.TrimSpace(id), "/")
	if slug == "" {
		return nil, fmt.Errorf("a ticket id is required")
	}
	out := map[string]any{}
	if err := c.Get(ctx, PathTickets+slug+"/", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
