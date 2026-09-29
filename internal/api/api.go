// Package api exposes the HTTP sync API.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"notes-server/internal/model"
	"notes-server/internal/store"
)

const (
	maxBody        = 16 << 20 // 16 MiB per sync request
	maxContent     = 1 << 20  // 1 MiB per note
	maxIDLen       = 64
	maxColorLen    = 32
	pageSize       = 500
	maxChangesSent = 5000
)

type Server struct {
	store   *store.Store
	version string
}

func New(s *store.Store, version string) *Server {
	return &Server{store: s, version: version}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.Handle("POST /api/v1/sync", s.auth(http.HandlerFunc(s.sync)))
	return logRequests(mux)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.version})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || secret == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		valid, err := s.store.Authenticate(r.Context(), secret)
		if err != nil {
			log.Printf("auth: %v", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !valid {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	var req model.SyncRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := validate(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := s.store.Sync(r.Context(), req.Cursor, req.Changes, pageSize)
	if err != nil {
		log.Printf("sync: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func validate(req model.SyncRequest) error {
	if req.Cursor < 0 {
		return errors.New("cursor must be >= 0")
	}
	if len(req.Changes) > maxChangesSent {
		return fmt.Errorf("too many changes (max %d per request)", maxChangesSent)
	}
	for i, n := range req.Changes {
		switch {
		case n.ID == "" || len(n.ID) > maxIDLen:
			return fmt.Errorf("changes[%d]: id must be 1-%d chars", i, maxIDLen)
		case n.UpdatedAt <= 0:
			return fmt.Errorf("changes[%d]: updated_at is required", i)
		case len(n.Content) > maxContent:
			return fmt.Errorf("changes[%d]: content exceeds %d bytes", i, maxContent)
		case len(n.Color) > maxColorLen:
			return fmt.Errorf("changes[%d]: color too long", i)
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, r.RemoteAddr)
	})
}
