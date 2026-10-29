package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type TopicFileSync struct {
	NamesLoading bool     `json:"names_loading,omitempty"`
	Status       string   `json:"status"`
	Phase        string   `json:"phase,omitempty"`
	LastChecked  int64    `json:"last_checked,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}
type topicFileRecord struct {
	attachments  []Attachment
	state        TopicFileSync
	done         chan struct{}
	nameJobs     int
	nameAttempts map[string]time.Time
}

func safeSourcePath(s string) string {
	parts := strings.Split(strings.ReplaceAll(s, "\\", "/"), "/")
	for i, p := range parts {
		parts[i] = sanitizeName(p, 60)
	}
	return strings.Join(parts, "/")
}
func hashLocalFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func mergeTopicAttachments(previous, discovered []Attachment) []Attachment {
	out := append([]Attachment{}, previous...)
	seen := map[string]int{}
	identity := func(a Attachment) string {
		if a.ResourceToken != "" {
			return a.ResourceType + ":" + a.ResourceToken
		}
		return a.ID + ":" + a.MessageID + ":" + a.FileKey
	}
	for i, a := range out {
		seen[identity(a)] = i
	}
	for _, a := range discovered {
		if i, ok := seen[identity(a)]; ok {
			out[i].LastReferenced = max(out[i].LastReferenced, a.LastReferenced)
		} else {
			seen[identity(a)] = len(out)
			out = append(out, a)
		}
	}
	return out
}
func topicContext(tr *ThreadResp) TopicCtx {
	return TopicCtx{ChatID: tr.Topic.ChatID, ChatName: tr.Topic.ChatName, TopicKey: firstNonEmpty(tr.Topic.RootMessageID, tr.Topic.ThreadID), TopicTitle: tr.Topic.Title}
}

// Register the message catalogue locally. Opening, polling and refreshing a
// conversation never inspect cloud folders, versions or download file bytes.
func (s *Service) registerTopicFiles(ctx context.Context, tr *ThreadResp) *ThreadResp {
	if tr == nil {
		return nil
	}
	s.enrichThreadQueue(tr)
	raw := append([]Attachment{}, tr.Attachments...)
	for i := range raw {
		raw[i] = classifyAttachment(raw[i])
	}
	tc := topicContext(tr)
	key := s.k("files:" + tc.TopicKey)
	s.fileMu.Lock()
	if s.fileJobs == nil {
		s.fileJobs = map[string]*topicFileRecord{}
	}
	rec := s.fileJobs[key]
	if rec == nil {
		done := make(chan struct{})
		close(done)
		rec = &topicFileRecord{attachments: raw, state: TopicFileSync{Status: "manual"}, done: done}
		s.fileJobs[key] = rec
	} else {
		rec.attachments = mergeTopicAttachments(rec.attachments, raw)
	}
	forceNames, _ := ctx.Value(refreshMaterialNamesKey{}).(bool)
	s.startMaterialNamesLocked(rec, tc, forceNames)
	tr.Attachments = append([]Attachment{}, rec.attachments...)
	tr.FileSync = rec.state
	s.fileMu.Unlock()
	if s.dl != nil {
		s.dl.Catalog(tc, tr.Attachments)
		tr.Downloads = s.dl.List(tc.TopicKey)
	}
	if tr.Downloads == nil {
		tr.Downloads = []DLEntry{}
	}
	return tr
}

func resourceAttachment(base Attachment, r DriveResource) Attachment {
	base.ResourceToken, base.ResourceType = r.Token, r.Type
	if r.Name != "" {
		base.Name = r.Name
	}
	if r.URL != "" {
		base.URL = r.URL
	}
	base.RemoteModified = cloudMillis(r.Modified)
	switch r.Type {
	case "file":
		base.Kind, base.ResType, base.FileKey = "file", "drive", r.Token
		base.NoDownload = false
	case "folder":
		base.Kind = "folder"
	default:
		base.Kind, base.ResType, base.FileKey = "doc", "", ""
	}
	base.ID = "cloud:" + r.Type + ":" + r.Token
	return base
}

func (s *Service) resolveTopicResources(ctx context.Context, raw []Attachment) ([]Attachment, []string) {
	return s.resolveTopicResourcesProgress(ctx, raw, nil)
}
func (s *Service) resolveTopicResourcesProgress(ctx context.Context, raw []Attachment, publish func([]Attachment)) ([]Attachment, []string) {
	var out []Attachment
	var warns []string
	visited := map[string]bool{}
	queue := append([]Attachment{}, raw...)
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			warns = append(warns, "资料扫描未完成："+err.Error())
			break
		}
		a := queue[0]
		queue = queue[1:]
		typ, tok := a.ResourceType, a.ResourceToken
		if typ == "" {
			out = append(out, a)
			continue
		}
		if typ == "wiki" {
			r, err := s.api.WikiResolve(ctx, tok)
			if err != nil {
				a.ResolveError = "Wiki 解析失败：" + truncate(err.Error(), 100)
				a.Kind = "unresolved"
				out = append(out, a)
				warns = append(warns, a.Name+"："+a.ResolveError)
				continue
			}
			a = resourceAttachment(a, *r)
			typ, tok = r.Type, r.Token
		}
		identity := typ + ":" + tok
		if visited[identity] {
			if a.SourcePath != "" {
				for i, v := range out {
					if v.ResourceType == typ && v.ResourceToken == tok && v.SourcePath == "" {
						out[i].SourcePath, out[i].SourceFolder = a.SourcePath, a.SourceFolder
					}
				}
			}
			continue
		}
		visited[identity] = true
		if typ != "folder" {
			out = append(out, a)
			continue
		}
		a.Kind = "folder"
		out = append(out, a)
		folderIndex := len(out) - 1
		path := a.SourcePath
		label := a.Name
		if label == "" || label == a.URL {
			label = "飞书文件夹"
		}
		suffix := tok
		if len(suffix) > 6 {
			suffix = suffix[len(suffix)-6:]
		}
		if path != "" {
			path += "/"
		}
		path += sanitizeName(label, 40) + "_" + suffix
		page := ""
		seenPages := map[string]bool{}
		for {
			if seenPages[page] {
				out[folderIndex].ResolveError = "文件夹分页重复，未完整扫描"
				warns = append(warns, label+"：文件夹分页重复")
				break
			}
			seenPages[page] = true
			p, err := s.api.DriveList(ctx, tok, page)
			if err != nil {
				out[folderIndex].ResolveError = "文件夹扫描失败：" + truncate(err.Error(), 100)
				warns = append(warns, label+"："+out[folderIndex].ResolveError)
				break
			}
			for _, f := range p.Files {
				child := a
				child.SourceFolder, child.SourcePath = a.URL, path
				child.ResolveError = ""
				if f.Token == "" || f.Type == "" {
					warns = append(warns, label+"：子项缺少资源标识")
					continue
				}
				if f.Type == "shortcut" {
					warns = append(warns, f.Name+"：快捷方式未返回目标资源，请直接发送目标链接")
					child.Kind, child.ResourceType = "unresolved", "shortcut"
					child.Name, child.URL = f.Name, f.URL
					child.ResolveError = "请直接发送目标链接"
					out = append(out, child)
					continue
				}
				child = resourceAttachment(child, f)
				if f.URL == "" {
					child.URL = ""
					if f.Type == "folder" {
						if u, err := urlForFolder(a.URL, f.Token); err == nil {
							child.URL = u
						}
					}
				}
				queue = append(queue, child)
			}
			if publish != nil {
				publish(mergeTopicAttachments(out, queue))
			}
			if !p.HasMore {
				break
			}
			if p.NextPage == "" {
				out[folderIndex].ResolveError = "文件夹分页缺失，未完整扫描"
				warns = append(warns, label+"：分页不完整")
				break
			}
			page = p.NextPage
		}
	}
	refs := []DriveRef{}
	for _, a := range out {
		if a.ResourceToken != "" && a.ResourceType != "folder" && a.ResourceType != "wiki" && a.ResourceType != "shortcut" {
			refs = append(refs, DriveRef{Token: a.ResourceToken, Type: a.ResourceType})
		}
	}
	metas, err := s.api.DriveMetas(ctx, refs)
	if err != nil {
		warns = append(warns, "部分资料元数据查询失败："+truncate(err.Error(), 120))
	}
	for i, a := range out {
		if r, ok := metas[a.ResourceType+":"+a.ResourceToken]; ok {
			out[i] = resourceAttachment(a, r)
		} else if a.ResourceType != "" && a.ResourceType != "folder" && a.ResourceType != "wiki" && a.ResourceType != "shortcut" && a.Kind != "unresolved" {
			// A successful file download is still a fresh snapshot if metadata is absent.
			if a.ResourceType != "file" {
				out[i].ResolveError = "暂未获取到文档名称或访问链接"
				warns = append(warns, a.Name+"："+out[i].ResolveError)
			}
		}
	}
	return out, warns
}

func (m *DownloadManager) waitFile(ctx context.Context, id string) error {
	m.mu.Lock()
	var e *DLEntry
	for _, v := range m.entries {
		if v.ID == id {
			e = v
			break
		}
	}
	if e == nil {
		m.mu.Unlock()
		return errNoSuchFile
	}
	fl := m.flights[e.Key]
	m.mu.Unlock()
	if fl != nil {
		select {
		case <-fl.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.Status != dlDone || e.SyncStatus == "failed" || !m.localExistsLocked(e) {
		return fmt.Errorf("%s", firstNonEmpty(e.SyncError, e.Reason, "文件未下载完成"))
	}
	return nil
}
func (m *DownloadManager) checkedFile(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.ID == id {
			e.LastChecked = m.now().UnixMilli()
			e.SyncStatus, e.SyncError = "current", ""
			m.saveLocked()
			return
		}
	}
}
func (m *DownloadManager) markFolderSources(topic string, atts []Attachment, complete bool) {
	if !complete {
		return
	}
	present := map[string]bool{}
	for _, a := range atts {
		if a.FileKey != "" {
			present[a.FileKey] = true
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.TopicKey == topic && e.SourcePath != "" {
			e.RemoteMissing = !present[e.FileKey]
			if e.RemoteMissing {
				e.SyncStatus, e.SyncError = "removed", "来源文件夹中已移除，保留本地副本"
			}
		}
	}
	m.saveLocked()
}

// Keep folder links on the same verified Feishu host.
func urlForFolder(source, token string) (string, error) {
	u, err := url.Parse(source)
	if err != nil {
		return "", err
	}
	u.Path = "/drive/folder/" + token
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// Writable destinations reject symlinks before creating directories or files.
func (m *DownloadManager) writeDestination(rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || strings.ContainsRune(rel, 0) || filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrOutsideDir
	}
	if err := os.MkdirAll(m.base, 0o755); err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(m.base)
	if err != nil {
		return "", err
	}
	parent := root
	parts := strings.Split(clean, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)
		st, err := os.Lstat(parent)
		if os.IsNotExist(err) {
			if err = os.Mkdir(parent, 0o755); err != nil && !os.IsExist(err) {
				return "", err
			}
			st, err = os.Lstat(parent)
		}
		if err != nil {
			return "", err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", ErrOutsideDir
		}
	}
	full := filepath.Join(parent, parts[len(parts)-1])
	if st, err := os.Lstat(full); err == nil {
		if !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
			return "", ErrOutsideDir
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return full, nil
}
