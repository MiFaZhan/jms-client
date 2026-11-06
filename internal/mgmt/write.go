package mgmt

import (
	"context"
	"fmt"
	"strings"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// Write-side management calls.
//
// Everything here mutates the bastion, so the MCP layer registers it only
// when writing is explicitly switched on, and only for the domains named.
// The paths follow the v4 convention and are NOT verified against a live
// instance: a write cannot be probed safely from production, so these are
// covered by unit tests against a fake server and flagged for
// rehearsal-environment verification.
//
// The scope is deliberately two operations. Node create/update/delete is
// how an asset tree gets maintained between console visits; account
// password rotation is the one routine operation that is both frequent and
// easy to get wrong by hand. Everything else (user changes, session kill,
// job execution, ticket approval) waits for a real need.

// CreateNode creates one node under parentID (empty means the root).
//
// full_value is the node's path and is what the tree is rebuilt from, so it
// is computed here rather than left to the caller: a mismatched full_value
// produces a node that exists but is invisible in its parent.
func CreateNode(ctx context.Context, c *api.Client, parentID, name string) (Node, error) {
	cleaned := strings.TrimSpace(name)
	if cleaned == "" {
		return Node{}, fmt.Errorf("a node name is required")
	}
	if strings.Contains(cleaned, "/") {
		return Node{}, fmt.Errorf("a node name cannot contain %q", "/")
	}

	fullValue := cleaned
	if strings.TrimSpace(parentID) != "" {
		parent, err := ResolveNode(ctx, c, parentID)
		if err != nil {
			return Node{}, err
		}
		parentPath := parent.FullVal
		if parentPath == "" {
			parentPath = parent.Value
		}
		if parentPath == "" {
			return Node{}, fmt.Errorf("parent node %q has no path to build on", parentID)
		}
		fullValue = parentPath + "/" + cleaned
	}

	body := map[string]any{"value": cleaned, "full_value": fullValue}
	var out Node
	if err := c.Post(ctx, PathNodes, body, &out); err != nil {
		return Node{}, err
	}
	return out, nil
}

// RenameNode changes one node's display value.
//
// Renaming a node changes its full_value, and children carry the old path.
// The bastion rewrites the subtree itself; this function therefore sends
// only the new value and lets the server own the cascade. Callers are told
// to verify the children afterwards.
func RenameNode(ctx context.Context, c *api.Client, idOrName, newName string) (Node, error) {
	node, err := ResolveNode(ctx, c, idOrName)
	if err != nil {
		return Node{}, err
	}
	cleaned := strings.TrimSpace(newName)
	if cleaned == "" {
		return Node{}, fmt.Errorf("a node name is required")
	}
	if strings.Contains(cleaned, "/") {
		return Node{}, fmt.Errorf("a node name cannot contain %q", "/")
	}
	body := map[string]any{"value": cleaned}
	var out Node
	if err := c.Patch(ctx, PathNodes+node.ID+"/", body, &out); err != nil {
		return Node{}, err
	}
	return out, nil
}

// DeleteNode removes one node.
//
// The bastion refuses to delete a node that still holds assets or child
// nodes, which is the safety net this function relies on instead of walking
// the subtree itself: refusing locally would duplicate a rule the server
// already enforces and might get wrong.
func DeleteNode(ctx context.Context, c *api.Client, idOrName string) (Node, error) {
	node, err := ResolveNode(ctx, c, idOrName)
	if err != nil {
		return Node{}, err
	}
	if err := c.Delete(ctx, PathNodes+node.ID+"/", nil); err != nil {
		return Node{}, err
	}
	return node, nil
}
