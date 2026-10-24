package mcpserver

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPOptions contains HTTP access controls. Tokens are never accepted in URLs.
type HTTPOptions struct {
	Token          string
	AllowedOrigins []string
}

// HTTPHandler serves exactly /mcp and /mcp/ without redirects. /healthz only
// reports process liveness; it does not probe iCloud or reveal account state.
func HTTPHandler(server *mcp.Server, options HTTPOptions) http.Handler {
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/healthz" {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
			return
		}
		if r.URL.Path != "/mcp" && r.URL.Path != "/mcp/" {
			http.NotFound(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			allowed := false
			for _, entry := range options.AllowedOrigins {
				if origin == entry {
					allowed = true
					break
				}
			}
			if !allowed {
				http.Error(w, "Origin not allowed", http.StatusForbidden)
				return
			}
		}
		if options.Token != "" {
			provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(provided), []byte(options.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
		transport.ServeHTTP(w, r)
	})
}
