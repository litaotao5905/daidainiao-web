package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type DriveRef struct {
	Token string `json:"doc_token"`
	Type  string `json:"doc_type"`
}
type DriveResource struct {
	Token    string `json:"token"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	OwnerID  string `json:"owner_id"`
	Modified string `json:"modified_time"`
}
type DriveListPage struct {
	Files    []DriveResource `json:"files"`
	HasMore  bool            `json:"has_more"`
	NextPage string          `json:"next_page_token"`
}

func (c *FeishuClient) DriveList(ctx context.Context, folder, page string) (*DriveListPage, error) {
	q := url.Values{"folder_token": {folder}, "page_size": {"200"}}
	if page != "" {
		q.Set("page_token", page)
	}
	var p DriveListPage
	err := c.call(ctx, idUser, "/drive/v1/files", q, &p)
	return &p, err
}

func (c *FeishuClient) WikiResolve(ctx context.Context, token string) (*DriveResource, error) {
	var d struct {
		Node struct {
			Token    string `json:"obj_token"`
			Type     string `json:"obj_type"`
			Title    string `json:"title"`
			Modified string `json:"obj_edit_time"`
		} `json:"node"`
	}
	if err := c.call(ctx, idUser, "/wiki/v2/spaces/get_node", url.Values{"token": {token}}, &d); err != nil {
		return nil, err
	}
	if d.Node.Token == "" || d.Node.Type == "" {
		return nil, fmt.Errorf("Wiki 未返回实际资源类型")
	}
	return &DriveResource{Token: d.Node.Token, Type: d.Node.Type, Name: d.Node.Title, Modified: d.Node.Modified}, nil
}

func (c *FeishuClient) DriveMetas(ctx context.Context, refs []DriveRef) (map[string]DriveResource, error) {
	out := map[string]DriveResource{}
	for start := 0; start < len(refs); start += 50 {
		end := min(start+50, len(refs))
		body, _ := json.Marshal(map[string]any{"request_docs": refs[start:end], "with_url": true})
		var d struct {
			Metas []struct {
				Token    string `json:"doc_token"`
				Type     string `json:"doc_type"`
				Title    string `json:"title"`
				URL      string `json:"url"`
				OwnerID  string `json:"owner_id"`
				Modified string `json:"latest_modify_time"`
			} `json:"metas"`
		}
		if err := c.callReq(ctx, idUser, http.MethodPost, "/drive/v1/metas/batch_query", nil, body, true, &d); err != nil {
			return out, err
		}
		for _, m := range d.Metas {
			out[m.Type+":"+m.Token] = DriveResource{Token: m.Token, Type: m.Type, Name: m.Title, URL: m.URL, OwnerID: m.OwnerID, Modified: m.Modified}
		}
	}
	return out, nil
}

func cloudMillis(v string) int64 {
	n, _ := strconv.ParseInt(v, 10, 64)
	if n > 0 && n < 1e11 {
		n *= 1000
	}
	return n
}

// The host is validated before extracting tokens; /drive/file and Wiki are
// resolved alongside the older /file format.
func driveRef(href string) (DriveRef, bool) {
	if !isDocURL(href) {
		return DriveRef{}, false
	}
	u, err := url.Parse(href)
	if err != nil {
		return DriveRef{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) > 0 && parts[0] == "drive" {
		parts = parts[1:]
	}
	if len(parts) < 2 || !keyRe.MatchString(parts[1]) {
		return DriveRef{}, false
	}
	t := map[string]string{"doc": "doc", "docs": "doc", "docx": "docx", "sheets": "sheet", "base": "bitable", "bitable": "bitable", "mindnotes": "mindnote", "slides": "slides", "minutes": "minutes", "file": "file", "folder": "folder", "wiki": "wiki"}[parts[0]]
	return DriveRef{Token: parts[1], Type: t}, t != ""
}

func classifyAttachment(a Attachment) Attachment {
	if r, ok := driveRef(a.URL); ok {
		a.ResourceToken, a.ResourceType = r.Token, r.Type
		switch r.Type {
		case "file":
			a.Kind, a.ResType, a.FileKey = "file", "drive", r.Token
			a.NoDownload = false
		case "folder":
			a.Kind = "folder"
		case "wiki":
			a.Kind = "unresolved"
		default:
			a.Kind = "doc"
		}
	}
	return a
}
