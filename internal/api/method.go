package api

// Method-aware routing.
//
// net/http's pattern mux answers 405 with an Allow header all by itself — but
// only when nothing else in the table matches the request at all. This server
// mounts a catch-all at "/api/v1/" so an unmatched API path gets the JSON error
// envelope instead of the dashboard's HTML, and that catch-all matches every
// method. The mux therefore never reaches its own method-mismatch branch, and
// DELETE /api/v1/fleet answers 404.
//
// 404 is the wrong answer and it is wrong in an expensive direction. It says
// "there is no such resource", so a client believes the fleet endpoint does not
// exist on this deployment: an SDK falls back to a legacy path, a health check
// declares the control plane a stranger, an operator goes looking for a
// misconfigured ingress. The resource is fine. The verb is wrong. 405 with an
// Allow header says exactly that in one round trip, and Allow tells the caller
// what to send instead.
//
// The check runs INSIDE the catch-all, after authentication, not as outer
// middleware. That is deliberate: the catch-all is authenticated because an
// unauthenticated 404 map is a free inventory of the control plane, and an
// unauthenticated 405 map is a better one — it enumerates paths AND their
// verbs. So an anonymous caller still gets 401, and only a caller the server
// already trusts learns that the path exists.

import (
	"net/http"
	"slices"
	"strings"
)

// CodeMethodNotAllowed accompanies 405. It is separate from CodeNotFound
// because the two mean opposite things to a client: not_found is "stop asking
// for this", method_not_allowed is "ask again with one of these verbs".
const CodeMethodNotAllowed = "method_not_allowed"

// probeMethods are the verbs the routing table is asked about when a request
// falls through to the catch-all.
//
// The list is the set a REST client could plausibly send, not the set this API
// registers, so a path that gains a PUT tomorrow is described correctly without
// anyone remembering to edit this. OPTIONS is absent on purpose: it is never
// registered as a pattern here and is answered below rather than routed.
var probeMethods = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
}

// methodQualified reports whether a ServeMux pattern names a method.
//
// A Go 1.22 pattern is "[METHOD ][HOST]/[PATH]", so the method — and only the
// method — introduces a space. "GET /api/v1/fleet" is a real route; "/api/v1/"
// is the catch-all that swallowed the request. That distinction is the whole
// mechanism: if the pattern that matched carries no method, no registered route
// wanted this request, and it is worth asking which ones would have.
func methodQualified(pattern string) bool {
	return strings.Contains(pattern, " ")
}

// allowedMethods asks mux which methods would have matched this request's path,
// and returns them sorted together with the path half of the pattern they
// matched.
//
// It works by re-asking the same mux with the method swapped, which means the
// answer is derived from the live routing table rather than from a parallel
// list somebody has to remember to update. A route added to router.go is
// described correctly by this function the moment it is registered.
//
// The request is shallow-copied per probe. ServeMux.Handler only reads Method,
// URL and Host, and never writes to the request, so the copy is enough and a
// full Clone's header duplication would be waste on a path that already lost.
//
// OPTIONS joins any non-empty result because this package answers OPTIONS
// itself. An empty result means no registered route matches the path under any
// verb — a genuinely unknown path, which must stay a 404.
func allowedMethods(mux *http.ServeMux, r *http.Request) (allow []string, route string) {
	for _, m := range probeMethods {
		if m == r.Method {
			// This method already failed to match; asking again cannot change
			// the answer, and listing it in Allow would be a lie the client
			// would act on by retrying the request it just sent.
			continue
		}
		probe := *r
		probe.Method = m
		_, pattern := mux.Handler(&probe)
		if !methodQualified(pattern) {
			continue
		}
		allow = append(allow, m)
		if route == "" {
			route = pattern[strings.IndexByte(pattern, ' ')+1:]
		}
	}
	if len(allow) == 0 {
		return nil, ""
	}
	allow = append(allow, http.MethodOptions)
	// Sorted so the header is byte-identical across restarts: probeMethods
	// happens to be ordered today, but a header a client caches or a test
	// asserts on should not depend on that.
	slices.Sort(allow)
	return slices.Compact(allow), route
}

// methodFallback wraps notFound so that a known path reached with an
// unregistered method answers 405 instead of falling through.
//
// mux must be the same mux this handler is mounted on: the probe asks it what
// it would have done, so a handler wrapping some other table would describe
// routes that do not exist.
func methodFallback(mux *http.ServeMux, notFound http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allow, route := allowedMethods(mux, r)
		if len(allow) == 0 {
			// Nothing in the table wants this path under any verb. This is the
			// honest 404 and it must stay one — answering 405 here would tell
			// a client that a typo'd URL is a real resource.
			notFound.ServeHTTP(w, r)
			return
		}

		// RFC 9110 makes Allow mandatory on 405 and recommends it on the
		// OPTIONS response; it is set before the body is written because
		// writeError commits the header.
		joined := strings.Join(allow, ", ")
		w.Header().Set("Allow", joined)

		if r.Method == http.MethodOptions {
			// A discovery request, not an error. 204 keeps it out of the 4xx
			// rate that an operator reads as clients getting things wrong.
			//
			// No Content-Length: RFC 9110 forbids one on a 204, and net/http
			// strips it anyway, so setting it only misleads whoever reads this
			// next into thinking it does something.
			w.WriteHeader(http.StatusNoContent)
			return
		}

		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			"this path exists but does not accept "+r.Method+"; it accepts "+joined,
			map[string]any{
				"path":   route,
				"method": r.Method,
				"allow":  allow,
			})
	})
}

// apiFallback is what /api/v1/ is mounted with: the JSON 404 this package
// already emitted, now preceded by the method check.
//
// It takes the mux rather than reaching for one because Handler() builds the
// table locally and the catch-all has to be registered on that same table
// while it is being assembled.
//
// The authentication is INSIDE this function rather than left to the caller,
// which is the difference between a documented rule and an enforced one. The
// catch-all it replaces was already behind requireRole because an
// unauthenticated 404 map is a free inventory of the control plane; what this
// function adds is an answer that also names the verbs, which is a better
// inventory. Mounting it is now a single expression that cannot be wired
// without its credential check:
//
//	mux.Handle("/api/v1/", s.apiFallback(mux))
func (s *Server) apiFallback(mux *http.ServeMux) http.Handler {
	return s.requireRole(RoleTenant, methodFallback(mux, http.HandlerFunc(s.handleAPINotFound)))
}
