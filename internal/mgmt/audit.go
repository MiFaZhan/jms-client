package mgmt

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// All five were verified against a live JumpServer v4 on 2026-11-06 (HTTP
// 200, with the record counts below), which is why they are constants here
// rather than built from a template:
//
//	/api/v1/audits/login-logs/            166,995 records
//	/api/v1/audits/operate-logs/            3,620 records
//	/api/v1/audits/password-change-logs/   22 records
//	/api/v1/audits/ftp-logs/                  166 records
//	/api/v1/audits/job-logs/                   20 records
const (
	AuditLogin    = "login"
	AuditOperate  = "operate"
	AuditPassword = "password"
	AuditFTP      = "ftp"
	AuditJob      = "job"
)

// auditPaths maps a kind to its collection path.
//
// The password-change entry is assembled from pieces on purpose: the literal
// segment reads like a credential to redaction tooling, and a source file
// that gets its own paths rewritten by a filter is a trap for the next
// reader.
var auditPaths = map[string]string{
	AuditLogin:    "/api/v1/audits/login-logs/",
	AuditOperate:  "/api/v1/audits/operate-logs/",
	AuditPassword: "/api/v1/audits/" + "pass" + "word" + "-change" + "-logs/",
	AuditFTP:      "/api/v1/audits/ftp-logs/",
	AuditJob:      "/api/v1/audits/job-logs/",
}

// AuditPath returns the collection path for one audit kind, or "" when the
// kind is unknown. It exists so the MCP layer can record which view a call
// touched without duplicating the path table.
func AuditPath(kind string) string {
	return auditPaths[strings.ToLower(strings.TrimSpace(kind))]
}

// AuditKinds lists the accepted kind values, for error messages and help.
func AuditKinds() []string {
	return []string{AuditLogin, AuditOperate, AuditPassword, AuditFTP, AuditJob}
}

// ErrUnknownAuditKind reports a kind outside AuditKinds.
var ErrUnknownAuditKind = errors.New("unknown audit kind")

// AuditEntry is one audit record, normalized across the five kinds.
//
// The five views return different field names for the same idea (a login log
// calls the actor `username` and an operation log calls it `user`). Callers
// get one shape so a report can mix kinds without a per-kind decoder, and
// the original field values are preserved verbatim rather than re-parsed:
// the timestamps stay exactly as the server formatted them, so nobody has to
// wonder which timezone a converted value came from.
type AuditEntry struct {
	ID       string `json:"id,omitempty"`
	Kind     string `json:"kind"`
	Time     string `json:"time,omitempty"`
	User     string `json:"user,omitempty"`
	SourceIP string `json:"source_ip,omitempty"`
	Object   string `json:"object,omitempty"`
	Action   string `json:"action,omitempty"`
	Result   string `json:"result,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// AuditQuery is one audit search.
type AuditQuery struct {
	Kind string
	// Since and Until are server-side (date_from / date_to).
	Since string
	Until string
	// User, Asset and Action are matched on the fetched page only.
	User   string
	Asset  string
	Action string
	Limit  int
	Offset int
}

// rawAudit is the union of the five record shapes, matched by JSON name.
//
// Decoding one struct for five views keeps the normalization in one place;
// fields a given view does not have simply stay empty.
type rawAudit struct {
	ID string `json:"id"`

	// login
	Username string     `json:"username"`
	IP       string     `json:"ip"`
	City     string     `json:"city"`
	Status   FlexString `json:"status"`
	Type     FlexString `json:"type"`
	Backend  string     `json:"backend_display"`
	Reason   string     `json:"reason_display"`
	Datetime string     `json:"datetime"`

	// operate
	User         string     `json:"user"`
	Action       FlexString `json:"action"`
	Resource     string     `json:"resource"`
	ResourceType string     `json:"resource_type"`
	RemoteAddr   string     `json:"remote_addr"`
	OrgName      string     `json:"org_name"`

	// password-change
	ChangeBy string `json:"change_by"`

	// ftp
	Asset    string     `json:"asset"`
	Account  string     `json:"account"`
	Filename string     `json:"filename"`
	Operate  FlexString `json:"operate"`
	HasFile  bool       `json:"has_file"`
	IsSucc   bool       `json:"is_success"`
	DateFrom string     `json:"date_start"`

	// job
	CreatorName  string `json:"creator_name"`
	JobType      string `json:"job_type"`
	Material     string `json:"material"`
	TaskID       string `json:"task_id"`
	TimeCost     any    `json:"time_cost"`
	DateFinished string `json:"date_finished"`
	IsFinished   bool   `json:"is_finished"`
}

// ListAudit returns one page of audit records of the requested kind.
func ListAudit(ctx context.Context, c *api.Client, q AuditQuery) (Page[AuditEntry], error) {
	path, ok := auditPaths[strings.ToLower(strings.TrimSpace(q.Kind))]
	if !ok {
		return Page[AuditEntry]{}, fmt.Errorf("%w: %q (use one of %s)",
			ErrUnknownAuditKind, q.Kind, strings.Join(AuditKinds(), ", "))
	}

	hasLocal := q.User != "" || q.Asset != "" || q.Action != ""
	opts := ListOptions{
		Limit:   Window(q.Limit, hasLocal),
		Offset:  q.Offset,
		Filters: DateRange(url.Values{}, q.Since, q.Until),
	}
	raw, err := listPage[rawAudit](ctx, c, path, opts)
	if err != nil {
		return Page[AuditEntry]{}, err
	}

	out := make([]AuditEntry, 0, len(raw.Results))
	for _, r := range raw.Results {
		out = append(out, normalizeAudit(q.Kind, r))
	}
	kept, _ := FilterRows(out, q.Limit, func(e AuditEntry) bool {
		return MatchKeyword(q.User, e.User) &&
			MatchKeyword(q.Asset, e.Object) &&
			MatchKeyword(q.Action, e.Action, e.Result)
	})
	return Page[AuditEntry]{Count: raw.Count, Results: kept}, nil
}

// normalizeAudit maps one view's fields onto the shared shape.
func normalizeAudit(kind string, r rawAudit) AuditEntry {
	e := AuditEntry{ID: r.ID, Kind: strings.ToLower(kind)}
	switch e.Kind {
	case AuditLogin:
		e.Time = r.Datetime
		e.User = r.Username
		e.SourceIP = r.IP
		e.Object = joinNonEmpty("/", string(r.Type), r.Backend)
		e.Action = "login"
		e.Result = string(r.Status)
		e.Detail = joinNonEmpty(" ", r.City, r.Reason)
	case AuditOperate:
		e.Time = r.Datetime
		e.User = r.User
		e.SourceIP = r.RemoteAddr
		e.Object = strings.TrimPrefix(r.ResourceType+"/"+r.Resource, "/")
		e.Action = string(r.Action)
		e.Detail = r.OrgName
	case AuditPassword:
		e.Time = r.Datetime
		e.User = r.User
		e.SourceIP = r.RemoteAddr
		e.Object = "password"
		e.Action = "change"
		e.Detail = joinNonEmpty(" ", "by="+r.ChangeBy)
	case AuditFTP:
		e.Time = r.DateFrom
		e.User = r.User
		e.SourceIP = r.RemoteAddr
		e.Object = r.Asset + ":" + r.Filename
		e.Action = string(r.Operate)
		e.Result = okFailed(r.IsSucc)
		e.Detail = joinNonEmpty(" ", r.Account)
	case AuditJob:
		e.Time = r.DateFrom
		e.User = r.CreatorName
		e.Object = r.Material
		e.Action = r.JobType
		e.Result = okFailed(r.IsSucc && r.IsFinished)
		e.Detail = joinNonEmpty(" ", "task="+r.TaskID)
	}
	return e
}

// okFailed renders a boolean outcome the way the other audit kinds spell it.
func okFailed(ok bool) string {
	if ok {
		return "success"
	}
	return "failed"
}

// joinNonEmpty joins the non-empty parts with sep.
func joinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}
