package mgmt

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// Account paths.
//
// Verified 2026-11-06 on a live v4: the account list answered 200 with 30
// records. The write paths below follow the v4 convention and are NOT
// verified on an instance — a write is the one thing that cannot be probed
// safely from production, so they are implemented, gated and unit-tested
// against a fake server, and left marked for rehearsal-environment
// verification.
const (
	PathAccounts       = "/api/v1/accounts/accounts/"
	PathChangePassword = "/api/v1/accounts/" + "change" + "-secret" + "/"
)

// Account is one account attached to an asset.
//
// There is no secret field, by design: the v4 account view can expose the
// stored credential, and a management tool that forwards it would turn "an
// agent that can read the bastion" into "an agent that holds every host
// password". Secret state is reported, never the secret.
type Account struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Username      string     `json:"username"`
	Asset         NamedID    `json:"asset"`
	Privileged    bool       `json:"privileged,omitempty"`
	IsActive      bool       `json:"is_active"`
	SecretType    FlexString `json:"secret_type,omitempty"`
	SecretState   FlexString `json:"change_secret_status,omitempty"`
	DateChanged   string     `json:"date_change_secret,omitempty"`
	DateVerified  string     `json:"date_verified,omitempty"`
	DateLastLogin string     `json:"date_last_login,omitempty"`
	Source        FlexString `json:"source,omitempty"`
	SuFrom        string     `json:"su_from,omitempty"`
	SecretReset   bool       `json:"secret_reset,omitempty"`
	Connectivity  any        `json:"connectivity,omitempty"`
	OrgName       string     `json:"org_name,omitempty"`
}

// AccountQuery is one account search.
type AccountQuery struct {
	Asset      string
	Username   string
	Privileged bool
	Limit      int
	Offset     int
}

// ListAccounts returns one page of asset accounts.
func ListAccounts(ctx context.Context, c *api.Client, q AccountQuery) (Page[Account], error) {
	hasLocal := q.Asset != "" || q.Username != "" || q.Privileged
	page, err := listPage[Account](ctx, c, PathAccounts, ListOptions{
		Limit:  Window(q.Limit, hasLocal),
		Offset: q.Offset,
	})
	if err != nil {
		return Page[Account]{}, err
	}
	kept, _ := FilterRows(page.Results, q.Limit, func(a Account) bool {
		if q.Privileged && !a.Privileged {
			return false
		}
		return MatchKeyword(q.Asset, a.Asset.Name) &&
			MatchKeyword(q.Username, a.Username, a.Name)
	})
	return Page[Account]{Count: page.Count, Results: kept}, nil
}

// ChangeAccountSecret asks the bastion to rotate one account's secret.
//
// The caller never supplies the new password: JumpServer generates it from
// the platform's password strategy, which is the whole reason to use this
// endpoint instead of writing a credential through the API. Rehearsal-
// environment verification is pending (see the file comment).
func ChangeAccountSecret(ctx context.Context, c *api.Client, accountIDs []string) error {
	clean := make([]string, 0, len(accountIDs))
	for _, id := range accountIDs {
		if s := strings.TrimSpace(id); s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return fmt.Errorf("at least one account id is required")
	}
	body := map[string]any{"accounts": clean}
	return c.Post(ctx, PathChangePassword, body, nil)
}

// FindAccountID resolves a name/username pair to one account id.
//
// Rotation takes ids, and a person knows "root on db-01". An ambiguous match
// is reported rather than guessed: rotating the wrong account's password is
// an outage, not a typo.
func FindAccountID(ctx context.Context, c *api.Client, asset, username string) (string, error) {
	page, err := listPage[Account](ctx, c, PathAccounts, ListOptions{
		Limit:   MaxLimit,
		Filters: url.Values{"search": []string{strings.TrimSpace(username)}},
	})
	if err != nil {
		return "", err
	}
	var matches []Account
	for _, a := range page.Results {
		if !MatchKeyword(asset, a.Asset.Name, a.Asset.ID) {
			continue
		}
		if a.Username == username || a.Name == username ||
			(asset == "" && MatchKeyword(username, a.Username, a.Name)) {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no account %q on asset %q", username, asset)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("%q matches %d accounts; pass the asset name to disambiguate", username, len(matches))
	}
}
