package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	workspacePreviewFolder = "_preview_cache"
	workspaceStagingFolder = "_workspace_staging"
)

type workspaceSource struct {
	Kind          string `json:"kind"`
	MessageID     string `json:"message_id"`
	FileKey       string `json:"file_key"`
	ResType       string `json:"res_type"`
	ResourceType  string `json:"resource_type"`
	ResourceToken string `json:"resource_token"`
	URL           string `json:"url"`
	Name          string `json:"name"`
	DLID          string `json:"dl_id"`
}

type workspaceFileRequest struct {
	ChatID  string `json:"chat_id"`
	ItemID  string `json:"item_id"`
	RelPath string `json:"rel_path"`
}

type workspaceTransferRequest struct {
	ChatID  string          `json:"chat_id"`
	ItemID  string          `json:"target_item_id"`
	Node    string          `json:"target_node"`
	Subpath string          `json:"target_subpath"`
	Path    []string        `json:"target_path"`
	Source  workspaceSource `json:"source"`
}

type workspaceTransferResult struct {
	Name    string `json:"name"`
	Token   string `json:"token,omitempty"`
	URL     string `json:"url,omitempty"`
	RelPath string `json:"rel_path,omitempty"`
}

func (src workspaceSource) cloudRef() (DriveRef, error) {
	switch src.Kind {
	case "im", "local_dl":
		return DriveRef{}, nil
	case "drive", "doc", "wiki":
	default:
		return DriveRef{}, badRequest("文件来源类型无效")
	}
	if ref, ok := driveRef(src.URL); ok {
		return ref, nil
	}
	kind, token := src.ResourceType, firstNonEmpty(src.ResourceToken, src.FileKey)
	if src.Kind == "wiki" {
		kind = "wiki"
	}
	if kind == "" && src.Kind == "drive" {
		kind = "file"
	}
	if workspaceResourceURL("", kind, token) == "" {
		return DriveRef{}, badRequest("缺少有效的飞书资源标识")
	}
	return DriveRef{Type: kind, Token: token}, nil
}

func (src workspaceSource) imRef() (string, error) {
	if !msgIDRe.MatchString(src.MessageID) || !keyRe.MatchString(src.FileKey) {
		return "", badRequest("缺少有效的会话附件标识")
	}
	switch src.ResType {
	case "image":
		return "image", nil
	case "", "file", "audio", "video", "media":
		return "file", nil
	}
	return "", badRequest("会话附件类型无效")
}

func openWorkspaceFile(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, ErrOutsideDir
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) {
		f.Close()
		return nil, nil, firstError(err, ErrOutsideDir)
	}
	return f, info, nil
}

func firstError(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func (s *Server) workspaceBoundFile(w http.ResponseWriter, r *http.Request, uid string, req workspaceFileRequest) (string, bool) {
	if !s.localOnly(w, r) {
		return "", false
	}
	item, err := s.svc.workspaceItem(uid, req.ChatID, req.ItemID)
	if err == nil && item.Kind != workspaceLocal {
		err = badRequest("请选择已绑定的本地文件")
	}
	if err == nil {
		var path string
		path, err = s.svc.workspaceLocalPath(item.LocalPath, req.RelPath, false)
		if err == nil {
			return path, true
		}
	}
	writeWorkspaceErr(w, err)
	return "", false
}

func (s *Server) handleWorkspaceStage(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.workspaceUser(w, r)
	if !ok {
		return
	}
	var req workspaceFileRequest
	if !decodeWorkspace(w, r, &req) {
		return
	}
	path, ok := s.workspaceBoundFile(w, r, uid, req)
	if !ok {
		return
	}
	f, _, err := openWorkspaceFile(path)
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	defer f.Close()
	staged, err := s.svc.Stage(filepath.Base(req.RelPath), f)
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, staged)
}

func (s *Server) handleWorkspaceReveal(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.workspaceUser(w, r)
	if !ok {
		return
	}
	var req workspaceFileRequest
	if !decodeWorkspace(w, r, &req) {
		return
	}
	path, ok := s.workspaceBoundFile(w, r, uid, req)
	if !ok {
		return
	}
	if !s.openerEnabled() {
		writeJSON(w, http.StatusForbidden, apiError{Error: "当前环境无法打开系统文件管理器"})
		return
	}
	hostPath := s.svc.workspaceOpenerHostPath(path)
	if hostPath != path {
		path = hostPath
	} else if s.docker && s.opener.HostURL != "" && s.cfg.HostDownloadDir == "" && !s.svc.workspaceMappingEnabled() {
		writeJSON(w, http.StatusForbidden, apiError{Error: "该服务端目录没有映射到宿主机"})
		return
	}
	if err := s.opener.Reveal(path); err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Service) workspaceDownloadEntry(id, chatID string) (DLEntry, error) {
	if s.dl == nil || !fileIDRe.MatchString(id) {
		return DLEntry{}, errNoSuchFile
	}
	e, ok := s.dl.Get(id)
	if !ok {
		return e, errNoSuchFile
	}
	if chatID != "" && e.ChatID != chatID {
		return e, ErrOutsideDir
	}
	return e, nil
}

// workspaceDownload streams local and remote bytes through the same transfer path.
func (s *Service) workspaceDownload(ctx context.Context, src workspaceSource, ref DriveRef, chatID string, useLocal bool) (*Download, error) {
	if src.Kind == "local_dl" {
		e, err := s.workspaceDownloadEntry(src.DLID, chatID)
		if err != nil {
			return nil, err
		}
		if e.Status == dlDone {
			return s.workspaceDownloadedCopy(e)
		}
		src.MessageID, src.FileKey, src.ResType = e.MessageID, e.FileKey, e.ResType
		src.Name = firstNonEmpty(src.Name, e.Name, e.NameHint)
		if e.ResType == "drive" {
			ref = DriveRef{Type: "file", Token: e.FileKey}
		} else if kind, ok := strings.CutPrefix(e.ResType, "export:"); ok {
			ref = DriveRef{Type: kind, Token: e.FileKey}
			if ref.Type == "wiki" {
				n, err := s.api.WikiNodeInfo(ctx, ref.Token)
				if err != nil {
					return nil, err
				}
				ref = DriveRef{Type: n.ObjType, Token: n.ObjToken}
			}
		}
	}
	if ref.Type != "" && ref.Type != "file" {
		if exportExtension(ref.Type) == "" {
			return nil, badRequest("该飞书资源暂不支持导出为 Office，请在飞书中打开")
		}
		return s.api.DriveExport(ctx, ref.Token, ref.Type)
	}
	resType, fileKey := "drive", ref.Token
	if ref.Type == "" {
		var err error
		resType, err = src.imRef()
		if err != nil {
			return nil, err
		}
		fileKey = src.FileKey
	}
	if useLocal && s.dl != nil {
		if e, found := s.dl.workspaceCopy(chatID, resType, src.MessageID, fileKey); found {
			if copy, err := s.workspaceDownloadedCopy(e); err == nil {
				return copy, nil
			}
		}
	}
	if ref.Type == "file" {
		return s.api.DriveDownload(ctx, ref.Token)
	}
	return s.api.Download(ctx, src.MessageID, src.FileKey, resType)
}

func (s *Service) workspaceDownloadedCopy(e DLEntry) (*Download, error) {
	path, err := s.dl.safePath(e.Rel)
	if err != nil {
		return nil, err
	}
	f, info, err := openWorkspaceFile(path)
	if err != nil {
		return nil, err
	}
	return &Download{Body: f, Filename: firstNonEmpty(e.Name, filepath.Base(path)), ContentType: e.Mime, Size: info.Size()}, nil
}

func workspaceFilename(src workspaceSource, d *Download, office string) string {
	name := strings.TrimSpace(src.Name)
	if strings.Contains(name, "://") {
		name = ""
	}
	if office != "" {
		name = firstNonEmpty(name, d.Filename, "飞书文档")
		if !strings.EqualFold(filepath.Ext(name), "."+office) {
			if ext := filepath.Ext(name); ext != "" && strings.Contains(" .doc .docx .xls .xlsx .ppt .pptx ", " "+strings.ToLower(ext)+" ") {
				name = strings.TrimSuffix(name, ext)
			}
			name += "." + office
		}
	} else {
		name = firstNonEmpty(name, d.Filename, "文件")
		if filepath.Ext(name) == "" {
			name += firstNonEmpty(filepath.Ext(d.Filename), extForMime(d.ContentType))
		}
	}
	return sanitizeName(name, 120)
}

func (s *Service) uploadWorkspaceDownload(ctx context.Context, folder, name string, d *Download) (string, error) {
	if d.Size > 0 {
		return s.api.DriveUploadToFolder(ctx, folder, name, d.Body, d.Size)
	}
	// Drive needs an exact size before upload. Spool bodies without a length.
	tmp, err := os.CreateTemp(s.cfg.DataDir, ".workspace-upload-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, workspaceReader{ctx: ctx, reader: d.Body})
	if err != nil {
		return "", err
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return s.api.DriveUploadToFolder(ctx, folder, name, tmp, size)
}

type workspaceReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r workspaceReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s *Server) handleWorkspaceTransfer(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.workspaceUser(w, r)
	if !ok {
		return
	}
	var req workspaceTransferRequest
	if !decodeWorkspace(w, r, &req) {
		return
	}
	item, err := s.svc.workspaceItem(uid, req.ChatID, req.ItemID)
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	if (item.Kind == workspaceLocal || req.Source.Kind == "local_dl") && !s.localOnly(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(workspaceContext(r, uid), 4*time.Minute)
	defer cancel()
	res, err := s.svc.transferWorkspace(ctx, req, item, s.localClient(r))
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Service) transferWorkspace(ctx context.Context, req workspaceTransferRequest, item WorkspaceItem, useLocal bool) (workspaceTransferResult, error) {
	var result workspaceTransferResult
	src := req.Source
	ref, err := src.cloudRef()
	if err != nil {
		return result, err
	}
	var wikiNode *WikiNodeDetail
	if ref.Type == "wiki" {
		wikiNode, err = s.api.WikiNodeInfo(ctx, ref.Token)
		if err != nil {
			return result, err
		}
		ref = DriveRef{Type: wikiNode.ObjType, Token: wikiNode.ObjToken}
		src.Name = firstNonEmpty(src.Name, wikiNode.Title)
	}
	switch item.Kind {
	case workspaceLocal:
		dir, err := s.workspaceLocalPath(item.LocalPath, req.Subpath, true)
		if err != nil {
			return result, err
		}
		d, err := s.workspaceDownload(ctx, src, ref, req.ChatID, useLocal)
		if err != nil {
			return result, err
		}
		defer d.Body.Close()
		name := workspaceFilename(src, d, exportExtension(ref.Type))
		saved, err := s.saveWorkspaceFile(ctx, item.LocalPath, req.Subpath, dir, name, d)
		if err != nil {
			return result, err
		}
		if ref.Type != "" {
			if err := s.rememberWorkspaceExport(ctx, filepath.Join(dir, saved), src, ref, wikiNode); err != nil {
				return result, err
			}
		}
		return workspaceTransferResult{Name: saved, RelPath: filepath.ToSlash(filepath.Join(req.Subpath, saved))}, nil
	case workspaceDrive, workspaceWiki:
		parent := ""
		folder := ""
		if item.Kind == workspaceDrive {
			folder, err = s.workspaceDriveTarget(ctx, item.Token, req.Node, req.Path)
		} else {
			parent, err = s.workspaceWikiTarget(ctx, item, req.Node)
			if err == nil {
				folder, err = s.api.DriveEnsurePrivateSubfolder(ctx, workspaceStagingFolder)
			}
		}
		if err != nil {
			return result, err
		}
		res, err := s.storeWorkspaceCloud(ctx, src, ref, req.ChatID, folder, useLocal)
		if err != nil {
			return result, err
		}
		if item.Kind == workspaceDrive {
			return workspaceTransferResult{Name: res.Name, Token: res.Token, URL: res.URL}, nil
		}
		if res.Token == ref.Token {
			return result, fmt.Errorf("飞书未返回有效的中转副本，已停止移动")
		}
		move, err := s.api.WikiMoveDocToSpace(ctx, item.SpaceID, parent, res.Type, res.Token)
		if err != nil {
			return result, fmt.Errorf("副本已放入飞书私密中转目录，迁入知识库失败或结果待确认：%w", err)
		}
		href := workspaceResourceURL(workspaceOrigin(s.cfg.BaseURL, item.URL), workspaceWiki, move.WikiToken)
		if move.Node != nil && isDocURL(move.Node.URL) {
			href = move.Node.URL
		}
		return workspaceTransferResult{Name: res.Name, Token: move.WikiToken, URL: href}, nil
	}
	return result, badRequest("工作区类型无效")
}

func (s *Service) storeWorkspaceCloud(ctx context.Context, src workspaceSource, ref DriveRef, chatID, folder string, useLocal bool) (*DriveResource, error) {
	if ref.Type != "" {
		if ref.Type != "file" && exportExtension(ref.Type) == "" {
			return nil, badRequest("该飞书资源类型暂不支持转存")
		}
		name := src.Name
		if name == "" || strings.Contains(name, "://") {
			metas, _ := s.api.DriveMetas(ctx, []DriveRef{ref})
			name = metas[ref.Type+":"+ref.Token].Name
		}
		name = sanitizeName(firstNonEmpty(name, ref.Token), 120)
		copy, err := s.api.DriveCopyFile(ctx, ref.Token, ref.Type, folder, name)
		if err != nil {
			return nil, err
		}
		copy.Type, copy.Name = firstNonEmpty(copy.Type, ref.Type), firstNonEmpty(copy.Name, name)
		return copy, nil
	}
	d, err := s.workspaceDownload(ctx, src, ref, chatID, useLocal)
	if err != nil {
		return nil, err
	}
	defer d.Body.Close()
	name := workspaceFilename(src, d, "")
	token, err := s.uploadWorkspaceDownload(ctx, folder, name, d)
	if err != nil {
		return nil, err
	}
	return &DriveResource{Token: token, Type: "file", Name: name}, nil
}

func (s *Service) saveWorkspaceFile(ctx context.Context, root, rel, dir, name string, d *Download) (string, error) {
	verified, err := s.workspaceLocalPath(root, rel, true)
	if err != nil || verified != dir {
		return "", firstError(err, ErrOutsideDir)
	}
	tmp, err := os.CreateTemp(dir, ".workspace-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	size, err := copyWithDownloadProgress(ctx, tmp, workspaceReader{ctx: ctx, reader: d.Body}, d.Size)
	closeErr := tmp.Close()
	if err != nil || closeErr != nil {
		return "", firstError(err, closeErr)
	}
	if d.Size > 0 && size != d.Size {
		return "", fmt.Errorf("实际读取文件大小与预期不符")
	}
	if verified, err = s.workspaceLocalPath(root, rel, true); err != nil || verified != dir {
		return "", firstError(err, ErrOutsideDir)
	}
	ext := filepath.Ext(name)
	for i := 0; ; i++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), i, ext)
		}
		err := publishWorkspaceFile(ctx, tmp.Name(), filepath.Join(dir, candidate))
		if !errors.Is(err, os.ErrExist) {
			return candidate, err
		}
	}
}

func publishWorkspaceFile(ctx context.Context, tmp, target string) error {
	// Hard links publish atomically without replacing an existing file.
	if err := os.Link(tmp, target); err == nil || errors.Is(err, os.ErrExist) {
		return err
	}
	// Some volumes (for example exFAT) do not support hard links.
	source, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer source.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, workspaceReader{ctx: ctx, reader: source})
	if err = firstError(err, out.Close()); err != nil {
		_ = os.Remove(target)
	}
	return err
}

type workspacePreviewRequest struct {
	workspaceFileRequest
	workspaceSource
	Source *workspaceSource `json:"source,omitempty"`
}

type workspacePreviewEntry struct {
	Token    string `json:"token"`
	URL      string `json:"url"`
	Cached   bool   `json:"cached"`
	GroupKey string `json:"group_key,omitempty"`
}

type workspacePreviewResult struct {
	URL            string `json:"url,omitempty"`
	Cached         bool   `json:"cached"`
	Source         string `json:"source,omitempty"`
	Conflict       string `json:"conflict,omitempty"`
	SourceURL      string `json:"source_url,omitempty"`
	SourceName     string `json:"source_name,omitempty"`
	RemoteModified int64  `json:"remote_modified,omitempty"`
	ExportedAt     int64  `json:"exported_at,omitempty"`
}

type workspacePreviewIndex map[string]map[string]workspacePreviewEntry

func (s *Service) workspacePreviewPath() string {
	return filepath.Join(s.cfg.DataDir, "settings", "preview_cache.json")
}

func (s *Service) workspaceCloudPreview(ctx context.Context, src workspaceSource) (string, error) {
	if isDocURL(src.URL) {
		return src.URL, nil
	}
	ref, err := src.cloudRef()
	if err != nil {
		return "", err
	}
	var href string
	switch ref.Type {
	case "file":
		href, err = s.api.DriveURL(ctx, ref.Token)
	case "wiki":
		var node *WikiNodeDetail
		node, err = s.api.WikiNodeInfo(ctx, ref.Token)
		if err == nil {
			href = node.URL
		}
	default:
		var metas map[string]DriveResource
		metas, err = s.api.DriveMetas(ctx, []DriveRef{ref})
		href = metas[ref.Type+":"+ref.Token].URL
		// The metadata endpoint does not support some resources, including folders.
		if href == "" {
			if ref.Type == "folder" {
				if _, listErr := s.api.DriveList(ctx, ref.Token, ""); listErr != nil {
					return "", listErr
				}
				err = nil
			}
			href = workspaceResourceURL(workspaceFeishuOrigin(s.cfg.BaseURL), ref.Type, ref.Token)
		}
	}
	if err != nil {
		return "", err
	}
	if !isDocURL(href) {
		return "", fmt.Errorf("飞书未返回有效的预览链接")
	}
	return href, nil
}

func (s *Server) handleWorkspacePreview(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.workspaceUser(w, r)
	if !ok {
		return
	}
	var req workspacePreviewRequest
	if !decodeWorkspace(w, r, &req) {
		return
	}
	src := req.workspaceSource
	if req.Source != nil {
		src = *req.Source
	}
	ctx, cancel := context.WithTimeout(workspaceContext(r, uid), 4*time.Minute)
	defer cancel()
	var view workspacePreviewResult
	var err error
	switch {
	case req.ItemID != "" && (src.Kind == "" || src.Kind == "local_file"):
		path, ok := s.workspaceBoundFile(w, r, uid, req.workspaceFileRequest)
		if !ok {
			return
		}
		view, err = s.svc.workspaceLocalPreview(ctx, uid, path, filepath.Base(req.RelPath))
	case src.Kind == "local_dl" || (src.Kind == "" && src.DLID != ""):
		if !s.localOnly(w, r) {
			return
		}
		var e DLEntry
		e, err = s.svc.workspaceDownloadEntry(src.DLID, req.ChatID)
		if err == nil && e.Status != dlDone {
			err = badRequest("文件尚未下载，请先下载后再预览")
		}
		if err == nil {
			var path string
			path, err = s.svc.dl.safePath(e.Rel)
			if err == nil {
				view, err = s.svc.workspaceLocalPreview(ctx, uid, path, e.Name)
			}
		}
	case src.Kind == "im" || (src.Kind == "" && src.MessageID != ""):
		var typ string
		typ, err = src.imRef()
		if err == nil {
			key := strings.Join([]string{"im", typ, src.MessageID, src.FileKey}, ":")
			var entry workspacePreviewEntry
			entry, err = s.svc.cachedWorkspacePreview(ctx, uid, key, "", func() (*Download, error) {
				d, err := s.svc.api.Download(ctx, src.MessageID, src.FileKey, typ)
				if err != nil {
					return nil, err
				}
				d.Filename = workspaceFilename(src, d, "")
				return d, nil
			}, nil)
			view = workspacePreviewResult{URL: entry.URL, Cached: entry.Cached}
		}
	default:
		if src.Kind == "" {
			src.Kind = "doc"
		}
		view.URL, err = s.svc.workspaceCloudPreview(ctx, src)
	}
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	writeWorkspacePreview(w, view)
}

func writeWorkspacePreview(w http.ResponseWriter, view workspacePreviewResult) {
	if view.Conflict != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"conflict":        view.Conflict,
			"source_url":      view.SourceURL,
			"source_name":     view.SourceName,
			"remote_modified": view.RemoteModified,
			"exported_at":     view.ExportedAt,
		})
		return
	}
	payload := map[string]any{"url": view.URL, "cached": view.Cached}
	if view.Source != "" {
		payload["source"] = view.Source
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleWorkspaceSyncLocal(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.workspaceUser(w, r)
	if !ok {
		return
	}
	var req workspaceFileRequest
	if !decodeWorkspace(w, r, &req) {
		return
	}
	path, ok := s.workspaceBoundFile(w, r, uid, req)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(workspaceContext(r, uid), 4*time.Minute)
	defer cancel()
	res, err := s.svc.syncWorkspaceLocal(ctx, uid, path)
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Service) workspaceLocalPreview(ctx context.Context, uid, path, name string) (workspacePreviewResult, error) {
	canonical := workspaceCanonicalPath(path)
	rec, linked, err := s.workspaceLink(uid, canonical)
	if err != nil {
		return workspacePreviewResult{}, err
	}
	if linked && isDocURL(rec.SourceURL) {
		sum, _, err := hashWorkspaceFile(ctx, path)
		if err != nil {
			return workspacePreviewResult{}, err
		}
		if sum == rec.ExportSHA256 {
			current := s.workspaceRemoteModified(ctx, rec)
			if rec.RemoteModified > 0 && current > rec.RemoteModified {
				return workspacePreviewResult{
					Conflict:       "cloud_updated",
					SourceURL:      rec.SourceURL,
					SourceName:     rec.SourceName,
					RemoteModified: current,
					ExportedAt:     rec.UpdatedAt,
				}, nil
			}
			return workspacePreviewResult{URL: rec.SourceURL, Cached: true, Source: "feishu_origin"}, nil
		}
	}
	f, info, err := openWorkspaceFile(path)
	if err != nil {
		return workspacePreviewResult{}, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, workspaceReader{ctx: ctx, reader: f})
	if err != nil {
		return workspacePreviewResult{}, err
	}
	digest := h.Sum(nil)
	key := fmt.Sprintf("local:%s:%d:%d:%x", path, size, info.ModTime().UnixMilli(), digest)
	uploaded := sha256.New()
	entry, err := s.cachedWorkspacePreview(ctx, uid, key, "local:"+canonical, func() (*Download, error) {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return &Download{
			Body: io.NopCloser(io.TeeReader(f, uploaded)), Size: size,
			Filename: sanitizeName(firstNonEmpty(name, filepath.Base(path)), 120),
		}, nil
	}, func() error {
		current, err := f.Stat()
		if err != nil {
			return err
		}
		if current.Size() != size || !bytes.Equal(uploaded.Sum(nil), digest) {
			return badRequest("预览期间文件已修改，请重试")
		}
		return nil
	})
	if err != nil {
		return workspacePreviewResult{}, err
	}
	return workspacePreviewResult{URL: entry.URL, Cached: entry.Cached}, nil
}

func (s *Service) cachedWorkspacePreview(ctx context.Context, uid, key, groupKey string, download func() (*Download, error), verifyUpload func() error) (workspacePreviewEntry, error) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	var result workspacePreviewEntry
	if err := ctx.Err(); err != nil {
		return result, err
	}
	index := workspacePreviewIndex{}
	if err := readWorkspaceJSON(s.workspacePreviewPath(), &index); err != nil {
		return result, err
	}
	if index == nil {
		index = workspacePreviewIndex{}
	}
	if entry := index[uid][key]; entry.Token != "" && isDocURL(entry.URL) {
		entry.Cached = true
		return entry, nil
	}
	d, err := download()
	if err != nil {
		return result, err
	}
	defer d.Body.Close()
	folder, err := s.api.DriveEnsurePrivateSubfolder(ctx, workspacePreviewFolder)
	if err != nil {
		return result, err
	}
	token, err := s.uploadWorkspaceDownload(ctx, folder, d.Filename, d)
	if err != nil {
		return result, err
	}
	committed := false
	defer func() {
		if !committed {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
			defer cancel()
			_ = s.api.DriveDeleteFile(cleanup, token, "file")
		}
	}()
	if verifyUpload != nil {
		if err = verifyUpload(); err != nil {
			return result, err
		}
	}
	if err = s.api.DriveClosePrivate(ctx, token, "file"); err != nil {
		return result, err
	}
	permissions, err := s.api.DrivePermissions(ctx, token, "file")
	if err != nil {
		return result, err
	}
	if err = verifyPrivatePermissions(permissions, uid, "", ""); err != nil {
		return result, err
	}
	href, err := s.api.DriveURL(ctx, token)
	if err != nil {
		return result, err
	}
	if !isDocURL(href) {
		return result, fmt.Errorf("飞书未返回有效的预览链接")
	}
	result = workspacePreviewEntry{Token: token, URL: href, GroupKey: groupKey}
	if index[uid] == nil {
		index[uid] = map[string]workspacePreviewEntry{}
	}
	index[uid][key] = result
	if err := writeWorkspaceJSON(s.workspacePreviewPath(), index); err != nil {
		return workspacePreviewEntry{}, err
	}
	committed = true
	s.retireWorkspacePreviewGroup(ctx, index, uid, key, groupKey, token)
	return result, nil
}

func (s *Service) retireWorkspacePreviewGroup(ctx context.Context, index workspacePreviewIndex, uid, key, groupKey, token string) {
	if groupKey == "" || index[uid] == nil {
		return
	}
	changed := false
	for oldKey, old := range index[uid] {
		if oldKey == key || old.GroupKey != groupKey || old.Token == "" || old.Token == token {
			continue
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
		err := s.api.DriveDeleteFile(cleanup, old.Token, "file")
		cancel()
		if err != nil {
			continue
		}
		delete(index[uid], oldKey)
		changed = true
	}
	if changed {
		_ = writeWorkspaceJSON(s.workspacePreviewPath(), index)
	}
}

func (s *Server) handleWorkspaceClearPreview(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.workspaceUser(w, r)
	if !ok {
		return
	}
	deleted, failed, err := s.svc.clearWorkspacePreview(workspaceContext(r, uid), uid)
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted, "failed": failed})
}

func (s *Service) clearWorkspacePreview(ctx context.Context, uid string) (deleted, failed int, err error) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	index := workspacePreviewIndex{}
	if err = readWorkspaceJSON(s.workspacePreviewPath(), &index); err != nil {
		return
	}
	for _, entry := range index[uid] {
		if err := s.api.DriveDeleteFile(ctx, entry.Token, "file"); err != nil {
			failed++
		} else {
			deleted++
		}
	}
	delete(index, uid)
	err = writeWorkspaceJSON(s.workspacePreviewPath(), index)
	return
}
