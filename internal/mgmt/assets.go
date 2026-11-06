package mgmt

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// Asset tree and platform paths.
//
// Verified 2026-11-06 on a live v4: nodes answered 200 with 8 records
// (fields id/key/value/full_value/name/org_name), platforms answered 200.
const (
	PathNodes     = "/api/v1/assets/nodes/"
	PathPlatforms = "/api/v1/assets/platforms/"
)

// Node is one node-tree entry.
type Node struct {
	ID       string `json:"id"`
	Key      string `json:"key,omitempty"`
	Value    string `json:"value"`
	FullVal  string `json:"full_value,omitempty"`
	Name     string `json:"name,omitempty"`
	OrgID    string `json:"org_id,omitempty"`
	OrgName  string `json:"org_name,omitempty"`
	Children []Node `json:"children,omitempty"`
}

// ListNodes returns the whole node list.
//
// The v4 node view is a flat list; the tree is rebuilt here because "where
// does this asset live" is a path question and a flat list makes the caller
// do the joining. Depth is a hard cap (8) so a malformed full_value cannot
// recurse forever.
func ListNodes(ctx context.Context, c *api.Client) ([]Node, error) {
	page, err := listPage[Node](ctx, c, PathNodes, ListOptions{Limit: MaxLimit})
	if err != nil {
		return nil, err
	}
	return BuildTree(page.Results), nil
}

// MaxNodeDepth caps tree reconstruction.
const MaxNodeDepth = 8

// BuildTree turns the flat node list into a tree keyed by full_value.
//
// full_value is the node's path ("default", "default/生产"), so a node's
// parent is its full_value with the last segment removed. Nodes whose parent
// is missing stay at the root rather than being dropped: a lost subtree is
// worse than a flat one, and the alternative is silently hiding an asset
// location from the very tool meant to show it.
//
// The tree is built in two passes — link, then render — because attaching
// children while copying roots would leave the copies empty. That bug is
// easy to write and invisible in a flat listing, so the two-phase shape is
// deliberate.
func BuildTree(flat []Node) []Node {
	type link struct {
		node     Node
		children []*link
	}

	index := make(map[string]*link, len(flat))
	order := make([]*link, 0, len(flat))
	for _, n := range flat {
		n.Children = nil
		if n.FullVal == "" {
			n.FullVal = n.Value
		}
		l := &link{node: n}
		order = append(order, l)
		// First writer wins: two nodes sharing a path are already a broken
		// tree, and re-pointing the index would move an existing child.
		if _, exists := index[n.FullVal]; !exists {
			index[n.FullVal] = l
		}
	}

	var roots []*link
	for _, l := range order {
		parentPath, hasParent := parentOf(l.node.FullVal)
		if hasParent {
			if parent, ok := index[parentPath]; ok && parent != l {
				parent.children = append(parent.children, l)
				continue
			}
		}
		roots = append(roots, l)
	}

	var render func(*link, int) Node
	render = func(l *link, depth int) Node {
		out := l.node
		if depth >= MaxNodeDepth {
			out.Children = nil
			return out
		}
		for _, child := range l.children {
			out.Children = append(out.Children, render(child, depth+1))
		}
		return out
	}

	tree := make([]Node, 0, len(roots))
	for _, l := range roots {
		tree = append(tree, render(l, 0))
	}
	return tree
}

// parentOf splits a node path into its parent path.
func parentOf(full string) (string, bool) {
	idx := strings.LastIndex(full, "/")
	if idx <= 0 {
		return "", false
	}
	return full[:idx], true
}

// Platform is one asset platform template.
type Platform struct {
	ID        int        `json:"id"`
	Name      string     `json:"name"`
	Type      FlexString `json:"type,omitempty"`
	Category  FlexString `json:"category,omitempty"`
	Protocols []struct {
		Name string `json:"name"`
		Port int    `json:"port,omitempty"`
	} `json:"protocols,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// ListPlatforms returns one page of platform templates.
func ListPlatforms(ctx context.Context, c *api.Client, keyword string, limit int) (Page[Platform], error) {
	page, err := listPage[Platform](ctx, c, PathPlatforms, ListOptions{Limit: Window(limit, keyword != "")})
	if err != nil {
		return Page[Platform]{}, err
	}
	kept, _ := FilterRows(page.Results, limit, func(p Platform) bool {
		return MatchKeyword(keyword, p.Name, string(p.Type), string(p.Category))
	})
	return Page[Platform]{Count: page.Count, Results: kept}, nil
}

// ResolveNode finds one node by exact name or id.
//
// It exists so the write path can accept a human-typed node name and still
// send an id: creating an asset under the wrong node is a mistake that is
// cheap to prevent here and annoying to undo by hand.
func ResolveNode(ctx context.Context, c *api.Client, idOrName string) (Node, error) {
	slug := strings.TrimSpace(idOrName)
	if slug == "" {
		return Node{}, fmt.Errorf("a node id or exact name is required")
	}
	page, err := listPage[Node](ctx, c, PathNodes, ListOptions{Limit: MaxLimit, Filters: url.Values{"search": []string{slug}}})
	if err != nil {
		return Node{}, err
	}
	var matches []Node
	for _, n := range page.Results {
		if n.ID == slug || n.Name == slug || n.Value == slug {
			matches = append(matches, n)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return Node{}, fmt.Errorf("no node named %q", slug)
	}
	return Node{}, fmt.Errorf("%q matches %d nodes; use the id instead", slug, len(matches))
}
