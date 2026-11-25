package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	workspaceWiki  = "wiki"
	workspaceDrive = "feishu_folder"
	workspaceLocal = "local_dir"
)

type ChatWorkspace struct {
	AttachContext bool            `json:"attach_context"`
	Items         []WorkspaceItem `json:"items"`
}

type WorkspaceItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	URL       string `json:"url,omitempty"`
	SpaceID   string `json:"space_id,omitempty"`
	Token     string `json:"token,omitempty"`
	LocalPath string `json:"local_path,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

type workspaceNode struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	URL         string `json:"url,omitempty"`
	Token       string `json:"token,omitempty"`
	RelPath     string `json:"rel_path,omitempty"`
	HasChildren bool   `json:"has_children"`
	CanStore    bool   `json:"can_store"`
	SourceURL   string `json:"source_url,omitempty"`
	SourceState string `json:"source_state,omitempty"`
}

type workspaceTreePage struct {
	Items    []workspaceNode `json:"items"`
	NextPage string          `json:"next_page_token,omitempty"`
}

type workspaceSettings map[string]map[string]ChatWorkspace

func readWorkspaceJSON[T any](path string, data *T) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, data)
}

func writeWorkspaceJSON(path string, data any) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o600)
}

func (s *Service) workspaceSettingsPath() string {
	return filepath.Join(s.cfg.DataDir, "settings", "workspaces.json")
}

func (s *Service) chatWorkspace(userID, chatID string) (ChatWorkspace, error) {
	config := workspaceSettings{}
	if err := readWorkspaceJSON(s.workspaceSettingsPath(), &config); err != nil {
		return ChatWorkspace{}, err
	}
	ws := config[userID][chatID]
	if ws.Items == nil {
		ws.Items = []WorkspaceItem{}
	}
	return ws, nil
}

func (s *Service) saveWorkspace(userID, chatID string, ws ChatWorkspace) error {
	s.workspaceMu.Lock()
	defer s.workspaceMu.Unlock()
	config := workspaceSettings{}
	if err := readWorkspaceJSON(s.workspaceSettingsPath(), &config); err != nil {
		return err
	}
	if config == nil {
		config = workspaceSettings{}
	}
	if config[userID] == nil {
		config[userID] = map[string]ChatWorkspace{}
	}
	config[userID][chatID] = ws
	return writeWorkspaceJSON(s.workspaceSettingsPath(), config)
}

func (s *Service) workspaceItem(userID, chatID, itemID string) (WorkspaceItem, error) {
	if !keyRe.MatchString(chatID) || !keyRe.MatchString(itemID) {
		return WorkspaceItem{}, badRequest("群或工作区标识无效")
	}
	ws, err := s.chatWorkspace(userID, chatID)
	if err != nil {
		return WorkspaceItem{}, err
	}
	for _, item := range ws.Items {
		if item.ID == itemID {
			return item, nil
		}
	}
	return WorkspaceItem{}, badRequest("当前群未绑定该工作区")
}

func (s *Server) workspaceUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid := s.svc.auth.UserID()
	if uid == "" {
		writeErr(w, ErrLoginRequired)
		return "", false
	}
	if r.Method != http.MethodGet && !s.checkCSRF(r) {
		writeJSON(w, http.StatusForbidden, apiError{Error: "缺少或错误的 CSRF 令牌，请刷新页面后重试"})
		return "", false
	}
	return uid, true
}

func workspaceContext(r *http.Request, uid string) context.Context {
	return context.WithValue(r.Context(), workspaceUserKey{}, uid)
}

func decodeWorkspace(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(out); err != nil || dec.Decode(new(any)) != io.EOF {
		writeErr(w, badRequest("请求参数无效"))
		return false
	}
	return true
}

func writeWorkspaceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrOutsideDir):
		writeJSON(w, http.StatusForbidden, apiError{Error: "路径超出已绑定的工作区范围"})
	case errors.Is(err, os.ErrNotExist), errors.Is(err, errNoSuchFile):
		writeJSON(w, http.StatusNotFound, apiError{Error: "文件或目录不存在"})
	case errors.Is(err, os.ErrPermission):
		writeJSON(w, http.StatusForbidden, apiError{Error: "无权访问文件或目录"})
	default:
		writeErr(w, err)
	}
}

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.workspaceUser(w, r)
	if !ok {
		return
	}
	var req struct {
		ChatID string `json:"chat_id"`
		ChatWorkspace
	}
	req.ChatID = r.URL.Query().Get("chat_id")
	if r.Method == http.MethodPost && !decodeWorkspace(w, r, &req) {
		return
	}
	if !keyRe.MatchString(req.ChatID) {
		writeErr(w, badRequest("群聊标识无效"))
		return
	}
	ws, err := s.svc.chatWorkspace(uid, req.ChatID)
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	if r.Method == http.MethodGet {
		if !s.localClient(r) {
			for i := range ws.Items {
				ws.Items[i].LocalPath = ""
			}
		}
		writeJSON(w, http.StatusOK, ws)
		return
	}
	if len(req.Items) > 100 {
		writeErr(w, badRequest("每个群最多绑定 100 个工作区目录"))
		return
	}
	previous := make(map[string]WorkspaceItem, len(ws.Items))
	for _, item := range ws.Items {
		previous[item.ID] = item
	}
	next := ChatWorkspace{AttachContext: req.AttachContext, Items: []WorkspaceItem{}}
	used := make(map[string]bool, len(req.Items))
	ctx := workspaceContext(r, uid)
	for _, item := range req.Items {
		old := previous[item.ID]
		if item.Kind == workspaceLocal || old.Kind == workspaceLocal {
			if item.Kind == workspaceLocal && item.LocalPath == "" && old.Kind == workspaceLocal {
				item.LocalPath = old.LocalPath
			}
			if old.Kind != workspaceLocal || old.LocalPath != item.LocalPath || item.Kind != old.Kind {
				if !s.localOnly(w, r) {
					return
				}
			}
		}
		if item.ID == "" {
			item.ID = newCSRFToken()
		}
		if !keyRe.MatchString(item.ID) || used[item.ID] {
			writeErr(w, badRequest("工作区条目标识无效或重复"))
			return
		}
		used[item.ID] = true
		item, err = s.svc.normalizeWorkspaceItem(ctx, item, old)
		if err != nil {
			writeWorkspaceErr(w, err)
			return
		}
		next.Items = append(next.Items, item)
	}
	for id, item := range previous {
		if item.Kind == workspaceLocal && !used[id] && !s.localOnly(w, r) {
			return
		}
	}
	if s.svc.auth.UserID() != uid {
		writeErr(w, ErrLoginRequired)
		return
	}
	if err := s.svc.saveWorkspace(uid, req.ChatID, next); err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	if !s.localClient(r) {
		for i := range next.Items {
			next.Items[i].LocalPath = ""
		}
	}
	writeJSON(w, http.StatusOK, next)
}

func (s *Service) normalizeWorkspaceItem(ctx context.Context, item, old WorkspaceItem) (WorkspaceItem, error) {
	item.Name = strings.TrimSpace(item.Name)
	item.URL = strings.TrimSpace(item.URL)
	item.CreatedAt = old.CreatedAt
	if item.CreatedAt == 0 {
		item.CreatedAt = time.Now().UnixMilli()
	}
	if item.Kind == workspaceLocal {
		item.Token, item.SpaceID, item.URL = "", "", ""
		if old.Kind != workspaceLocal || item.LocalPath != old.LocalPath {
			dir, err := s.workspaceLocalRoot(item.LocalPath)
			if err != nil {
				return item, err
			}
			item.LocalPath = s.workspaceDisplayPath(dir)
		}
		display := item.LocalPath
		if base := filepath.Base(display); base != "" && base != "." && base != "/" && base != `\` {
			item.Name = firstNonEmpty(item.Name, base)
		} else {
			item.Name = firstNonEmpty(item.Name, display)
		}
		return item, nil
	}
	item.LocalPath = ""
	if item.URL != "" {
		if spaceID := workspaceSpaceID(item.URL); spaceID != "" {
			item.Kind, item.SpaceID, item.Token = workspaceWiki, spaceID, ""
		} else {
			ref, ok := driveRef(item.URL)
			if !ok || (ref.Type != "wiki" && ref.Type != "folder") {
				return item, badRequest("请输入飞书知识库或云盘文件夹链接")
			}
			item.Token, item.SpaceID = ref.Token, ""
			item.Kind = workspaceWiki
			if ref.Type == "folder" {
				item.Kind = workspaceDrive
			}
		}
	}
	switch item.Kind {
	case workspaceWiki:
		if item.Token != "" {
			if !keyRe.MatchString(item.Token) {
				return item, badRequest("知识库节点标识无效")
			}
			if old.Kind == item.Kind && old.Token == item.Token && old.URL == item.URL {
				item.SpaceID, item.Name = old.SpaceID, firstNonEmpty(item.Name, old.Name)
				return item, nil
			}
			node, err := s.api.WikiNodeInfo(ctx, item.Token)
			if err != nil {
				return item, err
			}
			if !keyRe.MatchString(node.SpaceID) || !keyRe.MatchString(node.NodeToken) {
				return item, badRequest("飞书未返回有效的知识库节点信息")
			}
			item.SpaceID, item.Token = node.SpaceID, node.NodeToken
			item.URL = firstNonEmpty(item.URL, node.URL, wikiURL(s.cfg.BaseURL, node.NodeToken))
			item.Name = firstNonEmpty(item.Name, node.Title, item.Token)
			return item, nil
		}
		if !keyRe.MatchString(item.SpaceID) {
			return item, badRequest("请选择知识库空间或填写知识库链接")
		}
		if item.SpaceID != old.SpaceID || old.Token != "" {
			if _, err := s.api.WikiNodes(ctx, item.SpaceID, "", ""); err != nil {
				return item, err
			}
		}
		item.URL = firstNonEmpty(item.URL, workspaceFeishuOrigin(s.cfg.BaseURL)+"/wiki/space/"+item.SpaceID)
		item.Name = firstNonEmpty(item.Name, "知识库 "+item.SpaceID)
	case workspaceDrive:
		if !keyRe.MatchString(item.Token) {
			return item, badRequest("请选择飞书文件夹或填写文件夹链接")
		}
		if item.Token != old.Token || item.Kind != old.Kind {
			if _, err := s.api.DriveList(ctx, item.Token, ""); err != nil {
				return item, err
			}
		}
		item.SpaceID = ""
		item.URL = firstNonEmpty(item.URL, workspaceResourceURL(workspaceFeishuOrigin(s.cfg.BaseURL), "folder", item.Token))
		item.Name = firstNonEmpty(item.Name, "云盘文件夹 "+item.Token)
	default:
		return item, badRequest("工作区类型无效")
	}
	return item, nil
}

func workspaceSpaceID(href string) string {
	if !isDocURL(href) {
		return ""
	}
	u, _ := url.Parse(href)
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 3 && parts[0] == "wiki" && parts[1] == "space" && keyRe.MatchString(parts[2]) {
		return parts[2]
	}
	return ""
}

func workspaceResourceURL(origin, kind, token string) string {
	path := map[string]string{"file": "file", "folder": "drive/folder", "doc": "docs", "docx": "docx", "sheet": "sheets", "bitable": "base", "slides": "slides", "wiki": "wiki", "mindnote": "mindnotes", "minutes": "minutes"}[kind]
	if path == "" || !keyRe.MatchString(token) {
		return ""
	}
	return origin + "/" + path + "/" + url.PathEscape(token)
}

func workspaceOrigin(baseURL, href string) string {
	if isDocURL(href) {
		u, _ := url.Parse(href)
		return u.Scheme + "://" + u.Host
	}
	return workspaceFeishuOrigin(baseURL)
}

func realDirectory(path string) (string, error) {
	if path == "" {
		return "", badRequest("请选择本地文件夹")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", badRequest("指定路径不是文件夹")
	}
	return real, nil
}

func workspaceRelative(rel string) bool {
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(rel) && filepath.VolumeName(rel) == "" && !strings.ContainsRune(rel, 0)
}

func (s *Service) workspaceLocalPath(storedRoot, rel string, dir bool) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if !workspaceRelative(clean) {
		return "", ErrOutsideDir
	}
	base, err := s.workspaceLocalRoot(storedRoot)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(filepath.Join(base, clean))
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(base, real)
	if err != nil || !workspaceRelative(within) {
		return "", ErrOutsideDir
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if dir && !st.IsDir() {
		return "", badRequest("目标不是文件夹")
	}
	if !dir && !st.Mode().IsRegular() {
		return "", badRequest("请选择普通文件")
	}
	return real, nil
}

func (s *Server) handleWorkspaceLocalPick(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.workspaceUser(w, r); !ok || !s.localOnly(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	var path string
	var cancelled bool
	var err error
	if !(s.docker && s.opener.HostURL == "") {
		path, cancelled, err = s.opener.Pick(ctx)
	}
	if cancelled {
		writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
		return
	}
	if err == nil && path != "" {
		real, err := s.svc.workspaceLocalRoot(path)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]string{"path": s.svc.workspaceDisplayPath(real)})
			return
		}
	}
	if !s.docker {
		path, cancelled, err = pickFolderPath(runtime.GOOS, s.opener.Run)
		if cancelled {
			writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
			return
		}
		if err == nil && path != "" {
			if real, err := s.svc.workspaceLocalRoot(path); err == nil {
				writeJSON(w, http.StatusOK, map[string]string{"path": s.svc.workspaceDisplayPath(real)})
				return
			}
		}
	}
	reason := "无法打开系统文件夹选择器时，可在下方浏览本地目录完成绑定。"
	if s.docker && s.opener.HostURL == "" {
		reason = "Docker 中需要在本机启动 feishu-topics-opener（默认 127.0.0.1:18089），或直接在下方面板浏览「桌面 / 文稿 / 下载」。"
	}
	writeJSON(w, http.StatusOK, map[string]any{"fallback": true, "reason": reason})
}

func (s *Server) handleWorkspaceLocalPlaces(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.workspaceUser(w, r); !ok || !s.localOnly(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.svc.workspaceLocalPlaces()})
}

func (s *Server) handleWorkspaceLocalBrowse(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.workspaceUser(w, r); !ok || !s.localOnly(w, r) {
		return
	}
	dir := strings.TrimSpace(r.URL.Query().Get("dir"))
	if dir == "" {
		for _, place := range s.svc.workspaceLocalPlaces() {
			if place.ID == "home" {
				dir = place.Path
				break
			}
		}
		if dir == "" {
			dir = "~"
		}
	}
	data, err := s.svc.workspaceBrowseDir(dir)
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handleWorkspaceCloudBrowse(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.workspaceUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	kind := strings.TrimSpace(q.Get("kind"))
	ctx := workspaceContext(r, uid)
	switch kind {
	case workspaceDrive:
		token := strings.TrimSpace(q.Get("token"))
		if token == "" {
			root, err := s.svc.api.DriveRootFolder(ctx)
			if err != nil {
				writeWorkspaceErr(w, err)
				return
			}
			token = root
		}
		if !keyRe.MatchString(token) {
			writeErr(w, badRequest("云盘文件夹标识无效"))
			return
		}
		p, err := s.svc.api.DriveList(ctx, token, q.Get("page_token"))
		if err != nil {
			writeWorkspaceErr(w, err)
			return
		}
		origin := workspaceFeishuOrigin(s.cfg.BaseURL)
		type item struct {
			Name        string `json:"name"`
			Token       string `json:"token"`
			URL         string `json:"url,omitempty"`
			HasChildren bool   `json:"has_children"`
		}
		items := []item{}
		for _, f := range p.Files {
			if f.Type != "folder" {
				continue
			}
			items = append(items, item{
				Name: f.Name, Token: f.Token,
				URL:         firstNonEmpty(f.URL, workspaceResourceURL(origin, "folder", f.Token)),
				HasChildren: true,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"parent": map[string]string{"name": "当前文件夹", "token": token},
			"items":  items, "next_page_token": p.NextPage, "has_more": p.HasMore,
		})
	case workspaceWiki:
		spaceID := strings.TrimSpace(q.Get("space_id"))
		if spaceID == "" {
			p, err := s.svc.api.WikiSpaces(ctx, q.Get("page_token"))
			if err != nil {
				writeWorkspaceErr(w, err)
				return
			}
			type item struct {
				Name        string `json:"name"`
				SpaceID     string `json:"space_id"`
				URL         string `json:"url,omitempty"`
				HasChildren bool   `json:"has_children"`
			}
			items := []item{}
			for _, sp := range p.Items {
				items = append(items, item{
					Name: sp.Name, SpaceID: sp.SpaceID,
					URL:         workspaceFeishuOrigin(s.cfg.BaseURL) + "/wiki/space/" + sp.SpaceID,
					HasChildren: true,
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items": items, "next_page_token": p.PageToken, "has_more": p.HasMore,
			})
			return
		}
		if !keyRe.MatchString(spaceID) {
			writeErr(w, badRequest("知识库空间标识无效"))
			return
		}
		node := strings.TrimSpace(q.Get("node"))
		p, err := s.svc.api.WikiNodes(ctx, spaceID, node, q.Get("page_token"))
		if err != nil {
			writeWorkspaceErr(w, err)
			return
		}
		origin := workspaceFeishuOrigin(s.cfg.BaseURL)
		type item struct {
			Name        string `json:"name"`
			Token       string `json:"token"`
			SpaceID     string `json:"space_id"`
			URL         string `json:"url,omitempty"`
			HasChildren bool   `json:"has_children"`
		}
		items := []item{}
		for _, n := range p.Items {
			items = append(items, item{
				Name: n.Title, Token: n.NodeToken, SpaceID: spaceID,
				URL:         firstNonEmpty(n.URL, workspaceResourceURL(origin, workspaceWiki, n.NodeToken)),
				HasChildren: n.HasChild,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"parent":          map[string]string{"name": "当前节点", "space_id": spaceID, "token": node},
			"items":           items,
			"next_page_token": p.PageToken,
			"has_more":        p.HasMore,
		})
	default:
		writeErr(w, badRequest("工作区类型无效"))
	}
}

func (s *Server) handleWorkspaceSpaces(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.workspaceUser(w, r)
	if !ok {
		return
	}
	p, err := s.svc.api.WikiSpaces(workspaceContext(r, uid), r.URL.Query().Get("page_token"))
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleWorkspaceDriveRoots(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.workspaceUser(w, r)
	if !ok {
		return
	}
	ctx := workspaceContext(r, uid)
	root, err := s.svc.api.DriveRootFolder(ctx)
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	p, err := s.svc.api.DriveList(ctx, root, r.URL.Query().Get("page_token"))
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	folders := []DriveResource{}
	for _, f := range p.Files {
		if f.Type == "folder" {
			folders = append(folders, f)
		}
	}
	p.Files = folders
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleWorkspaceTree(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.workspaceUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	item, err := s.svc.workspaceItem(uid, q.Get("chat_id"), q.Get("item_id"))
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	if item.Kind == workspaceLocal && !s.localOnly(w, r) {
		return
	}
	var path []string
	if q.Get("path") != "" {
		path = strings.Split(q.Get("path"), ",")
	}
	page, err := s.svc.workspaceTree(workspaceContext(r, uid), item, q.Get("node"), q.Get("page_token"), path)
	if err != nil {
		writeWorkspaceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Service) workspaceTree(ctx context.Context, item WorkspaceItem, node, page string, path []string) (workspaceTreePage, error) {
	result := workspaceTreePage{Items: []workspaceNode{}}
	origin := workspaceOrigin(s.cfg.BaseURL, item.URL)
	switch item.Kind {
	case workspaceLocal:
		dir, err := s.workspaceLocalPath(item.LocalPath, node, true)
		if err != nil {
			return result, err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return result, err
		}
		uid, _ := ctx.Value(workspaceUserKey{}).(string)
		links, err := s.workspaceLinks(uid)
		if err != nil {
			return result, err
		}
		offset := 0
		if page != "" {
			offset, err = strconv.Atoi(page)
			if err != nil || offset < 0 {
				return result, badRequest("分页参数无效")
			}
		}
		for i, entry := range entries {
			if i < offset {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(node, entry.Name()))
			isDir := entry.IsDir()
			if entry.Type()&os.ModeSymlink != 0 {
				isDir = s.workspaceCanBrowseLink(item.LocalPath, rel)
			}
			real, err := s.workspaceLocalPath(item.LocalPath, rel, isDir)
			if err != nil {
				continue
			}
			kind := "local_file"
			if isDir {
				kind = workspaceLocal
			}
			itemNode := workspaceNode{Name: entry.Name(), Kind: kind, RelPath: rel, HasChildren: isDir, CanStore: isDir}
			if !isDir {
				if rec, ok := links[workspaceCanonicalPath(real)]; ok && isDocURL(rec.SourceURL) {
					sum, _, hashErr := hashWorkspaceFile(ctx, real)
					if hashErr == nil {
						itemNode.SourceURL = rec.SourceURL
						itemNode.SourceState = "modified"
						if sum == rec.ExportSHA256 {
							itemNode.SourceState = "linked"
						}
					} else if ctx.Err() != nil {
						return result, ctx.Err()
					}
				}
			}
			result.Items = append(result.Items, itemNode)
			if len(result.Items) == 200 && i+1 < len(entries) {
				result.NextPage = strconv.Itoa(i + 1)
				break
			}
		}
	case workspaceDrive:
		target, err := s.workspaceDriveTarget(ctx, item.Token, node, path)
		if err != nil {
			return result, err
		}
		p, err := s.api.DriveList(ctx, target, page)
		if err != nil {
			return result, err
		}
		if p.HasMore {
			result.NextPage = p.NextPage
		}
		for _, f := range p.Files {
			folder := f.Type == "folder"
			result.Items = append(result.Items, workspaceNode{Name: f.Name, Kind: f.Type, URL: firstNonEmpty(f.URL, workspaceResourceURL(origin, f.Type, f.Token)), Token: f.Token, HasChildren: folder, CanStore: folder})
		}
	case workspaceWiki:
		target, err := s.workspaceWikiTarget(ctx, item, node)
		if err != nil {
			return result, err
		}
		p, err := s.api.WikiNodes(ctx, item.SpaceID, target, page)
		if err != nil {
			return result, err
		}
		if p.HasMore {
			result.NextPage = p.PageToken
		}
		for _, n := range p.Items {
			result.Items = append(result.Items, workspaceNode{Name: n.Title, Kind: workspaceWiki, URL: firstNonEmpty(n.URL, workspaceResourceURL(origin, workspaceWiki, n.NodeToken)), Token: n.NodeToken, HasChildren: n.HasChild, CanStore: true})
		}
	}
	return result, nil
}

func (s *Service) workspaceCanBrowseLink(root, rel string) bool {
	_, err := s.workspaceLocalPath(root, rel, true)
	return err == nil
}

func (s *Service) workspaceWikiTarget(ctx context.Context, item WorkspaceItem, node string) (string, error) {
	if node == "" || node == item.Token {
		return item.Token, nil
	}
	seen := map[string]bool{}
	for token := node; token != ""; {
		if !keyRe.MatchString(token) || seen[token] {
			return "", ErrOutsideDir
		}
		seen[token] = true
		n, err := s.api.WikiNodeInfo(ctx, token)
		if err != nil {
			return "", err
		}
		if n.SpaceID != item.SpaceID || n.NodeToken != token {
			return "", ErrOutsideDir
		}
		if n.ParentNodeToken == item.Token {
			return node, nil
		}
		token = n.ParentNodeToken
	}
	return "", ErrOutsideDir
}

func (s *Service) workspaceDriveFolders(ctx context.Context, parent string, match func(string) bool) (bool, error) {
	for page := ""; ; {
		p, err := s.api.DriveList(ctx, parent, page)
		if err != nil {
			return false, err
		}
		for _, f := range p.Files {
			if f.Type == "folder" && keyRe.MatchString(f.Token) && match(f.Token) {
				return true, nil
			}
		}
		if !p.HasMore {
			return false, nil
		}
		if p.NextPage == "" || p.NextPage == page {
			return false, fmt.Errorf("飞书文件夹分页标识无效")
		}
		page = p.NextPage
	}
}

func (s *Service) workspaceDriveTarget(ctx context.Context, root, node string, path []string) (string, error) {
	if node == "" || node == root {
		return root, nil
	}
	if !keyRe.MatchString(node) {
		return "", ErrOutsideDir
	}
	if len(path) != 0 {
		if path[len(path)-1] != node || len(path) > 100 {
			return "", ErrOutsideDir
		}
		parent := root
		for _, child := range path {
			found, err := s.workspaceDriveFolders(ctx, parent, func(token string) bool { return token == child })
			if err != nil {
				return "", err
			}
			if !found {
				return "", ErrOutsideDir
			}
			parent = child
		}
		return node, nil
	}
	// Clients can supply only the target token. Traverse the binding to prove ancestry.
	pending := []string{root}
	seen := map[string]bool{root: true}
	for i := 0; i < len(pending); i++ {
		found, err := s.workspaceDriveFolders(ctx, pending[i], func(token string) bool {
			if !seen[token] {
				seen[token] = true
				pending = append(pending, token)
			}
			return token == node
		})
		if err != nil {
			return "", err
		}
		if found {
			return node, nil
		}
	}
	return "", ErrOutsideDir
}
