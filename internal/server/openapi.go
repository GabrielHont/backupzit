package server

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

//go:embed openapi.json
var publicOpenAPI json.RawMessage

func (s *Server) registerAPIDocs(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/openapi.json", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return publicOpenAPI, nil
	}))
}
