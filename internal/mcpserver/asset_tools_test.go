package mcpserver

import (
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestAssetToolsAreRegistered pins the asset-management tool names.
//
// The names matter: an MCP client binds to them, so a rename is a
// breaking change and must be deliberate rather than accidental. Each
// factory is called directly rather than through the SDK server so the
// assertion does not depend on the SDK's introspection API.
func TestAssetToolsAreRegistered(t *testing.T) {
	s := &Server{opts: Options{}}

	factories := map[string]func() (*mcp.Tool, mcp.ToolHandler){
		ToolAssetList:        s.assetListTool,
		ToolAssetShow:        s.assetShowTool,
		ToolAssetCreate:      s.assetCreateTool,
		ToolAssetUpdate:      s.assetUpdateTool,
		ToolAssetDelete:      s.assetDeleteTool,
		ToolAssetGrant:       s.assetGrantTool,
		ToolAssetPermissions: s.assetPermissionsTool,
		ToolAssetRevoke:      s.assetRevokeTool,
	}

	want := []string{
		ToolAssetCreate, ToolAssetDelete, ToolAssetGrant, ToolAssetList,
		ToolAssetPermissions, ToolAssetRevoke, ToolAssetShow, ToolAssetUpdate,
	}
	sort.Strings(want)

	for _, name := range want {
		factory, ok := factories[name]
		if !ok {
			t.Errorf("no factory for %q", name)
			continue
		}
		tool, handler := factory()
		if tool == nil {
			t.Errorf("%s: factory returned no tool", name)
			continue
		}
		// The tool's own name must match the constant, or a client looking
		// it up by the documented name will not find it.
		if tool.Name != name {
			t.Errorf("tool.Name = %q, want %q", tool.Name, name)
		}
		if handler == nil {
			t.Errorf("%s: factory returned no handler", name)
		}
		if tool.Description == "" {
			t.Errorf("%s: description is empty, so a client cannot tell what it does", name)
		}
		if tool.InputSchema == nil {
			t.Errorf("%s: input schema is empty", name)
		}
	}
}

// TestToolNamesAreUnique guards against two tools sharing a name, which
// would make one of them unreachable.
func TestToolNamesAreUnique(t *testing.T) {
	names := []string{
		ToolLS, ToolExec, ToolResolveAsset, ToolConfigList,
		ToolAssetList, ToolAssetShow, ToolAssetCreate, ToolAssetUpdate,
		ToolAssetDelete, ToolAssetGrant, ToolAssetPermissions, ToolAssetRevoke,
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate tool name %q", n)
		}
		seen[n] = true
	}
}
