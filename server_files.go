package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// csrfHeader must carry the per-process token on every call that changes
// local state or launches a program.
const csrfHeader = "X-FT-CSRF"

func newCSRFToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// openerEnabled: launching programs only makes sense when the server runs on
// the user's own desktop (not in Docker) and was not switched off.
func (s *Server) openerEnabled() bool {
	if s.opener == nil || s.noOpen {
		return false
	}
	return true
}

func isLoopbackAddr(remote string) bool {
	h, _, err := net.SplitHostPort(remote)
	if err != nil {
		h = remote
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

func isLocalHostHeader(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// checkCSRF validates the custom header (a cross-site page cannot send it
// without a CORS preflight, which this server never grants), a same-origin
// Origin and Fetch-Metadata.
func (s *Server) checkCSRF(r *http.Request) bool {
	tok := r.Header.Get(csrfHeader)
	if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(s.csrf)) != 1 {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || u.Host != r.Host {
			return false
		}
	}
	if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
		return false
	}
	return true
}

// localOnly gates actions that launch local programs: loopback client, a
// localhost Host header (ALLOW_ANY_HOST does not relax this) and the CSRF token.
func (s *Server) localOnly(w http.ResponseWriter, r *http.Request) bool {
	if (!isLoopbackAddr(r.RemoteAddr) && !s.docker) || !isLocalHostHeader(r.Host) {
		writeJSON(w, http.StatusForbidden, apiError{Error: "只允许在本机通过 localhost 访问此功能"})
		return false
	}
	if !s.checkCSRF(r) {
		writeJSON(w, http.StatusForbidden, apiError{Error: "缺少或错误的 CSRF 令牌，请刷新页面后重试"})
		return false
	}
	return true
}

func (s *Server) handleCSRF(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"token": s.csrf})
}

type downloadsResp struct {
	FileSync    *TopicFileSync `json:"file_sync,omitempty"`
	Attachments []Attachment   `json:"attachments,omitempty"`
	Enabled     bool           `json:"enabled"`
	Dir         string         `json:"dir"`
	Opener      bool           `json:"opener"`
	Docker      bool           `json:"docker,omitempty"`
	Items       []DLEntry      `json:"items"`
}

func (s *Server) dlInfo(items []DLEntry) downloadsResp {
	dm := s.svc.dl
	if dm == nil {
		return downloadsResp{Items: []DLEntry{}}
	}
	if items == nil {
		items = []DLEntry{}
	}
	dir := dm.Base()
	if dm.hostBase != "" {
		dir = dm.hostBase
	}
	return downloadsResp{Enabled: true, Dir: dir, Opener: s.openerEnabled(), Docker: s.docker, Items: items}
}

func (s *Server) handleDownloads(w http.ResponseWriter, r *http.Request) {
	if !s.svc.auth.LoggedIn() {
		writeErr(w, ErrLoginRequired)
		return
	}
	if s.svc.dl == nil {
		writeJSON(w, 200, s.dlInfo(nil))
		return
	}
	topic := r.URL.Query().Get("topic")
	resp := s.dlInfo(nil)
	if topic != "" {
		s.svc.fileMu.Lock()
		if rec := s.svc.fileJobs[s.svc.k("files:"+topic)]; rec != nil {
			snap := rec.state
			snap.Warnings = append([]string{}, rec.state.Warnings...)
			resp.FileSync = &snap
			resp.Attachments = append([]Attachment{}, rec.attachments...)
		}
		s.svc.fileMu.Unlock()
	}
	resp.Items = s.svc.dl.List(topic)
	writeJSON(w, 200, resp)
}

type fileReq struct {
	ID string `json:"id"`
}

func readFileReq(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req fileReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || !fileIDRe.MatchString(req.ID) {
		writeJSON(w, 400, apiError{Error: "需要文件 id"})
		return "", false
	}
	return req.ID, true
}

func (s *Server) handleFileOpen(w http.ResponseWriter, r *http.Request)   { s.fileAction(w, r, false) }
func (s *Server) handleFileReveal(w http.ResponseWriter, r *http.Request) { s.fileAction(w, r, true) }

func (s *Server) fileAction(w http.ResponseWriter, r *http.Request, reveal bool) {
	if !s.localOnly(w, r) {
		return
	}
	if !s.svc.auth.LoggedIn() {
		writeErr(w, ErrLoginRequired)
		return
	}
	if !s.openerEnabled() || s.svc.dl == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Error: "当前运行环境（如 Docker）无法打开宿主机程序；请用「复制文件路径」，或在宿主机上直接运行本程序"})
		return
	}
	id, ok := readFileReq(w, r)
	if !ok {
		return
	}
	e, p, err := s.svc.dl.Resolve(id)
	if err != nil {
		code := http.StatusConflict
		switch {
		case errors.Is(err, errNoSuchFile):
			code = http.StatusNotFound
		case errors.Is(err, ErrOutsideDir):
			code = http.StatusForbidden
		}
		writeJSON(w, code, apiError{Error: err.Error()})
		return
	}
	target := p
	if s.opener.HostURL != "" && e.Path != "" {
		target = e.Path
	}
	if reveal {
		err = s.opener.Reveal(target)
	} else {
		err = s.opener.Open(target)
	}
	if errors.Is(err, ErrExecutable) {
		writeJSON(w, http.StatusForbidden, apiError{Error: err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: "无法启动系统程序：" + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleFileRedownload(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		writeJSON(w, http.StatusForbidden, apiError{Error: "缺少或错误的 CSRF 令牌，请刷新页面后重试"})
		return
	}
	if !s.svc.auth.LoggedIn() {
		writeErr(w, ErrLoginRequired)
		return
	}
	id, ok := readFileReq(w, r)
	if !ok {
		return
	}
	if s.svc.dl == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Error: "已关闭文件下载（disable_download）"})
		return
	}
	if err := s.svc.dl.Redownload(id); err != nil {
		writeJSON(w, http.StatusNotFound, apiError{Error: err.Error()})
		return
	}
	e, _ := s.svc.dl.Get(id)
	writeJSON(w, 200, e)
}

// handleFileContent serves a downloaded file from the local copy.
func (s *Server) handleFileContent(w http.ResponseWriter, r *http.Request) {
	if !s.svc.auth.LoggedIn() {
		writeErr(w, ErrLoginRequired)
		return
	}
	id := r.PathValue("id")
	if s.svc.dl == nil || !fileIDRe.MatchString(id) {
		writeJSON(w, 404, apiError{Error: "没有这个文件"})
		return
	}
	e, p, err := s.svc.dl.Resolve(id)
	if err != nil {
		writeJSON(w, 404, apiError{Error: err.Error()})
		return
	}
	s.serveFile(w, r, p, e.Name, e.Mime)
}

// serveFile streams a regular local file with a safe content type: only
// known-inert types are shown inline, text is always text/plain.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, path, name, ctype string) {
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	ct := contentTypeFor(ctype, name, func() string { return sniffMime(path) })
	inline := inlineTypes[ct] && r.URL.Query().Get("dl") != "1"
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	if name == "" {
		name = "download"
	}
	w.Header().Set("Content-Type", ct)
	if strings.HasPrefix(ct, "text/") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// dlFetch is the DownloadManager's fetcher. Messages resources use the
// existing logic (resource_auth: app identity and/or user identity, with the
// cache in data/cache/res); Drive files use the user identity.
func (s *Server) dlFetch(ctx context.Context, e DLEntry, force bool) (*dlFetched, error) {
	if strings.HasPrefix(e.ResType, "export:") {
		return s.exportDocument(ctx, e)
	}
	if e.ResType == "drive" {
		// Read the version before and after streaming; never label old bytes with a newer version.
		before, beforeErr := s.svc.api.DriveMeta(ctx, e.FileKey)
		d, err := s.svc.api.DriveDownload(ctx, e.FileKey)
		if err != nil {
			return nil, err
		}
		defer d.Body.Close()
		if err := os.MkdirAll(s.resDir, 0o700); err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(s.resDir, ".drv-*")
		if err != nil {
			return nil, err
		}
		_, copyErr := copyWithDownloadProgress(ctx, tmp, d.Body, d.Size)
		closeErr := tmp.Close()
		if copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			os.Remove(tmp.Name())
			return nil, copyErr
		}
		after, afterErr := s.svc.api.DriveMeta(ctx, e.FileKey)
		var mod int64
		if beforeErr == nil && afterErr == nil && before != nil && after != nil {
			if before.LatestModifyTime != after.LatestModifyTime {
				os.Remove(tmp.Name())
				return nil, fmt.Errorf("下载期间云端文件发生更新，请重试")
			}
			mod = after.LatestModifyTime
		}
		return &dlFetched{Path: tmp.Name(), Name: d.Filename, Mime: d.ContentType, ModTime: mod, Cleanup: func(bool) { os.Remove(tmp.Name()) }}, nil
	}
	base := resBase(s.cfg.DataDir, e.MessageID, e.FileKey, e.ResType)
	if force {
		_ = os.Remove(base + ".bin")
		_ = os.Remove(base + ".json")
		s.failMu.Lock()
		delete(s.resFail, base)
		s.failMu.Unlock()
	}
	meta, err := s.ensureResource(ctx, base, e.MessageID, e.FileKey, e.ResType)
	if err != nil {
		return nil, err
	}
	// the cache copy is dropped once the file is safely in the download folder
	return &dlFetched{Path: base + ".bin", Name: meta.Filename, Mime: meta.ContentType, Cleanup: func(ok bool) {
		if ok {
			_ = os.Remove(base + ".bin")
			_ = os.Remove(base + ".json")
		}
	}}, nil
}
