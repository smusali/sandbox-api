package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnknownRouteIsNotFound(t *testing.T) {
	handler := NewRouter(nil, Routes{})
	for _, target := range []string{"/missing", "/v1/projects", "/v1/projects/", "/auth/login/", "/Auth/login"} {
		t.Run(target, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
			assertAPIError(t, rec, http.StatusNotFound, CodeNotFound, "The requested resource was not found.")
			if rec.Header().Get("WWW-Authenticate") != "" {
				t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

func TestPublicRouteRejectsImplicitHead(t *testing.T) {
	var protected int
	handler := NewRouter(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			protected++
			next.ServeHTTP(w, r)
		})
	}, Routes{
		Login: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
	})

	ok := httptest.NewRecorder()
	handler.ServeHTTP(ok, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if ok.Code != http.StatusNoContent {
		t.Fatalf("GET status = %d", ok.Code)
	}

	for _, method := range []string{http.MethodHead, http.MethodPost, "get", http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(method, "/auth/login", nil))
			assertAPIError(t, rec, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "The requested method is not allowed.")
			if rec.Header().Get("Allow") != http.MethodGet {
				t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
			}
			if strings.Contains(rec.Header().Get("Allow"), http.MethodHead) {
				t.Fatalf("Allow listed HEAD: %q", rec.Header().Get("Allow"))
			}
		})
	}
	if protected != 0 {
		t.Fatalf("protected calls = %d", protected)
	}
}

func TestProtectedRouteFailsClosedWithoutMiddleware(t *testing.T) {
	handler := NewRouter(nil, Routes{
		CreateProject: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler ran")
		}),
		ListProjects: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler ran")
		}),
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/projects", nil))
	assertAPIError(t, rec, http.StatusUnauthorized, CodeUnauthenticated, "A valid access token is required.")
	if rec.Header().Get("Allow") != "" {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}
	if rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestProtectedMethodNotAllowedAfterAuthentication(t *testing.T) {
	handler := NewRouter(passThrough, Routes{
		CreateProject: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		}),
		ListProjects: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	})

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/projects", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d", get.Code)
	}
	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/v1/projects", nil))
	if post.Code != http.StatusCreated {
		t.Fatalf("POST status = %d", post.Code)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/v1/projects", nil))
	assertAPIError(t, rec, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "The requested method is not allowed.")
	if rec.Header().Get("Allow") != "GET, POST" {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}
}

func TestDeleteSandboxAllowHeader(t *testing.T) {
	handler := NewRouter(passThrough, Routes{
		StopSandbox: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			WriteNoContent(w)
		}),
	})
	ok := httptest.NewRecorder()
	handler.ServeHTTP(ok, httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/7719ac21-1b89-4865-89b5-0ed2f8721003", nil))
	if ok.Code != http.StatusNoContent || ok.Body.Len() != 0 {
		t.Fatalf("status = %d, body = %q", ok.Code, ok.Body.String())
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sandboxes/7719ac21-1b89-4865-89b5-0ed2f8721003", nil))
	assertAPIError(t, rec, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "The requested method is not allowed.")
	if rec.Header().Get("Allow") != http.MethodDelete {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}
}

func TestPathIDValidation(t *testing.T) {
	handler := NewRouter(passThrough, Routes{
		CreateSandbox: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := PathID(r, "projectID"); err != nil {
				WriteError(w, err)
				return
			}
			WriteNoContent(w)
		}),
	})

	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/v1/projects/not-a-uuid/sandboxes", nil))
	assertAPIError(t, bad, http.StatusBadRequest, CodeInvalidRequest, "The supplied request is invalid.")

	ok := httptest.NewRecorder()
	handler.ServeHTTP(ok, httptest.NewRequest(http.MethodPost, "/v1/projects/6d502949-7e8c-4f47-8b0c-3f1e3b913001/sandboxes", nil))
	if ok.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", ok.Code, ok.Body.String())
	}
}

func TestPanicBecomesInternalError(t *testing.T) {
	secret := "pq: secret stack token"
	handler := NewRouter(nil, Routes{
		Login: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(secret)
		}),
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("body leaked panic: %s", rec.Body.String())
	}
	assertAPIError(t, rec, http.StatusInternalServerError, CodeInternalError, "The request could not be completed.")
}

func TestTrailingSlashStaysNotFoundWhenRouteExists(t *testing.T) {
	handler := NewRouter(passThrough, Routes{
		ListProjects: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/", nil))
	assertAPIError(t, rec, http.StatusNotFound, CodeNotFound, "The requested resource was not found.")
}

func passThrough(next http.Handler) http.Handler {
	return next
}
