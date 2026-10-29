package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
)

func (m *MockAPI) DriveClosePrivate(ctx context.Context, token, kind string) error {
	if err := m.DriveSetLinkShare(ctx, token, "closed"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.privateClosed == nil {
		m.privateClosed = map[string]bool{}
	}
	m.privateClosed[token] = true
	return nil
}
func (m *MockAPI) DrivePermissions(ctx context.Context, token, kind string) (*DrivePermissionSnapshot, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := &DrivePermissionSnapshot{LinkShare: m.linkShares[token], Members: append([]DrivePermissionMember{}, m.driveMembers[token]...)}
	if m.privateClosed[token] {
		p.ShareEntity, p.ManageEntity = "same_tenant", "collaborator_full_access"
	}
	return p, nil
}
func (m *MockAPI) DriveList(ctx context.Context, folder, page string) (*DriveListPage, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	return &DriveListPage{Files: []DriveResource{}}, nil
}
func (m *MockAPI) WikiResolve(ctx context.Context, token string) (*DriveResource, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	if strings.Contains(token, "denied") {
		return nil, fmt.Errorf("没有 Wiki 查看权限")
	}
	return &DriveResource{Token: "docmock" + token, Type: "docx", Name: "示例在线文档"}, nil
}
func (m *MockAPI) DriveMetas(ctx context.Context, refs []DriveRef) (map[string]DriveResource, error) {
	out := map[string]DriveResource{}
	for _, r := range refs {
		if r.Type == "file" {
			d, err := m.DriveMeta(ctx, r.Token)
			if err != nil {
				continue
			}
			out[r.Type+":"+r.Token] = DriveResource{Token: r.Token, Type: r.Type, Name: d.Title, URL: d.URL, OwnerID: d.OwnerID, Modified: fmt.Sprint(d.LatestModifyTime)}
		} else {
			out[r.Type+":"+r.Token] = DriveResource{Token: r.Token, Type: r.Type, Name: "在线文档 " + r.Token}
		}
	}
	return out, nil
}

func (m *MockAPI) DriveExport(ctx context.Context, token, kind string) (*Download, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	ext := exportExtension(kind)
	if ext == "" {
		return nil, fmt.Errorf("不支持此类型导出")
	}
	data := []byte("mock exported document " + token)
	return &Download{Body: io.NopCloser(bytes.NewReader(data)), Filename: "示例在线文档." + ext, ContentType: "application/octet-stream", Size: int64(len(data))}, nil
}
