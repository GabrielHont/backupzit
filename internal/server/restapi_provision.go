package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

// Public mutations deliberately reject unknown keys and trailing JSON so
// misspelled limits or retention settings cannot silently be ignored.
func readAPIJSON(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); !strings.EqualFold(strings.TrimSpace(strings.Split(ct, ";")[0]), "application/json") {
		return apiErr(http.StatusUnsupportedMediaType, "Content-Type must be application/json")
	}
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	var raw json.RawMessage
	if err := d.Decode(&raw); err != nil {
		return apiErr(http.StatusBadRequest, "invalid JSON request: %s", err)
	}
	if len(raw) == 0 || raw[0] != '{' {
		return apiErr(400, "request must be a JSON object")
	}
	if err := rejectNullScalars(raw, reflect.TypeOf(dst)); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return apiErr(http.StatusBadRequest, "request must contain exactly one JSON object")
	}
	fields := json.NewDecoder(strings.NewReader(string(raw)))
	fields.DisallowUnknownFields()
	if err := fields.Decode(dst); err != nil {
		return apiErr(400, "invalid JSON request: %s", err)
	}
	return nil
}

// encoding/json treats null scalar fields as a no-op, which is unsafe for
// prefilled defaults such as ttlHours or TLS. Nullable pointers/slices and
// explicit RawMessage union fields retain their normal JSON semantics.
func rejectNullScalars(raw json.RawMessage, t reflect.Type) error {
	if t == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	if string(raw) == "null" {
		switch t.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface:
			return nil
		}
		return apiErr(400, "null is not allowed for %s", t)
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return nil
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			for k, v := range fields {
				if strings.EqualFold(k, name) {
					if err := rejectNullScalars(v, f.Type); err != nil {
						return apiErr(400, "%s: %s", name, err)
					}
				}
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return nil
		}
		for _, v := range values {
			if err := rejectNullScalars(v, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) newEnrollment(r *http.Request, ttl time.Duration, maxUses *int64, label string) (EnrollmentToken, string, error) {
	t, secret, err := s.store.createEnrollmentToken(r.Context(), ttl, maxUses, label)
	if err == nil {
		s.audit(r, "enrollment_token.create", "token %d, label %q", t.ID, t.Label)
	}
	return t, secret, err
}

func (s *Server) registerAPIProvision(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/enrollment-tokens", s.api(PermAgents, func(w http.ResponseWriter, r *http.Request) (any, error) {
		req := struct {
			MaxUses  *int64 `json:"maxUses"`
			TTLHours int    `json:"ttlHours"`
			Label    string `json:"label"`
		}{MaxUses: new(int64(1)), TTLHours: 24}
		if err := readAPIJSON(r, &req); err != nil {
			return nil, err
		}
		if req.TTLHours < 1 || req.TTLHours > 8760 {
			return nil, apiErr(400, "ttlHours must be between 1 and 8760")
		}
		if req.MaxUses != nil && *req.MaxUses < 1 {
			return nil, apiErr(400, "maxUses must be positive or null (unlimited)")
		}
		if len(strings.TrimSpace(req.Label)) > 255 || strings.ContainsRune(req.Label, 0) {
			return nil, apiErr(400, "label must be at most 255 bytes and contain no NUL")
		}
		if !strings.HasPrefix(s.CertFingerprint, "SHA256:") {
			return nil, apiErr(409, "enrollment requires the console TLS certificate")
		}
		t, secret, err := s.newEnrollment(r, time.Duration(req.TTLHours)*time.Hour, req.MaxUses, req.Label)
		if err != nil {
			return nil, err
		}
		return apiResponse{201, struct {
			EnrollmentToken
			Code string `json:"code"`
		}{t, api.EncodeEnrollCode(s.publicURL(r), secret, s.CertFingerprint)}}, nil
	}))
	mux.HandleFunc("GET /api/v1/enrollment-tokens", s.api(PermAgents, func(w http.ResponseWriter, r *http.Request) (any, error) {
		list, err := s.store.listEnrollmentTokens(r.Context())
		if list == nil {
			list = []EnrollmentToken{}
		}
		return list, err
	}))
	mux.HandleFunc("DELETE /api/v1/enrollment-tokens/{id}", s.api(PermAgents, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		if err := s.store.revokeEnrollmentToken(r.Context(), id); err != nil {
			return nil, err
		}
		s.audit(r, "enrollment_token.revoke", "token %d", id)
		return apiResponse{204, nil}, nil
	}))
	mux.HandleFunc("GET /api/v1/limits", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.store.consoleLimits(r.Context())
	}))
	mux.HandleFunc("PUT /api/v1/limits", s.api(PermSettings, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req map[string]json.RawMessage
		if err := readAPIJSON(r, &req); err != nil {
			return nil, err
		}
		// maxAgents always; storage (Unirea: the firm's backup space) when the
		// portal sends a measurement with it.
		_, hasStorage := req["storage"]
		if req["maxAgents"] == nil || len(req) > 2 || (len(req) == 2 && !hasStorage) {
			return nil, apiErr(400, "provide maxAgents (integer or null) and optionally storage")
		}
		var l ConsoleLimits
		if err := json.Unmarshal(req["maxAgents"], &l.MaxAgents); err != nil || (l.MaxAgents != nil && *l.MaxAgents < 0) {
			return nil, apiErr(400, "maxAgents must be non-negative or null (unlimited)")
		}
		var storage StorageLimit
		if hasStorage {
			dec := json.NewDecoder(bytes.NewReader(req["storage"]))
			dec.DisallowUnknownFields()
			// Every field is required: a missing limit must not read as unlimited.
			var keys map[string]json.RawMessage
			if json.Unmarshal(req["storage"], &keys) != nil || keys["limitBytes"] == nil || keys["usedBytes"] == nil || keys["measuredAt"] == nil {
				return nil, apiErr(400, "storage must be {limitBytes, usedBytes, measuredAt}")
			}
			var in struct {
				LimitBytes *int64    `json:"limitBytes"`
				UsedBytes  *int64    `json:"usedBytes"`
				MeasuredAt time.Time `json:"measuredAt"`
				Full       *bool     `json:"full"` // read only: ignored
			}
			if err := dec.Decode(&in); err != nil || in.UsedBytes == nil {
				return nil, apiErr(400, "storage must be {limitBytes, usedBytes, measuredAt}")
			}
			storage = StorageLimit{LimitBytes: in.LimitBytes, UsedBytes: *in.UsedBytes, MeasuredAt: in.MeasuredAt}
			if err := storage.valid(); err != nil {
				return nil, apiErr(400, "%s", err.Error())
			}
		}
		before, _ := s.store.consoleLimits(r.Context())
		if err := s.store.setConsoleLimits(r.Context(), l); err != nil {
			return nil, err
		}
		// The portal sends the limits with every measurement: only changes go to the audit log.
		if !sameLimit(before.MaxAgents, l.MaxAgents) {
			s.audit(r, "limits.update", "maxAgents=%s", req["maxAgents"])
		}
		if hasStorage {
			if err := s.store.setStorageLimit(r.Context(), storage); err != nil {
				return nil, err
			}
			after, err := s.store.storageLimit(r.Context())
			if err != nil {
				return nil, err
			}
			if after != nil && (before.Storage == nil || before.Storage.Full != after.Full) {
				s.audit(r, "limits.storage", "full=%t used=%d limit=%s", after.Full, after.UsedBytes, req["storage"])
			}
		}
		return s.store.consoleLimits(r.Context())
	}))
}

func sameLimit(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
