package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// workspaceLinkRecord binds a file exported into a local workspace folder to
// the Feishu document it came from. The index key is the canonical container path.
type workspaceLinkRecord struct {
	LocalPath      string `json:"local_path"`
	ExportSHA256   string `json:"export_sha256"`
	SourceType     string `json:"source_type"`
	SourceToken    string `json:"source_token"`
	WikiToken      string `json:"wiki_token,omitempty"`
	SourceURL      string `json:"source_url"`
	SourceName     string `json:"source_name"`
	RemoteModified int64  `json:"remote_modified,omitempty"`
	UpdatedAt      int64  `json:"updated_at"`
}

type workspaceLinkIndex map[string]map[string]workspaceLinkRecord

type workspaceSyncResult struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	RemoteModified int64  `json:"remote_modified,omitempty"`
}

var errWorkspaceLocalEdited = badRequest("本地文件已修改，已停止覆盖以保护你的编辑")

func (s *Service) workspaceLinksPath() string {
	return filepath.Join(s.cfg.DataDir, "settings", "workspace_links.json")
}

func workspaceCanonicalPath(path string) string {
	clean := filepath.Clean(path)
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return clean
	}
	return real
}

func hashWorkspaceFile(ctx context.Context, path string) (string, os.FileInfo, error) {
	f, info, err := openWorkspaceFile(path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, workspaceReader{ctx: ctx, reader: f}); err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(h.Sum(nil)), info, nil
}

func (s *Service) loadWorkspaceLinks() (workspaceLinkIndex, error) {
	index := workspaceLinkIndex{}
	if err := readWorkspaceJSON(s.workspaceLinksPath(), &index); err != nil {
		return nil, err
	}
	if index == nil {
		index = workspaceLinkIndex{}
	}
	return index, nil
}

func (s *Service) workspaceLinks(uid string) (map[string]workspaceLinkRecord, error) {
	s.linksMu.Lock()
	defer s.linksMu.Unlock()
	index, err := s.loadWorkspaceLinks()
	if err != nil {
		return nil, err
	}
	out := map[string]workspaceLinkRecord{}
	for key, rec := range index[uid] {
		out[key] = rec
	}
	return out, nil
}

func (s *Service) workspaceLink(uid, canonical string) (workspaceLinkRecord, bool, error) {
	s.linksMu.Lock()
	defer s.linksMu.Unlock()
	index, err := s.loadWorkspaceLinks()
	if err != nil {
		return workspaceLinkRecord{}, false, err
	}
	rec, ok := index[uid][canonical]
	return rec, ok, nil
}

func (s *Service) saveWorkspaceLink(uid, canonical string, rec workspaceLinkRecord) error {
	if uid == "" || canonical == "" {
		return badRequest("缺少登录用户")
	}
	s.linksMu.Lock()
	defer s.linksMu.Unlock()
	index, err := s.loadWorkspaceLinks()
	if err != nil {
		return err
	}
	if index[uid] == nil {
		index[uid] = map[string]workspaceLinkRecord{}
	}
	index[uid][canonical] = rec
	return writeWorkspaceJSON(s.workspaceLinksPath(), index)
}

// fillWorkspaceOrigin records the newest cloud edit time it can see.
// Lookup failures leave the timestamp at zero so callers can keep the previous value.
func (s *Service) fillWorkspaceOrigin(ctx context.Context, rec *workspaceLinkRecord) {
	var latest int64
	note := func(ms int64) {
		if ms > latest {
			latest = ms
		}
	}
	if rec.WikiToken != "" {
		if node, err := s.api.WikiNodeInfo(ctx, rec.WikiToken); err == nil && node != nil {
			note(cloudMillis(node.Modified))
			if !isDocURL(rec.SourceURL) && isDocURL(node.URL) {
				rec.SourceURL = node.URL
			}
			if rec.SourceName == "" {
				rec.SourceName = node.Title
			}
		}
	}
	if rec.SourceType != "" && rec.SourceToken != "" {
		metas, err := s.api.DriveMetas(ctx, []DriveRef{{Type: rec.SourceType, Token: rec.SourceToken}})
		if err == nil {
			meta := metas[rec.SourceType+":"+rec.SourceToken]
			note(cloudMillis(meta.Modified))
			if !isDocURL(rec.SourceURL) && isDocURL(meta.URL) {
				rec.SourceURL = meta.URL
			}
			if rec.SourceName == "" && meta.Name != "" {
				rec.SourceName = meta.Name
			}
		}
		if latest == 0 && rec.SourceType == "file" {
			if meta, err := s.api.DriveMeta(ctx, rec.SourceToken); err == nil && meta != nil {
				note(meta.LatestModifyTime)
				if !isDocURL(rec.SourceURL) && isDocURL(meta.URL) {
					rec.SourceURL = meta.URL
				}
			}
		}
	}
	rec.RemoteModified = latest
}

func (s *Service) workspaceRemoteModified(ctx context.Context, rec workspaceLinkRecord) int64 {
	probe := rec
	probe.RemoteModified = 0
	s.fillWorkspaceOrigin(ctx, &probe)
	return probe.RemoteModified
}

func (s *Service) rememberWorkspaceExport(ctx context.Context, saved string, src workspaceSource, ref DriveRef, wiki *WikiNodeDetail) error {
	uid, _ := ctx.Value(workspaceUserKey{}).(string)
	if uid == "" {
		return badRequest("缺少登录用户")
	}
	rec := workspaceLinkRecord{
		SourceType:  ref.Type,
		SourceToken: ref.Token,
		SourceName:  firstNonEmpty(src.Name, filepath.Base(saved)),
	}
	if wiki != nil {
		rec.WikiToken = wiki.NodeToken
	}
	switch {
	case isDocURL(src.URL):
		rec.SourceURL = src.URL
	case wiki != nil && isDocURL(wiki.URL):
		rec.SourceURL = wiki.URL
	case wiki != nil:
		rec.SourceURL = workspaceResourceURL(workspaceFeishuOrigin(s.cfg.BaseURL), "wiki", wiki.NodeToken)
	default:
		rec.SourceURL = workspaceResourceURL(workspaceFeishuOrigin(s.cfg.BaseURL), ref.Type, ref.Token)
	}
	s.fillWorkspaceOrigin(ctx, &rec)
	sum, _, err := hashWorkspaceFile(ctx, saved)
	if err != nil {
		return err
	}
	canonical := workspaceCanonicalPath(saved)
	rec.ExportSHA256 = sum
	rec.LocalPath = s.workspaceDisplayPath(canonical)
	rec.UpdatedAt = time.Now().UnixMilli()
	return s.saveWorkspaceLink(uid, canonical, rec)
}

func (s *Service) downloadWorkspaceLink(ctx context.Context, rec workspaceLinkRecord) (*Download, error) {
	switch {
	case rec.SourceType == "file":
		return s.api.DriveDownload(ctx, rec.SourceToken)
	case exportExtension(rec.SourceType) != "":
		return s.api.DriveExport(ctx, rec.SourceToken, rec.SourceType)
	default:
		return nil, badRequest("该飞书资源暂不支持同步到本地")
	}
}

func writeWorkspaceTemp(ctx context.Context, dir string, d *Download) (path, sum string, err error) {
	if d == nil || d.Size <= 0 {
		return "", "", fmt.Errorf("实际读取文件大小与预期不符")
	}
	tmp, err := os.CreateTemp(dir, ".workspace-sync-*")
	if err != nil {
		return "", "", err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	n, copyErr := io.Copy(tmp, io.TeeReader(workspaceReader{ctx: ctx, reader: d.Body}, h))
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		err = firstError(copyErr, closeErr)
		return "", "", err
	}
	if n != d.Size {
		err = fmt.Errorf("实际读取文件大小与预期不符")
		return "", "", err
	}
	return tmp.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Service) syncWorkspaceLocal(ctx context.Context, uid, path string) (workspaceSyncResult, error) {
	var result workspaceSyncResult
	canonical := workspaceCanonicalPath(path)
	rec, ok, err := s.workspaceLink(uid, canonical)
	if err != nil {
		return result, err
	}
	if !ok {
		return result, badRequest("该本地文件没有关联的飞书文档，无法同步")
	}
	before, err := os.Stat(path)
	if err != nil {
		return result, err
	}
	sum, info, err := hashWorkspaceFile(ctx, path)
	if err != nil {
		return result, err
	}
	if sum != rec.ExportSHA256 || !os.SameFile(before, info) {
		return result, errWorkspaceLocalEdited
	}
	current := s.workspaceRemoteModified(ctx, rec)
	d, err := s.downloadWorkspaceLink(ctx, rec)
	if err != nil {
		return result, err
	}
	defer d.Body.Close()
	tmp, nextSum, err := writeWorkspaceTemp(ctx, filepath.Dir(path), d)
	if err != nil {
		return result, err
	}
	defer os.Remove(tmp)
	sum, info, err = hashWorkspaceFile(ctx, path)
	if err != nil {
		return result, err
	}
	if sum != rec.ExportSHA256 || !os.SameFile(before, info) {
		return result, errWorkspaceLocalEdited
	}
	backup, err := replaceWorkspaceFile(tmp, path, before.Mode().Perm())
	if err != nil {
		return result, err
	}
	if current > 0 {
		rec.RemoteModified = current
	}
	rec.ExportSHA256 = nextSum
	rec.LocalPath = s.workspaceDisplayPath(canonical)
	rec.UpdatedAt = time.Now().UnixMilli()
	if err := s.saveWorkspaceLink(uid, canonical, rec); err != nil {
		if undo := os.Rename(backup, path); undo != nil {
			os.Remove(backup)
			return result, err
		}
		os.Remove(backup)
		return result, err
	}
	os.Remove(backup)
	return workspaceSyncResult{Name: filepath.Base(path), URL: rec.SourceURL, RemoteModified: rec.RemoteModified}, nil
}

func replaceWorkspaceFile(tmp, target string, mode os.FileMode) (string, error) {
	if err := os.Chmod(tmp, mode); err != nil {
		return "", err
	}
	bak, err := os.CreateTemp(filepath.Dir(target), ".workspace-sync-bak-*")
	if err != nil {
		return "", err
	}
	bakName := bak.Name()
	if err := bak.Close(); err != nil {
		os.Remove(bakName)
		return "", err
	}
	if err := os.Rename(target, bakName); err != nil {
		os.Remove(bakName)
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		if undo := os.Rename(bakName, target); undo != nil {
			return bakName, err
		}
		os.Remove(bakName)
		return "", err
	}
	return bakName, nil
}
