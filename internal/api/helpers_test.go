package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// PathSelfAssetsLike is a neutral list-endpoint path used by the pagination
// tests; the api package must stay decoupled from any concrete resource.
const PathSelfAssetsLike = "/api/v1/perms/users/self/assets/"

// newTestServer starts an in-process server and registers its cleanup.
func newTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}
