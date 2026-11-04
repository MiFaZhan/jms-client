// Package permits implements JumpServer asset authorization (AssetPermission).
//
// An asset that exists is not yet reachable: JumpServer only exposes an
// asset to a user through an AssetPermission record, which binds a set of
// users (or user groups) to a set of assets (or nodes) together with the
// allowed accounts, protocols and actions. Creating an asset therefore
// takes two steps — the asset, then the permission — and `jms ls` only
// shows assets that the second step has granted.
//
// Endpoint facts, verified against the JumpServer source
// (apps/perms/urls/asset_permission.py registers the AssetPermissionViewSet
// on the BulkRouter under "asset-permissions") and against the running
// instance on 2026-11-04:
//
//	POST   /api/v1/perms/asset-permissions/        create a permission
//	GET    /api/v1/perms/asset-permissions/        list permissions
//	DELETE /api/v1/perms/asset-permissions/<id>/   revoke
//
// The read-only user view (/api/v1/perms/users/<user>/assets/) answers
// "what may this user reach" and implements no write method at all, so it
// cannot be used to grant anything.
//
// Dependency direction: permits depends on api.
package permits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// PathPermissions is the AssetPermission collection.
const PathPermissions = "/api/v1/perms/asset-permissions/"

// AccountAll grants every account that the asset holds. JumpServer also
// accepts "@SPEC", "@USER", "@ANON", "@INPUT" and explicit account names.
const AccountAll = "@ALL"

// ProtocolAll grants every protocol the asset offers.
const ProtocolAll = "all"

// Action is one entry of the `actions` field.
//
// The API returns it as an object ({"label":..,"value":"connect"}) in a
// read but accepts a bare string in a write, so both shapes have to be
// understood.
type Action struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// UnmarshalJSON accepts either a bare string or the labelled object the
// server returns.
func (a *Action) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		a.Value = s
		return nil
	}
	type alias Action
	var obj alias
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*a = Action(obj)
	return nil
}

// MarshalJSON writes the bare value, which is what a write request wants.
func (a Action) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.Value)
}

// Permission is one authorization rule.
type Permission struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Users []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"users"`
	UserGroups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"user_groups"`
	Assets []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"assets"`
	Nodes []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"nodes"`
	Accounts  []string `json:"accounts"`
	Protocols []string `json:"protocols"`
	Actions   []Action `json:"actions"`
	IsActive  bool     `json:"is_active"`
	Comment   string   `json:"comment"`
	DateStart string   `json:"date_start"`
}

// GrantSpec describes one permission to create.
type GrantSpec struct {
	// Name is the permission's display name; it must be unique.
	Name string
	// UserIDs are the users the grant applies to.
	UserIDs []string
	// AssetIDs are the assets being granted.
	AssetIDs []string
	// Accounts selects which accounts are usable; empty means AccountAll.
	Accounts []string
	// Protocols selects which protocols are usable; empty means ProtocolAll.
	Protocols []string
	// Actions are the allowed operations; empty means a plain connect.
	Actions []string
	// Comment is the free-text note.
	Comment string
}

// grantPayload is the request body the AssetPermission serializer accepts.
//
// The serializer declares users, assets, nodes, accounts, protocols and
// actions as optional, but a permission with no user and no asset grants
// nothing, so the caller is required to supply both.
type grantPayload struct {
	Name      string   `json:"name"`
	Users     []string `json:"users"`
	Assets    []string `json:"assets"`
	Accounts  []string `json:"accounts"`
	Protocols []string `json:"protocols"`
	Actions   []string `json:"actions"`
	Comment   string   `json:"comment"`
	IsActive  bool     `json:"is_active"`
}

// DefaultActions is what a permission grants when GrantSpec.Actions is
// empty: connect, which covers opening a session in any protocol.
func DefaultActions() []string {
	return []string{"connect"}
}

// Grant creates an AssetPermission.
//
// This is the step that makes an asset appear in `jms ls`. Creating an
// asset without granting it leaves it visible only to the administrator
// view (`jms asset list`).
func Grant(ctx context.Context, c *api.Client, spec GrantSpec) (Permission, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return Permission{}, errors.New("permission name is required")
	}
	if len(spec.UserIDs) == 0 {
		return Permission{}, errors.New("at least one user is required")
	}
	if len(spec.AssetIDs) == 0 {
		return Permission{}, errors.New("at least one asset is required")
	}

	accounts := spec.Accounts
	if len(accounts) == 0 {
		accounts = []string{AccountAll}
	}
	protocols := spec.Protocols
	if len(protocols) == 0 {
		protocols = []string{ProtocolAll}
	}
	actions := spec.Actions
	if len(actions) == 0 {
		actions = DefaultActions()
	}

	payload := grantPayload{
		Name:      spec.Name,
		Users:     spec.UserIDs,
		Assets:    spec.AssetIDs,
		Accounts:  accounts,
		Protocols: protocols,
		Actions:   actions,
		Comment:   spec.Comment,
		IsActive:  true,
	}

	var created Permission
	if err := c.Post(ctx, PathPermissions, payload, &created); err != nil {
		return Permission{}, err
	}
	return created, nil
}

// List returns every permission rule.
func List(ctx context.Context, c *api.Client) ([]Permission, error) {
	items, err := c.GetAll(ctx, PathPermissions, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Permission, 0, len(items))
	for _, raw := range items {
		var p Permission
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// FindByAsset returns the permissions that grant the given asset.
//
// The list endpoint reports only the relation *counts* (assets_amount,
// users_amount), not the related objects: users and assets come back as
// null. The relations are expanded only by the detail endpoint, so each
// candidate has to be read individually before its assets can be
// inspected.
func FindByAsset(ctx context.Context, c *api.Client, assetID string) ([]Permission, error) {
	list, err := List(ctx, c)
	if err != nil {
		return nil, err
	}
	out := make([]Permission, 0, len(list))
	for _, summary := range list {
		if len(summary.Assets) == 0 {
			// The summary carries no assets; read the detail.
			detail, err := Get(ctx, c, summary.ID)
			if err != nil {
				continue
			}
			summary = detail
		}
		for _, a := range summary.Assets {
			if a.ID == assetID {
				out = append(out, summary)
				break
			}
		}
	}
	return out, nil
}

// Get returns one permission with its relations expanded.
func Get(ctx context.Context, c *api.Client, id string) (Permission, error) {
	var p Permission
	if err := c.Get(ctx, PathPermissions+id+"/", nil, &p); err != nil {
		return Permission{}, err
	}
	return p, nil
}

// Revoke deletes a permission by id.
func Revoke(ctx context.Context, c *api.Client, id string) error {
	if err := c.Delete(ctx, PathPermissions+id+"/", nil); err != nil {
		return fmt.Errorf("revoke permission %s: %w", id, err)
	}
	return nil
}

// CurrentUser returns the id of the authenticated user.
//
// It reads /api/v1/users/profile/, which the login already established;
// the id is what a permission's users array needs.
func CurrentUser(ctx context.Context, c *api.Client) (string, string, error) {
	var profile struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if err := c.Get(ctx, "/api/v1/users/profile/", nil, &profile); err != nil {
		return "", "", err
	}
	if profile.ID == "" {
		return "", "", errors.New("profile response carried no user id")
	}
	name := profile.Name
	if name == "" {
		name = profile.Username
	}
	return profile.ID, name, nil
}
