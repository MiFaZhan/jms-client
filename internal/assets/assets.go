// Package assets looks up JumpServer assets and resolves their connection
// parameters.
//
// Endpoints and selection rules mirror the Python reference implementation
// (DESIGN.md「KoKo 协议」的「认证」). The account-selection order in particular is not
// arbitrary: the connection-token API requires the account *alias*
// (e.g. "@USER"), and the server rejects the display name.
//
// Dependency direction: assets depends on api.
package assets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// PathSelfAssets is the listing endpoint; a detail request appends
// "<id>/".
const (
	PathSelfAssets = "/api/v1/perms/users/self/assets/"
)

// ErrNotFound is returned when no asset matches a name.
var ErrNotFound = errors.New("asset not found")

// Asset is one row of an asset listing.
type Asset struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Platform string `json:"platform"`
	// Type is the asset type label (e.g. "Linux", "网站"). The API returns
	// it as {"label":...,"value":...} or as a bare string.
	Type string `json:"type"`
}

// Info is a resolved, connectable asset.
type Info struct {
	ID       string
	Name     string
	Address  string
	Account  string
	Protocol string
	Platform string
	Type     string
	OrgID    string
}

// rawAsset is the shape the listing returns.
//
// Platform is polymorphic: some JumpServer versions return a string
// ("Linux"), others an object ({"name": "Linux"}). It is decoded as raw
// JSON and rendered by platformName, so either shape parses. Type is
// polymorphic the same way; it is not exposed on Asset, but declaring it
// as raw documents that either shape must stay parseable.
type rawAsset struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Address  string          `json:"address"`
	Platform json.RawMessage `json:"platform"`
	OrgID    string          `json:"org_id"`
	Type     json.RawMessage `json:"type"`
}

// List returns authorized assets, at most limit entries.
//
// A limit of 0 or less returns everything. Pagination is api.Client.GetAll's
// job, so the whole listing is fetched and then truncated; the Python
// reference stops paging early, but the Go transport materializes pages.
func List(ctx context.Context, c *api.Client, limit int) ([]Asset, error) {
	items, err := c.GetAll(ctx, PathSelfAssets, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Asset, 0, len(items))
	for _, raw := range items {
		asset, ok := decodeAsset(raw)
		if !ok {
			// One malformed row must not fail the whole listing.
			continue
		}
		out = append(out, asset)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Search returns every authorized asset matching keyword (name, address
// or comment).
func Search(ctx context.Context, c *api.Client, keyword string) ([]Asset, error) {
	items, err := c.GetAll(ctx, PathSelfAssets, map[string][]string{"search": {keyword}})
	if err != nil {
		return nil, err
	}
	out := make([]Asset, 0, len(items))
	for _, raw := range items {
		asset, ok := decodeAsset(raw)
		if !ok {
			continue
		}
		out = append(out, asset)
	}
	return out, nil
}

// Resolve searches an asset by name and resolves its connection
// parameters. An exact name match wins; otherwise the first decodable
// search result is used.
//
// account and protocol override the automatic selection when non-empty.
//
// Errors: ErrNotFound (wrapped) when the search yields no usable asset.
func Resolve(ctx context.Context, c *api.Client, name, account, protocol string) (Info, error) {
	items, err := c.GetAll(ctx, PathSelfAssets, map[string][]string{"search": {name}})
	if err != nil {
		return Info{}, err
	}

	// Decode every row first: a malformed row is skipped rather than
	// fatal, and the "first result" fallback must skip past it too.
	decoded := make([]rawAsset, 0, len(items))
	for _, raw := range items {
		var candidate rawAsset
		if err := json.Unmarshal(raw, &candidate); err != nil {
			continue
		}
		decoded = append(decoded, candidate)
	}
	if len(decoded) == 0 {
		return Info{}, fmt.Errorf("%w: no asset matches %q", ErrNotFound, name)
	}

	target := decoded[0]
	for _, candidate := range decoded {
		if candidate.Name == name {
			target = candidate
			break
		}
	}

	detail, err := fetchAssetDetail(ctx, c, target.ID)
	if err != nil {
		return Info{}, err
	}

	selAccount := account
	if strings.TrimSpace(selAccount) == "" {
		selAccount = SelectAccount(detail.PermedAccounts)
	}
	selProtocol := protocol
	if strings.TrimSpace(selProtocol) == "" {
		selProtocol = SelectProtocol(detail.PermedProtocols)
	}

	return Info{
		ID:       target.ID,
		Name:     firstNonEmpty(target.Name, name),
		Address:  target.Address,
		Account:  selAccount,
		Protocol: selProtocol,
		Platform: platformName(target.Platform),
		Type:     labelName(target.Type),
		OrgID:    target.OrgID,
	}, nil
}

// assetDetail is the asset detail payload with the permed account and
// protocol lists.
type assetDetail struct {
	PermedAccounts  []map[string]any `json:"permed_accounts"`
	PermedProtocols []map[string]any `json:"permed_protocols"`
}

func fetchAssetDetail(ctx context.Context, c *api.Client, id string) (assetDetail, error) {
	var detail assetDetail
	if err := c.Get(ctx, PathSelfAssets+id+"/", nil, &detail); err != nil {
		return assetDetail{}, err
	}
	return detail, nil
}

// decodeAsset renders one listing row, reporting false when the row is
// malformed (e.g. a numeric id) so the caller can skip it.
func decodeAsset(raw json.RawMessage) (Asset, bool) {
	var a rawAsset
	if err := json.Unmarshal(raw, &a); err != nil {
		return Asset{}, false
	}
	return Asset{
		ID:       a.ID,
		Name:     a.Name,
		Address:  a.Address,
		Platform: platformName(a.Platform),
		Type:     labelName(a.Type),
	}, true
}

// SelectAccount picks the best account alias: @USER > a named account >
// @INPUT.
//
// The connection-token API needs the account *alias* (e.g. "@USER"), not
// the display name (e.g. "Dynamic user"). An entry that carries neither a
// usable alias nor username is skipped, which is why the "@USER" scan is
// a separate pass.
func SelectAccount(permed []map[string]any) string {
	if len(permed) == 0 {
		return "@INPUT"
	}
	for _, acc := range permed {
		if alias, _ := acc["alias"].(string); alias == "@USER" {
			return "@USER"
		}
	}
	for _, acc := range permed {
		if alias, _ := acc["alias"].(string); alias != "" && !strings.HasPrefix(alias, "@") {
			return alias
		}
		if username, _ := acc["username"].(string); username != "" && !strings.HasPrefix(username, "@") {
			return username
		}
	}
	// Nothing better is available: fall back to the first entry's alias,
	// then its username, mirroring the Python `first.get("alias",
	// first.get("username", "@INPUT"))`.
	first := permed[0]
	if alias, ok := first["alias"].(string); ok {
		return alias
	}
	if username, ok := first["username"].(string); ok {
		return username
	}
	return "@INPUT"
}

// SelectProtocol picks the best protocol, preferring ssh (case
// insensitive). It returns the canonical lower-case "ssh" so callers do
// not have to normalize.
func SelectProtocol(permed []map[string]any) string {
	if len(permed) == 0 {
		return "ssh"
	}
	for _, proto := range permed {
		if name, _ := proto["name"].(string); strings.EqualFold(name, "ssh") {
			return "ssh"
		}
	}
	if name, _ := permed[0]["name"].(string); name != "" {
		return name
	}
	return "ssh"
}

// platformName renders the polymorphic platform field: the API returns
// either a string or {"name": "..."}. Anything else renders as "".
// labelName renders a polymorphic label field.
//
// The API returns these as {"label":"Linux","value":"linux"} (type,
// category) or as a bare string on older versions; label wins, then value,
// then the bare string.
func labelName(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	var obj struct {
		Label string `json:"label"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	if obj.Label != "" {
		return obj.Label
	}
	return obj.Value
}

func platformName(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	var obj struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.Name
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
