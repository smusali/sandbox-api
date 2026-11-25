package httpapi

import (
	"log"
	"net/http"
	"strings"
)

// Middleware wraps a handler, as authentication does for protected routes.
type Middleware func(http.Handler) http.Handler

// Routes holds the handlers mounted by NewRouter.
// A nil handler leaves that method unregistered.
type Routes struct {
	Login         http.Handler
	Callback      http.Handler
	CreateProject http.Handler
	ListProjects  http.Handler
	AddMember     http.Handler
	CreateSandbox http.Handler
	ListSandboxes http.Handler
	StopSandbox   http.Handler
}

type endpoint struct {
	pattern string
	public  bool
	methods map[string]http.Handler
}

// NewRouter serves the public auth paths and the protected /v1 paths.
// A protected path with no authentication middleware is rejected as unauthenticated.
// HEAD is not treated as GET.
func NewRouter(protect Middleware, routes Routes) http.Handler {
	mux := http.NewServeMux()
	for _, ep := range endpoints(routes) {
		if len(ep.methods) == 0 {
			continue
		}
		mux.Handle(ep.pattern, ep.handler(protect))
	}
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, NotFound())
	}))
	return recoverHandler(mux)
}

func endpoints(routes Routes) []endpoint {
	return []endpoint{
		{pattern: "/auth/login", public: true, methods: oneMethod(http.MethodGet, routes.Login)},
		{pattern: "/auth/callback", public: true, methods: oneMethod(http.MethodGet, routes.Callback)},
		{
			pattern: "/v1/projects",
			methods: twoMethods(http.MethodGet, routes.ListProjects, http.MethodPost, routes.CreateProject),
		},
		{
			pattern: "/v1/projects/{projectID}/members",
			methods: oneMethod(http.MethodPost, routes.AddMember),
		},
		{
			pattern: "/v1/projects/{projectID}/sandboxes",
			methods: twoMethods(http.MethodGet, routes.ListSandboxes, http.MethodPost, routes.CreateSandbox),
		},
		{
			pattern: "/v1/sandboxes/{sandboxID}",
			methods: oneMethod(http.MethodDelete, routes.StopSandbox),
		},
	}
}

func oneMethod(method string, handler http.Handler) map[string]http.Handler {
	if handler == nil {
		return nil
	}
	return map[string]http.Handler{method: handler}
}

func twoMethods(firstMethod string, first http.Handler, secondMethod string, second http.Handler) map[string]http.Handler {
	methods := make(map[string]http.Handler)
	if first != nil {
		methods[firstMethod] = first
	}
	if second != nil {
		methods[secondMethod] = second
	}
	if len(methods) == 0 {
		return nil
	}
	return methods
}

func (ep endpoint) handler(protect Middleware) http.Handler {
	allow := allowHeader(ep.methods)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selected := ep.methods[r.Method]
		if selected == nil {
			WriteError(w, MethodNotAllowed(allow))
			return
		}
		selected.ServeHTTP(w, r)
	})
	if ep.public {
		return next
	}
	if protect == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			WriteError(w, Unauthenticated())
		})
	}
	return protect(next)
}

func allowHeader(methods map[string]http.Handler) string {
	order := []string{http.MethodGet, http.MethodPost, http.MethodDelete}
	allowed := make([]string, 0, len(order))
	for _, method := range order {
		if methods[method] != nil {
			allowed = append(allowed, method)
		}
	}
	return strings.Join(allowed, ", ")
}

func recoverHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			log.Printf("internal error: %v", recovered)
			WriteError(w, Internal())
		}()
		next.ServeHTTP(w, r)
	})
}
