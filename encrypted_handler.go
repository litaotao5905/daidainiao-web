package main

import (
	"encoding/json"
	"net/http"
)

type EncryptedReplyReq struct {
	Prompt       string   `json:"prompt"`
	Attachments  []string `json:"attachments,omitempty"`
	ContextLimit int      `json:"context_limit,omitempty"`
}

type EncryptedSettingsReq struct {
	ContextLimit int `json:"context_limit"`
}

func (s *Server) handleEncryptedThread(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")
	if !threadRe.MatchString(threadID) {
		writeJSON(w, 400, apiError{Error: "thread_id 非法"})
		return
	}

	thread, msgs, err := s.svc.GetEncryptedThread(r.Context(), threadID)
	if err != nil {
		writeErr(w, err)
		return
	}

	writeJSON(w, 200, map[string]any{
		"thread":   thread,
		"messages": msgs,
	})
}

func (s *Server) handleEncryptedReply(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")
	if !threadRe.MatchString(threadID) {
		writeJSON(w, 400, apiError{Error: "thread_id 非法"})
		return
	}

	var req EncryptedReplyReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, apiError{Error: "请求体不是有效的 JSON"})
		return
	}

	msg, err := s.svc.SendEncryptedTurn(r.Context(), threadID, req.Prompt, req.Attachments, req.ContextLimit)
	if err != nil {
		writeErr(w, err)
		return
	}

	writeJSON(w, 200, map[string]any{
		"message": msg,
	})
}

func (s *Server) handleEncryptedSettings(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")
	if !threadRe.MatchString(threadID) {
		writeJSON(w, 400, apiError{Error: "thread_id 非法"})
		return
	}

	var req EncryptedSettingsReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(&req); err != nil {
		writeJSON(w, 400, apiError{Error: "请求体不是有效的 JSON"})
		return
	}

	if s.svc.encryptedStore == nil {
		s.svc.encryptedStore = NewEncryptedStore(s.cfg.DataDir)
	}

	if err := s.svc.encryptedStore.UpdateContextLimit(threadID, req.ContextLimit); err != nil {
		writeErr(w, err)
		return
	}

	writeJSON(w, 200, map[string]any{
		"ok":            true,
		"context_limit": req.ContextLimit,
	})
}

func (s *Server) handleEncryptedSync(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")
	if !threadRe.MatchString(threadID) {
		writeJSON(w, 400, apiError{Error: "thread_id 非法"})
		return
	}

	thread, msgs, err := s.svc.SyncEncryptedDoc(r.Context(), threadID)
	if err != nil {
		writeErr(w, err)
		return
	}

	writeJSON(w, 200, map[string]any{
		"thread":   thread,
		"messages": msgs,
	})
}

func (s *Server) handleEncryptedPermissions(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")
	if !threadRe.MatchString(threadID) {
		writeJSON(w, 400, apiError{Error: "thread_id 非法"})
		return
	}

	snap, err := s.svc.AuditEncryptedPermissions(r.Context(), threadID)
	if err != nil {
		writeJSON(w, 403, map[string]any{
			"valid": false,
			"error": err.Error(),
		})
		return
	}

	writeJSON(w, 200, map[string]any{
		"valid":       true,
		"permissions": snap,
	})
}
