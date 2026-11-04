// Package assetsmanage implements the write side of asset management:
// creating, updating, showing and deleting JumpServer assets.
//
// The read side lives in the assets package. This one is kept separate
// because the write paths have different concerns: they need node and
// account ids, they mutate an external system, and their request bodies
// are version-sensitive.
//
// Endpoint facts (verified against the running JumpServer on 2026-11-04):
//
//   - POST   /api/v1/assets/hosts/    creates a host asset
//   - PUT    /api/v1/assets/hosts/<id>/    updates it
//   - DELETE /api/v1/assets/hosts/<id>/    deletes it
//   - GET    /api/v1/assets/hosts/    lists every host asset (admin view)
//
// The plural "/assets/" view answers GET and POST with
// "Action not allowed: POST, only “ supported" — the hosts view is the
// one that implements creation.
//
// Dependency direction: assetsmanage depends on api.
package assetsmanage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// Endpoint paths for the host-asset view.
const (
	// PathHosts is the host-asset collection. POST creates, GET lists.
	PathHosts = "/api/v1/assets/hosts/"
	// PathNodes lists the asset nodes (the "default" node is the one
	// every host asset belongs to).
	PathNodes = "/api/v1/assets/nodes/"
	// PathPlatforms lists the asset platforms; their id goes into a
	// create request as "platform".
	PathPlatforms = "/api/v1/assets/platforms/"
)

// ErrNotFound is returned when no asset matches an id or name.
var ErrNotFound = errors.New("asset not found")

// ErrConflict is returned when the server refuses a create because the
// name is already taken.
var ErrConflict = errors.New("asset name already exists")

// PlatformIDLinux is the id of the built-in Linux platform. JumpServer
// assigns it at install time and it has been stable across the versions
// this client talks to.
const PlatformIDLinux = 1

// Node is one asset node.
type Node struct {
	ID    string `json:"id"`
	Key   string `json:"key"`
	Value string `json:"value"`
	Name  string `json:"name"`
}

// Platform is one asset platform.
type Platform struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Account is an account attached to an asset. Creating a host asset with
// an account is what makes `jms exec` able to authenticate.
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
	Alias    string `json:"alias"`
}

// Asset is a host asset as the server reports it.
type Asset struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Platform struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"platform"`
	Comment  string      `json:"comment"`
	Nodes    []AssetNode `json:"nodes"`
	Accounts []Account   `json:"accounts"`
}

// AssetNode is a node an asset belongs to.
type AssetNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Spec describes the asset to create.
type Spec struct {
	// Name is the asset name. It must be unique on the server.
	Name string
	// Address is the reachable address (IP or hostname).
	Address string
	// PlatformID selects the platform; zero means PlatformIDLinux.
	PlatformID int
	// NodeID is the node the asset belongs to. Empty means the default
	// node is looked up.
	NodeID string
	// Accounts are the accounts to attach. An asset with no account can
	// be listed but not connected to.
	Accounts []AccountSpec
	// Comment is the free-text note.
	Comment string
	// Protocols are the protocols to enable; empty means ssh+sftp.
	Protocols []string
}

// AccountSpec describes an account to attach to a new asset.
type AccountSpec struct {
	// Name is the account display name.
	Name string
	// Username is the login name on the target host.
	Username string
	// Password is the account secret. It is sent in the create request
	// and never stored locally.
	Password string
}

// createPayload is the request body JumpServer accepts for a host asset.
type createPayload struct {
	Name      string        `json:"name"`
	Address   string        `json:"address"`
	Platform  int           `json:"platform"`
	Nodes     []string      `json:"nodes"`
	Comment   string        `json:"comment"`
	Accounts  []accountBody `json:"accounts"`
	Protocols []protocolRow `json:"protocols"`
}

type accountBody struct {
	Name     string `json:"name"`
	Username string `json:"username"`
	Password string `json:"password"`
	Secret   string `json:"secret_type"`
}

type protocolRow struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// DefaultProtocols is what a host asset gets when Spec.Protocols is
// empty: ssh for the session and sftp for file transfer, both on 22.
func DefaultProtocols() []protocolRow {
	return []protocolRow{{Name: "ssh", Port: 22}, {Name: "sftp", Port: 22}}
}

// List returns every host asset visible to the caller.
//
// This is the admin view: it lists all assets, not only the ones the
// caller is permitted to connect to. Use the assets package's List for
// the permission-scoped view.
func List(ctx context.Context, c *api.Client) ([]Asset, error) {
	items, err := c.GetAll(ctx, PathHosts, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Asset, 0, len(items))
	for _, raw := range items {
		var a Asset
		if err := json.Unmarshal(raw, &a); err != nil {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// Get returns one asset by id.
func Get(ctx context.Context, c *api.Client, id string) (Asset, error) {
	var a Asset
	if err := c.Get(ctx, PathHosts+id+"/", nil, &a); err != nil {
		if isNotFound(err) {
			return Asset{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return Asset{}, err
	}
	return a, nil
}

// FindByName returns the asset with the given name.
//
// The name must match exactly: two assets may share an address, and a
// prefix match would silently pick the wrong one.
func FindByName(ctx context.Context, c *api.Client, name string) (Asset, error) {
	list, err := List(ctx, c)
	if err != nil {
		return Asset{}, err
	}
	for _, a := range list {
		if a.Name == name {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("%w: no asset named %q", ErrNotFound, name)
}

// Create creates a host asset and returns the created asset.
//
// A name that already exists is reported as ErrConflict rather than a
// generic error, so a caller can tell "you already made this" from
// "the server said no".
func Create(ctx context.Context, c *api.Client, spec Spec) (Asset, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return Asset{}, errors.New("asset name is required")
	}
	if strings.TrimSpace(spec.Address) == "" {
		return Asset{}, errors.New("asset address is required")
	}

	platformID := spec.PlatformID
	if platformID == 0 {
		platformID = PlatformIDLinux
	}
	nodeID := spec.NodeID
	if nodeID == "" {
		id, err := DefaultNodeID(ctx, c)
		if err != nil {
			return Asset{}, err
		}
		nodeID = id
	}

	protocols := DefaultProtocols()
	if len(spec.Protocols) > 0 {
		protocols = make([]protocolRow, 0, len(spec.Protocols))
		for _, p := range spec.Protocols {
			protocols = append(protocols, protocolRow{Name: p, Port: 22})
		}
	}

	accounts := make([]accountBody, 0, len(spec.Accounts))
	for _, a := range spec.Accounts {
		accounts = append(accounts, accountBody{
			Name:     a.Name,
			Username: a.Username,
			Password: a.Password,
			Secret:   "password",
		})
	}

	payload := createPayload{
		Name:      spec.Name,
		Address:   spec.Address,
		Platform:  platformID,
		Nodes:     []string{nodeID},
		Comment:   spec.Comment,
		Accounts:  accounts,
		Protocols: protocols,
	}

	var created Asset
	if err := c.Post(ctx, PathHosts, payload, &created); err != nil {
		if isConflict(err) {
			return Asset{}, fmt.Errorf("%w: %q", ErrConflict, spec.Name)
		}
		return Asset{}, err
	}
	return created, nil
}

// UpdateFields carries the mutable fields of an asset. A nil pointer
// means "leave unchanged", so a caller can rename without clearing the
// comment.
type UpdateFields struct {
	Name    *string
	Comment *string
	Address *string
}

// Update applies the given fields to an existing asset.
//
// The server treats PUT as a full replacement: it rejects the request
// unless name, address, platform and protocols are all present ("该字段
// 是必填项"). So the current asset is read first and the caller's fields
// are merged into it; only then is the complete object sent.
//
// A field the caller did not mention keeps its current value — the merge
// is what stops an update from clearing the comment or the accounts.
func Update(ctx context.Context, c *api.Client, id string, fields UpdateFields) (Asset, error) {
	current, err := Get(ctx, c, id)
	if err != nil {
		return Asset{}, err
	}
	if fields.Name == nil && fields.Comment == nil && fields.Address == nil {
		return current, nil
	}

	body := map[string]any{
		"name":      current.Name,
		"address":   current.Address,
		"platform":  current.Platform.ID,
		"nodes":     nodeIDs(current.Nodes),
		"comment":   current.Comment,
		"protocols": DefaultProtocols(),
	}
	if len(current.Accounts) > 0 {
		body["accounts"] = current.Accounts
	}
	if fields.Name != nil {
		body["name"] = *fields.Name
	}
	if fields.Comment != nil {
		body["comment"] = *fields.Comment
	}
	if fields.Address != nil {
		body["address"] = *fields.Address
	}

	var updated Asset
	if err := c.Put(ctx, PathHosts+id+"/", body, &updated); err != nil {
		if isNotFound(err) {
			return Asset{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return Asset{}, err
	}
	return updated, nil
}

// nodeIDs extracts the node ids from an asset's node list.
func nodeIDs(nodes []AssetNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

// Delete removes an asset.
//
// The asset view answers DELETE on the resource itself; it rejects POST
// with "method_not_allowed", so a delete cannot be expressed as a POST
// with an ?action=delete query.
func Delete(ctx context.Context, c *api.Client, id string) error {
	if err := c.Delete(ctx, PathHosts+id+"/", nil); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return err
	}
	return nil
}

// DefaultNodeID returns the id of the default asset node, which is the
// node every host asset belongs to on a fresh install.
func DefaultNodeID(ctx context.Context, c *api.Client) (string, error) {
	nodes, err := Nodes(ctx, c)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.Value == "DEFAULT" || n.Key == "1" {
			return n.ID, nil
		}
	}
	if len(nodes) > 0 {
		return nodes[0].ID, nil
	}
	return "", errors.New("no asset node available")
}

// Nodes returns the asset nodes.
func Nodes(ctx context.Context, c *api.Client) ([]Node, error) {
	items, err := c.GetAll(ctx, PathNodes, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(items))
	for _, raw := range items {
		var n Node
		if err := json.Unmarshal(raw, &n); err != nil {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// Platforms returns the asset platforms.
func Platforms(ctx context.Context, c *api.Client) ([]Platform, error) {
	items, err := c.GetAll(ctx, PathPlatforms, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Platform, 0, len(items))
	for _, raw := range items {
		var p Platform
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// isNotFound reports whether the error is a 404 from the API.
func isNotFound(err error) bool {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 404
	}
	return false
}

// isConflict reports whether the error is a name-uniqueness rejection.
//
// The server answers a duplicate name with HTTP 400 and a per-field
// message ("name": ["字段必须唯一"]), so the status alone cannot tell a
// name clash from a malformed request; the body has to be inspected.
func isConflict(err error) bool {
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != 400 {
		return false
	}
	body := apiErr.Body
	return strings.Contains(body, "name") &&
		(strings.Contains(body, "必须唯一") || strings.Contains(body, "already exists"))
}
