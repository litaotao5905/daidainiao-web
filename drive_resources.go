package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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

type DriveSearchPage struct {
	Items   []DriveResource
	HasMore bool
	Total   int
}

func (c *FeishuClient) DriveSearchObjects(ctx context.Context, query string, docsTypes []string, offset, count int) (*DriveSearchPage, error) {
	body, _ := json.Marshal(struct {
		Query     string   `json:"search_key"`
		DocsTypes []string `json:"docs_types,omitempty"`
		Offset    int      `json:"offset"`
		Count     int      `json:"count"`
	}{query, docsTypes, offset, count})
	var data struct {
		Items []struct {
			Token   string `json:"docs_token"`
			Type    string `json:"docs_type"`
			Title   string `json:"title"`
			OwnerID string `json:"owner_id"`
			URL     string `json:"url"`
		} `json:"docs_entities"`
		HasMore bool `json:"has_more"`
		Total   int  `json:"total"`
	}
	if err := c.callReq(ctx, idUser, http.MethodPost, "/suite/docs-api/search/object", nil, body, true, &data); err != nil {
		return nil, err
	}
	page := &DriveSearchPage{Items: make([]DriveResource, 0, len(data.Items)), HasMore: data.HasMore, Total: data.Total}
	for _, item := range data.Items {
		page.Items = append(page.Items, DriveResource{Token: item.Token, Type: item.Type, Name: item.Title, OwnerID: item.OwnerID, URL: item.URL})
	}
	return page, nil
}

type WikiSpace struct {
	SpaceID string `json:"space_id"`
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
}

type WikiSpacePage struct {
	Items     []WikiSpace `json:"items"`
	HasMore   bool        `json:"has_more"`
	PageToken string      `json:"page_token"`
}

type WikiNodeDetail struct {
	SpaceID         string `json:"space_id"`
	NodeToken       string `json:"node_token"`
	ParentNodeToken string `json:"parent_node_token"`
	ObjToken        string `json:"obj_token"`
	ObjType         string `json:"obj_type"`
	Title           string `json:"title"`
	HasChild        bool   `json:"has_child"`
	URL             string `json:"url"`
	Modified        string `json:"obj_edit_time,omitempty"`
}

type WikiNodePage struct {
	Items     []WikiNodeDetail `json:"items"`
	HasMore   bool             `json:"has_more"`
	PageToken string           `json:"page_token"`
}

type WikiSearchNode struct {
	NodeToken string `json:"node_id"`
	SpaceID   string `json:"space_id"`
	Title     string `json:"title"`
	URL       string `json:"url"`
}

type WikiSearchPage struct {
	Items     []WikiSearchNode `json:"items"`
	HasMore   bool             `json:"has_more"`
	PageToken string           `json:"page_token"`
}

type WikiMoveResult struct {
	WikiToken string          `json:"wiki_token"`
	TaskID    string          `json:"task_id,omitempty"`
	Node      *WikiNodeDetail `json:"node,omitempty"`
}

func wikiPageQuery(pageToken string) url.Values {
	q := url.Values{"page_size": {"50"}}
	if pageToken != "" {
		q.Set("page_token", pageToken)
	}
	return q
}

func (c *FeishuClient) WikiSpaces(ctx context.Context, pageToken string) (*WikiSpacePage, error) {
	var page WikiSpacePage
	err := c.call(ctx, idUser, "/wiki/v2/spaces", wikiPageQuery(pageToken), &page)
	return &page, err
}

func (c *FeishuClient) WikiNodes(ctx context.Context, spaceID, parentNodeToken, pageToken string) (*WikiNodePage, error) {
	q := wikiPageQuery(pageToken)
	if parentNodeToken != "" {
		q.Set("parent_node_token", parentNodeToken)
	}
	var page WikiNodePage
	err := c.call(ctx, idUser, "/wiki/v2/spaces/"+url.PathEscape(spaceID)+"/nodes", q, &page)
	return &page, err
}

func (c *FeishuClient) WikiSearchNodes(ctx context.Context, query, spaceID, pageToken string) (*WikiSearchPage, error) {
	body, _ := json.Marshal(struct {
		Query   string `json:"query"`
		SpaceID string `json:"space_id,omitempty"`
	}{query, spaceID})
	var page WikiSearchPage
	if err := c.callReq(ctx, idUser, http.MethodPost, "/wiki/v1/nodes/search", wikiPageQuery(pageToken), body, true, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

func (c *FeishuClient) WikiNodeInfo(ctx context.Context, token string) (*WikiNodeDetail, error) {
	var data struct {
		Node *WikiNodeDetail `json:"node"`
	}
	if err := c.call(ctx, idUser, "/wiki/v2/spaces/get_node", url.Values{"token": {token}}, &data); err != nil {
		return nil, err
	}
	if data.Node == nil || data.Node.ObjToken == "" || data.Node.ObjType == "" {
		return nil, fmt.Errorf("Wiki 未返回实际资源类型")
	}
	if data.Node.URL == "" && data.Node.NodeToken != "" {
		data.Node.URL = wikiURL(c.base, data.Node.NodeToken)
	}
	return data.Node, nil
}

func wikiURL(baseURL, nodeToken string) string {
	return workspaceFeishuOrigin(baseURL) + "/wiki/" + url.PathEscape(nodeToken)
}

func workspaceFeishuOrigin(baseURL string) string {
	if strings.Contains(baseURL, "larksuite.com") {
		return "https://www.larksuite.com"
	}
	return "https://www.feishu.cn"
}

func (c *FeishuClient) WikiResolve(ctx context.Context, token string) (*DriveResource, error) {
	node, err := c.WikiNodeInfo(ctx, token)
	if err != nil {
		return nil, err
	}
	return &DriveResource{Token: node.ObjToken, Type: node.ObjType, Name: node.Title, Modified: node.Modified}, nil
}

func (c *FeishuClient) WikiMoveDocToSpace(ctx context.Context, spaceID, parentWikiToken, objType, objToken string) (*WikiMoveResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"parent_wiki_token": parentWikiToken, "obj_type": objType, "obj_token": objToken, "apply": false})
	var result WikiMoveResult
	path := "/wiki/v2/spaces/" + url.PathEscape(spaceID) + "/nodes/move_docs_to_wiki"
	if err := c.callReq(ctx, idUser, http.MethodPost, path, nil, body, false, &result); err != nil {
		return nil, err
	}
	if result.WikiToken != "" {
		return &result, nil
	}
	if result.TaskID == "" {
		return nil, fmt.Errorf("飞书未返回知识库节点或迁移任务标识")
	}
	return c.pollWikiMove(ctx, result.TaskID, time.Second)
}

func (c *FeishuClient) pollWikiMove(ctx context.Context, taskID string, interval time.Duration) (*WikiMoveResult, error) {
	for {
		var data struct {
			Task struct {
				MoveResult []struct {
					Node    *WikiNodeDetail `json:"node"`
					Status  *int            `json:"status"`
					Message string          `json:"status_msg"`
				} `json:"move_result"`
			} `json:"task"`
		}
		err := c.call(ctx, idUser, "/wiki/v2/tasks/"+url.PathEscape(taskID), url.Values{"task_type": {"move"}}, &data)
		if err != nil {
			return nil, fmt.Errorf("无法确认知识库迁移任务 %s 的结果：%w", taskID, err)
		}
		var node *WikiNodeDetail
		pending := len(data.Task.MoveResult) == 0
		for _, move := range data.Task.MoveResult {
			if move.Status == nil || *move.Status > 0 {
				pending = true
				continue
			}
			if *move.Status < 0 {
				return nil, fmt.Errorf("知识库迁移失败（状态 %d）：%s", *move.Status, move.Message)
			}
			if move.Node == nil || move.Node.NodeToken == "" {
				return nil, fmt.Errorf("飞书迁移任务 %s 未返回知识库节点", taskID)
			}
			node = move.Node
		}
		if !pending && node != nil {
			return &WikiMoveResult{WikiToken: node.NodeToken, TaskID: taskID, Node: node}, nil
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return nil, fmt.Errorf("尚未确认知识库迁移任务 %s 的结果：%w", taskID, err)
		}
	}
}

func (c *FeishuClient) DriveCopyFile(ctx context.Context, fileToken, docType, folderToken, name string) (*DriveResource, error) {
	body, _ := json.Marshal(map[string]string{"name": name, "type": docType, "folder_token": folderToken})
	var data struct {
		File *DriveResource `json:"file"`
	}
	if err := c.callReq(ctx, idUser, http.MethodPost, "/drive/v1/files/"+url.PathEscape(fileToken)+"/copy", nil, body, false, &data); err != nil {
		return nil, err
	}
	if data.File == nil || data.File.Token == "" || data.File.Token == fileToken {
		return nil, fmt.Errorf("飞书未返回有效的副本标识")
	}
	return data.File, nil
}

func (c *FeishuClient) DriveDeleteFile(ctx context.Context, token, docType string) error {
	return c.callReq(ctx, idUser, http.MethodDelete, "/drive/v1/files/"+url.PathEscape(token), url.Values{"type": {docType}}, nil, true, nil)
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
	if parts[0] == "wiki" && parts[1] == "space" && len(parts) > 2 {
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
