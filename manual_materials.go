package main

import (
	"encoding/json"
	"io"
	"net/http"
)

// Only an explicit click expands the selected folder. Other topic resources
// are left alone, and discoveries are catalogued without fetching their bytes.
func (s *Server) handleExpandMaterials(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		writeJSON(w, 403, apiError{Error: "请刷新页面后重试"})
		return
	}
	if !s.svc.auth.LoggedIn() {
		writeErr(w, ErrLoginRequired)
		return
	}
	id := r.PathValue("thread_id")
	if !threadRe.MatchString(id) {
		writeJSON(w, 400, apiError{Error: "话题标识无效"})
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil {
		writeJSON(w, 400, apiError{Error: "文件夹标识无效"})
		return
	}
	tr, err := s.svc.Thread(r.Context(), id, false)
	if err != nil {
		writeErr(w, err)
		return
	}
	var target *Attachment
	for _, a := range tr.Attachments {
		if a.ResourceType == "folder" && a.ResourceToken == req.Token {
			item := a
			target = &item
			break
		}
	}
	if target == nil {
		writeJSON(w, 404, apiError{Error: "当前话题没有这个文件夹"})
		return
	}
	uid := s.svc.auth.UserID()
	atts, warnings := s.svc.resolveTopicResources(r.Context(), []Attachment{*target})
	if s.svc.auth.UserID() != uid {
		writeErr(w, ErrLoginRequired)
		return
	}
	tc := topicContext(tr)
	s.svc.fileMu.Lock()
	if rec := s.svc.fileJobs[s.svc.k("files:"+tc.TopicKey)]; rec != nil {
		rec.attachments = mergeTopicAttachments(rec.attachments, atts)
		rec.state = TopicFileSync{Status: "manual", Warnings: warnings}
	}
	s.svc.fileMu.Unlock()
	tr = s.svc.registerTopicFiles(r.Context(), tr)
	writeJSON(w, 200, tr)
}
