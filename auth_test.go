package hubsync

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthMiddlewareValidToken(t *testing.T) {
	handler := AuthMiddleware(BearerToken("secret"), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestAuthMiddlewareInvalidToken(t *testing.T) {
	handler := AuthMiddleware(BearerToken("secret"), func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called with invalid token")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddlewareMissingToken(t *testing.T) {
	handler := AuthMiddleware(BearerToken("secret"), func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called without auth header")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddlewareEmptyToken(t *testing.T) {
	called := false
	handler := AuthMiddleware(BearerToken(""), func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if !called {
		t.Error("handler should be called when token is empty (no auth)")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestAuthMiddlewareMalformedHeader(t *testing.T) {
	handler := AuthMiddleware(BearerToken("secret"), func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called with malformed auth")
	})

	// No "Bearer " prefix
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Basic abc123")
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
