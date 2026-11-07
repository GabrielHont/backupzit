package server

import "net/http"

func (s *Server) registerAPITargets(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/targets", s.api(PermStorage, func(w http.ResponseWriter, r *http.Request) (any, error) {
		req := struct {
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			Endpoint  string `json:"endpoint"`
			Bucket    string `json:"bucket"`
			Prefix    string `json:"prefix"`
			Region    string `json:"region"`
			AccessKey string `json:"accessKey"`
			SecretKey string `json:"secretKey"`
			TLS       bool   `json:"tls"`
			LockDays  int    `json:"lockDays"`
		}{Kind: "s3", TLS: true}
		if err := readAPIJSON(r, &req); err != nil {
			return nil, err
		}
		if req.Kind != "s3" {
			return nil, apiErr(400, "only S3 targets can be created through this endpoint")
		}
		location, err := s3Location(req.Endpoint, req.Bucket, req.Prefix, !req.TLS)
		if err != nil {
			return nil, mutationAPIError(err, false)
		}
		t := Target{Name: req.Name, Kind: "s3", URL: location, S3Region: req.Region, S3AccessKey: req.AccessKey, S3SecretKey: req.SecretKey, S3LockDays: req.LockDays, Encrypted: true}
		id, key, err := s.createStorageTarget(r, t)
		if err != nil {
			return nil, mutationAPIError(err, false)
		}
		// Credentials never enter this response. The recovery key is returned
		// only here, not by list/detail endpoints or audit entries.
		return apiResponse{201, map[string]any{"id": id, "name": t.Name, "kind": t.Kind, "location": location, "encrypted": true, "immutable_days": t.S3LockDays, "recoveryKey": key}}, nil
	}))
	mux.HandleFunc("DELETE /api/v1/targets/{id}", s.api(PermStorage, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		if err := s.deleteStorageTarget(r, id); err != nil {
			return nil, mutationAPIError(err, true)
		}
		return apiResponse{204, nil}, nil
	}))
}
