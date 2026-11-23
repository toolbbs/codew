package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"codew/backend/internal/security"
	"codew/backend/internal/store"
)

func codewRuntimeStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "codew.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCodewHealthIdentity(t *testing.T) {
	r := httptest.NewRecorder()
	NewRouter(codewRuntimeStore(t)).ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if r.Code != http.StatusOK {
		t.Fatalf("health status: %d", r.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["service"] != "codew-api" || body["status"] != "ok" || body["version"] != panelVersion {
		t.Fatal("health identity or version mismatch")
	}
}

func codewAuthenticatedRead(t *testing.T, handler http.Handler, secret string) int {
	t.Helper()
	token, err := security.SignJWT(secret, security.Claims{Sub: "synthetic-admin", Username: "synthetic", Role: "super_admin", Exp: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response.Code
}

func TestCodewJWTNamespaceTakesPrecedence(t *testing.T) {
	t.Setenv("CODEW_JWT_SECRET", "synthetic-codew-secret")
	t.Setenv("ZXY_JWT_SECRET", "synthetic-legacy-secret")
	handler := NewRouter(codewRuntimeStore(t))
	if code := codewAuthenticatedRead(t, handler, "synthetic-codew-secret"); code != http.StatusOK {
		t.Fatalf("new namespace status: %d", code)
	}
	if code := codewAuthenticatedRead(t, handler, "synthetic-legacy-secret"); code != http.StatusUnauthorized {
		t.Fatalf("legacy namespace accepted, status: %d", code)
	}
}

func TestCodewDoesNotFallBackToLegacyJWT(t *testing.T) {
	t.Setenv("CODEW_JWT_SECRET", "")
	t.Setenv("ZXY_JWT_SECRET", "synthetic-legacy-secret")
	if code := codewAuthenticatedRead(t, NewRouter(codewRuntimeStore(t)), "synthetic-legacy-secret"); code != http.StatusUnauthorized {
		t.Fatalf("legacy-only credentials accepted, status: %d", code)
	}
}

func TestCodewAdminSeedUsesOnlyNewNamespace(t *testing.T) {
	t.Setenv("CODEW_ADMIN_USERNAME", "codew-fixture-admin")
	t.Setenv("CODEW_ADMIN_PASSWORD", "synthetic-codew-password")
	t.Setenv("ZXY_ADMIN_USERNAME", "legacy-fixture-admin")
	t.Setenv("ZXY_ADMIN_PASSWORD", "synthetic-legacy-password")
	s := codewRuntimeStore(t)
	admin, ok := s.FindAdminByUsername("codew-fixture-admin")
	if !ok || !security.VerifyPassword("synthetic-codew-password", admin.PasswordHash) {
		t.Fatal("new namespace admin was not initialized")
	}
	if _, ok := s.FindAdminByUsername("legacy-fixture-admin"); ok {
		t.Fatal("legacy namespace admin was imported")
	}
}

func TestCodewMissingLocalIdentityStillBlocksDeletion(t *testing.T) {
	s := codewRuntimeStore(t)
	var id string
	for key := range s.Data.Servers {
		id = key
		break
	}
	t.Setenv("CODEW_LOCAL_SERVER_ID", "")
	t.Setenv("ZXY_LOCAL_SERVER_ID", id)
	s.Mu.Lock()
	defer s.Mu.Unlock()
	conflicts := (&Router{store: s}).serverDeleteConflictsLocked(id)
	for _, conflict := range conflicts {
		if conflict.Kind == "local_identity_unknown" {
			return
		}
	}
	t.Fatal("missing codew identity did not fail closed")
}
