// Package mgmt is the JumpServer v4 management API: assets, accounts,
// users, sessions, audits, jobs, tickets and platform templates.
//
// It is deliberately separate from assets/permits/assetsmanage, which cover
// the slices the connection path needs. This package is the "manage the
// bastion itself" side, and it is read-first: every function here is a GET
// except the few exported writers, which the MCP layer registers only when
// writing is explicitly enabled (MIFA-73).
//
// Dependency direction: mgmt depends on api only. It never touches
// transport, the connection pool, or the MCP layer, so a management call
// can never open a terminal session.
package mgmt

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
)

// ErrPathNotAllowed is the sentinel behind every refusal to fetch a path.
//
// Callers test it with errors.Is so that "the operator typed a path we do
// not serve" and "the server said no" stay distinguishable: the first is a
// local policy decision that never reaches the network.
var ErrPathNotAllowed = errors.New("path is not allowed for read-only access")

// ReadOnlyPrefixes is the whole surface `jms api get` may read.
//
// It is a whitelist, not a blacklist, because the failure modes are
// asymmetric: a missing entry costs one subcommand, an entry we should not
// have added exposes a management area nobody audited. Each entry names the
// domain it unlocks so a reviewer can check it against the JumpServer
// documentation without reading the code.
var ReadOnlyPrefixes = []struct {
	Prefix  string
	Comment string
}{
	{"/api/v1/audits/", "审计日志：登录、操作、改密、文件传输、作业"},
	{"/api/v1/terminal/", "终端会话、命令记录、录像元信息、终端任务"},
	{"/api/v1/assets/", "资产、节点树、平台模板（只读视图）"},
	{"/api/v1/accounts/", "资产账号（不含密钥字段）"},
	{"/api/v1/users/", "用户、用户组、角色、当前用户 profile"},
	{"/api/v1/orgs/", "组织与组织成员"},
	{"/api/v1/perms/", "授权记录与用户可见资产"},
	{"/api/v1/ops/", "作业、作业执行记录、计划任务"},
	{"/api/v1/tickets/", "工单与工单流程"},
	{"/api/v1/settings/", "系统设置（读取；写入永不放行）"},
	{"/api/v1/rbac/", "角色与权限位（v4 RBAC）"},
	{"/api/v1/labels/", "资产标签"},
	{"/api/v1/notifications/", "通知记录"},
}

// forbiddenSegments are path segments that are refused even under an
// allowed prefix.
//
// These are the segments whose GET response carries a secret rather than a
// statement about configuration: a login token, an account's stored
// password, a key. Matching is exact per segment on purpose — a substring
// rule would also block /api/v1/audits/password-change-logs/, which is a
// read we want.
var forbiddenSegments = map[string]bool{
	"authentication": true,
	"auth":           true,
	"secret":         true,
	"secrets":        true,
	"token":          true,
	"tokens":         true,
	"key":            true,
	"keys":           true,
	"passphrase":     true,
	"password":       true,
}

// MaxPathLength bounds the path so a pathological input cannot be turned
// into a megabyte-long error message.
const MaxPathLength = 512

// ValidateReadPath checks that raw may be fetched read-only, and returns
// the cleaned path.
//
// It rejects, in order: an empty value, an absolute URL (which would let a
// caller redirect the request to another host), a non-/api/v1 prefix, a
// traversal or doubled slash, a forbidden segment, and anything outside
// ReadOnlyPrefixes. A query string is refused here: the caller passes
// parameters separately so they can be masked before they are logged.
func ValidateReadPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: empty path", ErrPathNotAllowed)
	}
	if len(trimmed) > MaxPathLength {
		return "", fmt.Errorf("%w: path longer than %d bytes", ErrPathNotAllowed, MaxPathLength)
	}
	if strings.ContainsFunc(trimmed, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("%w: control characters are refused", ErrPathNotAllowed)
	}
	if strings.Contains(trimmed, "://") {
		return "", fmt.Errorf("%w: an absolute URL is not accepted, pass a path starting with /api/v1/", ErrPathNotAllowed)
	}
	if strings.ContainsAny(trimmed, "?#") {
		return "", fmt.Errorf("%w: pass the query string as parameters (-q key=value), not inside the path", ErrPathNotAllowed)
	}
	if !strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("%w: the path must start with /api/v1/", ErrPathNotAllowed)
	}
	if strings.Contains(trimmed, "..") || strings.Contains(trimmed, "//") {
		return "", fmt.Errorf("%w: traversal and empty segments are refused", ErrPathNotAllowed)
	}
	if !strings.HasSuffix(trimmed, "/") {
		// JumpServer redirects a bare collection name to its slashed form.
		// Following that redirect is harmless, but an explicit refusal is
		// easier to explain than a path that silently became another one.
		return "", fmt.Errorf("%w: add the trailing slash (%q)", ErrPathNotAllowed, trimmed+"/")
	}

	// path.Clean drops the trailing slash, so the cleaned form is compared
	// against the trimmed one minus that slash: anything else means the path
	// rewrote itself ("./", "x/../"), which is refused rather than guessed at.
	cleaned := path.Clean(trimmed)
	if cleaned != strings.TrimSuffix(trimmed, "/") {
		return "", fmt.Errorf("%w: unclean path %q", ErrPathNotAllowed, trimmed)
	}
	if !strings.HasPrefix(cleaned, "/api/v1/") {
		return "", fmt.Errorf("%w: only /api/v1/... management paths are served", ErrPathNotAllowed)
	}

	for _, seg := range strings.Split(strings.Trim(cleaned, "/"), "/") {
		if forbiddenSegments[strings.ToLower(seg)] {
			return "", fmt.Errorf("%w: the %q segment carries credentials and is never readable", ErrPathNotAllowed, seg)
		}
	}

	for _, entry := range ReadOnlyPrefixes {
		if strings.HasPrefix(cleaned+"/", entry.Prefix) {
			return cleaned + "/", nil
		}
	}
	return "", fmt.Errorf("%w: %q is outside the read-only management surface", ErrPathNotAllowed, trimmed)
}

// ReadablePrefixes returns the allowed prefixes, for help text and for the
// refusal message a caller sees.
func ReadablePrefixes() []string {
	out := make([]string, 0, len(ReadOnlyPrefixes))
	for _, entry := range ReadOnlyPrefixes {
		out = append(out, entry.Prefix)
	}
	sort.Strings(out)
	return out
}

// MaskQuery renders a query string for the audit log with every
// credential-looking value replaced.
//
// The management read surface has no business carrying a password, but the
// audit log is exactly the place a mistake would become permanent, so the
// masking happens on the way in rather than on the way out.
func MaskQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		if isSensitiveKey(k) {
			b.WriteString("***")
			continue
		}
		joined := strings.Join(values[k], ",")
		b.WriteString(truncate(joined, 128))
	}
	return b.String()
}

// isSensitiveKey reports whether a query parameter name looks like a
// credential. Prefix matching is intentional: "password", "new_password"
// and "account_secret" must all be masked, while "secret_count" is close
// enough to be masked too — a false positive costs nothing here.
func isSensitiveKey(k string) bool {
	lower := strings.ToLower(k)
	for _, marker := range []string{"password", "passwd", "secret", "token", "key", "passphrase", "otp"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// truncate bounds s to at most n bytes without splitting a UTF-8 rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8Start(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// utf8Start reports whether b is a UTF-8 leading byte.
func utf8Start(b byte) bool {
	return b&0xC0 != 0x80
}
