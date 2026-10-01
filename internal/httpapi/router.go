package httpapi

import (
	"encoding/json"
	"net/http"
)

// Option configures the router.
type Option func(*http.ServeMux)

func NewRouter(options ...Option) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(struct {
			Status string `json:"status"`
		}{Status: "ok"})
	})
	for _, option := range options {
		option(mux)
	}
	return mux
}
