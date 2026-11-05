package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// DocxDocument represents the metadata of a Feishu Docx document.
type DocxDocument struct {
	DocumentID string `json:"document_id"`
	RevisionID int    `json:"revision_id"`
	Title      string `json:"title"`
}

// DocxRawContent represents raw markdown-style text of a Docx document.
type DocxRawContent struct {
	Content string `json:"content"`
	Lang    int    `json:"lang"`
}

// CreateDocx creates a new Feishu Docx document inside the specified folder.
func (c *FeishuClient) CreateDocx(ctx context.Context, folderToken, title string) (*DocxDocument, error) {
	reqBody, err := json.Marshal(map[string]string{
		"folder_token": folderToken,
		"title":        title,
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Document *DocxDocument `json:"document"`
	}
	if err := c.callReq(ctx, idUser, http.MethodPost, "/docx/v1/documents", nil, reqBody, true, &resp); err != nil {
		return nil, fmt.Errorf("创建飞书文档失败：%w", err)
	}
	if resp.Document == nil || resp.Document.DocumentID == "" {
		return nil, fmt.Errorf("创建飞书文档未返回 document_id")
	}
	return resp.Document, nil
}

// GetDocxMeta queries metadata (including latest revision_id) of a Docx document.
func (c *FeishuClient) GetDocxMeta(ctx context.Context, docToken string) (*DocxDocument, error) {
	var resp struct {
		Document *DocxDocument `json:"document"`
	}
	if err := c.call(ctx, idUser, "/docx/v1/documents/"+url.PathEscape(docToken), nil, &resp); err != nil {
		return nil, fmt.Errorf("获取飞书文档元信息失败：%w", err)
	}
	if resp.Document == nil {
		return nil, fmt.Errorf("未获取到文档元信息")
	}
	return resp.Document, nil
}

// GetDocxRawContent queries raw text (markdown style) of a Docx document.
func (c *FeishuClient) GetDocxRawContent(ctx context.Context, docToken string) (string, error) {
	q := url.Values{"lang": {"0"}}
	var resp struct {
		Content string `json:"content"`
	}
	if err := c.call(ctx, idUser, "/docx/v1/documents/"+url.PathEscape(docToken)+"/raw_content", q, &resp); err != nil {
		return "", fmt.Errorf("读取文档纯文本失败：%w", err)
	}
	return resp.Content, nil
}

// AppendDocxChildren appends child blocks to a parent block in a Docx document.
// If index is -1, it appends to the very end of the children list.
func (c *FeishuClient) AppendDocxChildren(ctx context.Context, docToken, parentBlockID string, children []map[string]any, index int) error {
	if len(children) == 0 {
		return nil
	}
	if parentBlockID == "" {
		parentBlockID = docToken // document root block ID is docToken itself
	}
	payload := map[string]any{
		"children": children,
	}
	if index >= 0 {
		payload["index"] = index
	}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/docx/v1/documents/%s/blocks/%s/children", url.PathEscape(docToken), url.PathEscape(parentBlockID))
	if err := c.callReq(ctx, idUser, http.MethodPost, path, nil, reqBody, true, nil); err != nil {
		return fmt.Errorf("追加文档内容失败：%w", err)
	}
	return nil
}

// GetDocxChildren retrieves direct child blocks of a parent block in a Docx document.
func (c *FeishuClient) GetDocxChildren(ctx context.Context, docToken, parentBlockID string) ([]map[string]any, error) {
	if parentBlockID == "" {
		parentBlockID = docToken
	}
	q := url.Values{"page_size": {"500"}}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	path := fmt.Sprintf("/docx/v1/documents/%s/blocks/%s/children", url.PathEscape(docToken), url.PathEscape(parentBlockID))
	if err := c.call(ctx, idUser, path, q, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// ClearDocxChildren deletes all child blocks under the document root block.
func (c *FeishuClient) ClearDocxChildren(ctx context.Context, docToken string) error {
	items, err := c.GetDocxChildren(ctx, docToken, docToken)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	payload := map[string]any{
		"start_index": 0,
		"end_index":   len(items),
	}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/docx/v1/documents/%s/blocks/%s/children/batch_delete", url.PathEscape(docToken), url.PathEscape(docToken))
	return c.callReq(ctx, idUser, http.MethodDelete, path, nil, reqBody, true, nil)
}

// RewriteDocxContent clears existing root children and writes new blocks,
// guaranteeing that only the selected context is present in the document.
func (c *FeishuClient) RewriteDocxContent(ctx context.Context, docToken string, blocks []map[string]any) error {
	_ = c.ClearDocxChildren(ctx, docToken)
	return c.AppendDocxChildren(ctx, docToken, "", blocks, -1)
}

// DocxURL returns the full web URL for a Docx document.
func (c *FeishuClient) DocxURL(ctx context.Context, token string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"request_docs": []map[string]string{{"doc_token": token, "doc_type": "docx"}},
		"with_url":     true,
	})
	var d struct {
		Metas []struct {
			URL string `json:"url"`
		} `json:"metas"`
	}
	if err := c.callReq(ctx, idUser, http.MethodPost, "/drive/v1/metas/batch_query", nil, body, true, &d); err == nil && len(d.Metas) > 0 && d.Metas[0].URL != "" {
		return d.Metas[0].URL, nil
	}
	return "https://open.feishu.cn/docx/" + token, nil
}

// FolderURL returns the full web URL for a Drive folder.
func (c *FeishuClient) FolderURL(ctx context.Context, token string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"request_docs": []map[string]string{{"doc_token": token, "doc_type": "folder"}},
		"with_url":     true,
	})
	var d struct {
		Metas []struct {
			URL string `json:"url"`
		} `json:"metas"`
	}
	if err := c.callReq(ctx, idUser, http.MethodPost, "/drive/v1/metas/batch_query", nil, body, true, &d); err == nil && len(d.Metas) > 0 && d.Metas[0].URL != "" {
		return d.Metas[0].URL, nil
	}
	return "https://open.feishu.cn/drive/folder/" + token, nil
}

// ensureEncryptedTopicFolder creates or returns the private folder under "网页呆呆鸟-加密版/<topic>".
func (c *FeishuClient) ensureEncryptedTopicFolder(ctx context.Context, topic string) (string, string, error) {
	c.folderMu.Lock()
	defer c.folderMu.Unlock()
	root, err := c.rootFolder(ctx)
	if err != nil {
		return "", "", err
	}
	base, err := c.privateFolder(ctx, root, EncryptedDrivePrivateRootName)
	if err != nil {
		return "", "", err
	}
	if topic == "" {
		return base, EncryptedDrivePrivateRootName, nil
	}
	folder, err := c.privateFolder(ctx, base, topic)
	return folder, EncryptedDrivePrivateRootName + "/" + topic, err
}
