package mgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// Identity paths.
//
// Verified 2026-11-06 on a live v4: users (9 records), groups (6), roles (7,
// served from the v4 RBAC view), orgs (2) and the current-user profile all
// answered 200. Two guesses did NOT hold and are recorded here so nobody
// re-adds them: /api/v1/users/roles/ and /api/v1/orgs/org-members/ both
// answered 404 on this build.
const (
	PathUsers   = "/api/v1/users/users/"
	PathGroups  = "/api/v1/users/groups/"
	PathRoles   = "/api/v1/rbac/roles/"
	PathOrgs    = "/api/v1/orgs/orgs/"
	PathProfile = "/api/v1/users/profile/"
)

// Identity kinds.
const (
	IdentUsers      = "users"
	IdentGroups     = "groups"
	IdentRoles      = "roles"
	IdentOrgs       = "orgs"
	IdentOrgMembers = "org_members"
)

// IdentityKinds lists the accepted kind values.
func IdentityKinds() []string {
	return []string{IdentUsers, IdentGroups, IdentRoles, IdentOrgs, IdentOrgMembers}
}

// NamedID is the (id, name) pair most identity rows need.
type NamedID struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// Label is the best human-readable name the record has.
//
// Roles arrive as {id, display_name} while groups arrive as {id, name}; a
// single field would leave half the columns blank for no reason.
func (n NamedID) Label() string {
	if n.Name != "" {
		return n.Name
	}
	return n.DisplayName
}

// User is one bastion account.
//
// Deliberately absent: anything that authenticates. The v4 user view exposes
// no password, but it does expose MFA binding state and API-key metadata, so
// the fields are listed explicitly rather than decoded wholesale.
type User struct {
	ID             string     `json:"id"`
	Username       string     `json:"username"`
	Name           string     `json:"name"`
	Email          string     `json:"email,omitempty"`
	Phone          Phone      `json:"phone,omitempty"`
	IsActive       bool       `json:"is_active"`
	IsValid        bool       `json:"is_valid"`
	IsSuperuser    bool       `json:"is_superuser"`
	IsServiceAcct  bool       `json:"is_service_account"`
	Source         FlexString `json:"source,omitempty"`
	DateExpired    string     `json:"date_expired,omitempty"`
	LastLogin      string     `json:"last_login,omitempty"`
	MfaLevel       FlexString `json:"mfa_level,omitempty"`
	LoginBlocked   bool       `json:"login_blocked,omitempty"`
	SystemRoles    []NamedID  `json:"system_roles,omitempty"`
	Groups         []NamedID  `json:"groups,omitempty"`
	PasswordUpdate string     `json:"date_password_last_updated,omitempty"`
}

// Phone is a user's phone number.
//
// The v4 view returns {"code":"+86","phone":"..."} while a build with the
// field disabled returns a plain string, so both are accepted. Rendering it
// as a structured value keeps the country code visible instead of dropping
// it into a display-only string.
type Phone struct {
	Code   string `json:"code,omitempty"`
	Number string `json:"number,omitempty"`
}

// UnmarshalJSON accepts a string or a {"code","phone"} object.
func (p *Phone) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		p.Number = s
		return nil
	}
	var obj struct {
		Code  string `json:"code"`
		Phone string `json:"phone"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil
	}
	p.Code, p.Number = obj.Code, obj.Phone
	return nil
}

// String renders the number with its country code when one is set.
func (p Phone) String() string {
	if p.Code == "" {
		return p.Number
	}
	if p.Number == "" {
		return ""
	}
	return p.Code + " " + p.Number
}

// Group is one user group.
type Group struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Comment    string `json:"comment,omitempty"`
	UsersCount int    `json:"users_amount,omitempty"`
	OrgName    string `json:"org_name,omitempty"`
}

// Role is one v4 RBAC role.
type Role struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	DisplayName string     `json:"display_name,omitempty"`
	Scope       FlexString `json:"scope,omitempty"`
	Builtin     bool       `json:"builtin,omitempty"`
	UsersCount  int        `json:"users_amount,omitempty"`
}

// Org is one organization.
type Org struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default,omitempty"`
	IsRoot    bool   `json:"is_root,omitempty"`
}

// IdentityQuery is one identity search.
type IdentityQuery struct {
	Kind string
	// Org narrows org_members (and users) to one organization.
	Org    string
	Active string
	Limit  int
	Offset int
}

// ListIdentity returns one page of the requested identity kind.
func ListIdentity(ctx context.Context, c *api.Client, q IdentityQuery) (Page[any], error) {
	kind := strings.ToLower(strings.TrimSpace(q.Kind))
	switch kind {
	case IdentUsers, IdentOrgMembers:
		filters := url.Values{}
		// users?org=<id> is how organization membership is served on v4:
		// verified, 9 users for the default org. There is no separate
		// org-members view on this build.
		if org := strings.TrimSpace(q.Org); org != "" {
			filters.Set("org", org)
		}
		page, err := listPage[User](ctx, c, PathUsers, ListOptions{Limit: q.Limit, Offset: q.Offset, Filters: filters})
		if err != nil {
			return Page[any]{}, err
		}
		return toAny(page), nil
	case IdentGroups:
		page, err := listPage[Group](ctx, c, PathGroups, ListOptions{Limit: q.Limit, Offset: q.Offset})
		if err != nil {
			return Page[any]{}, err
		}
		return toAny(page), nil
	case IdentRoles:
		page, err := listPage[Role](ctx, c, PathRoles, ListOptions{Limit: q.Limit, Offset: q.Offset})
		if err != nil {
			return Page[any]{}, err
		}
		return toAny(page), nil
	case IdentOrgs:
		page, err := listPage[Org](ctx, c, PathOrgs, ListOptions{Limit: q.Limit, Offset: q.Offset})
		if err != nil {
			return Page[any]{}, err
		}
		return toAny(page), nil
	default:
		return Page[any]{}, fmt.Errorf("unknown identity kind %q (use one of %s)",
			q.Kind, strings.Join(IdentityKinds(), ", "))
	}
}

// toAny re-types a page so one tool can return four kinds.
func toAny[T any](p Page[T]) Page[any] {
	out := make([]any, 0, len(p.Results))
	for _, row := range p.Results {
		out = append(out, row)
	}
	return Page[any]{Count: p.Count, Results: out}
}

// GetIdentity returns one record of the requested kind.
func GetIdentity(ctx context.Context, c *api.Client, kind, idOrName string) (any, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	slug := strings.Trim(strings.TrimSpace(idOrName), "/")
	if slug == "" {
		return nil, fmt.Errorf("an id or exact name is required")
	}
	switch kind {
	case IdentUsers, IdentOrgMembers:
		var out User
		if err := getByIDOrName(ctx, c, PathUsers, slug, &out); err != nil {
			return nil, err
		}
		return out, nil
	case IdentGroups:
		var out Group
		if err := getByIDOrName(ctx, c, PathGroups, slug, &out); err != nil {
			return nil, err
		}
		return out, nil
	case IdentRoles:
		var out Role
		if err := getByIDOrName(ctx, c, PathRoles, slug, &out); err != nil {
			return nil, err
		}
		return out, nil
	case IdentOrgs:
		var out Org
		if err := getByIDOrName(ctx, c, PathOrgs, slug, &out); err != nil {
			return nil, err
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown identity kind %q (use one of %s)",
			kind, strings.Join(IdentityKinds(), ", "))
	}
}

// getByIDOrName fetches path+slug/, falling back to a name search.
//
// JumpServer addresses most records by UUID, but a person reading a report
// usually has the name. The fallback costs one extra request only when the
// id lookup 404s, so the common case stays one request.
func getByIDOrName[T any](ctx context.Context, c *api.Client, path, slug string, out *T) error {
	err := c.Get(ctx, path+slug+"/", nil, out)
	if err == nil {
		return nil
	}
	if apiErr, ok := api.AsAPIError(err); !ok || apiErr.StatusCode != 404 {
		return err
	}
	// Not an id: search by name and take an exact match only, so "admin"
	// never silently resolves to "admin-2".
	page, listErr := listPage[T](ctx, c, path, ListOptions{Limit: MaxLimit, Filters: url.Values{"search": []string{slug}}})
	if listErr != nil {
		return err
	}
	var matches []T
	for _, row := range page.Results {
		if matchesName(row, slug) {
			matches = append(matches, row)
		}
	}
	switch len(matches) {
	case 0:
		return err
	case 1:
		*out = matches[0]
		return nil
	default:
		return fmt.Errorf("%q matches %d records; use the id instead", slug, len(matches))
	}
}

// matchesName reports whether a decoded record carries name == slug.
//
// It re-marshals around the type switch because the caller is generic over
// four unrelated structs; the cost is one small allocation per candidate on
// a path that only runs on an id-lookup miss.
func matchesName[T any](row T, slug string) bool {
	switch v := any(row).(type) {
	case User:
		return v.Username == slug || v.Name == slug
	case Group:
		return v.Name == slug
	case Role:
		return v.Name == slug || v.DisplayName == slug
	case Org:
		return v.Name == slug
	default:
		return false
	}
}
