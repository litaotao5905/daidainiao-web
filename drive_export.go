package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func exportExtension(kind string) string {
	switch kind {
	case "doc", "docx":
		return "docx"
	case "sheet", "bitable":
		return "xlsx"
	case "slides":
		return "pptx"
	}
	return ""
}

// DriveExport creates one job and polls that same ticket, using the logged-in
// user throughout. A failed or rate-limited job never falls back to another identity.
func (c *FeishuClient) DriveExport(ctx context.Context, token, kind string) (*Download, error) {
	ext := exportExtension(kind)
	if ext == "" {
		return nil, fmt.Errorf("该在线资源暂不支持导出，请在飞书中打开")
	}
	body, _ := json.Marshal(map[string]string{"token": token, "type": kind, "file_extension": ext})
	var created struct {
		Ticket string `json:"ticket"`
	}
	if err := c.exportCall(ctx, http.MethodPost, "/drive/v1/export_tasks", nil, body, &created); err != nil {
		return nil, err
	}
	if created.Ticket == "" {
		return nil, fmt.Errorf("飞书未返回导出任务标识")
	}
	for {
		var data struct {
			Result struct {
				Status *int   `json:"job_status"`
				Token  string `json:"file_token"`
				Name   string `json:"file_name"`
				Error  string `json:"job_error_msg"`
			} `json:"result"`
		}
		if err := c.exportCall(ctx, http.MethodGet, "/drive/v1/export_tasks/"+url.PathEscape(created.Ticket), url.Values{"token": {token}}, nil, &data); err != nil {
			return nil, err
		}
		r := data.Result
		if r.Status == nil {
			return nil, fmt.Errorf("飞书未返回导出任务状态")
		}
		switch *r.Status {
		case 0:
			if r.Token == "" {
				return nil, fmt.Errorf("飞书未返回导出文件标识")
			}
			resp, _, err := c.roundTrip(ctx, idUser, http.MethodGet, "/drive/v1/export_tasks/file/"+url.PathEscape(r.Token)+"/download", nil)
			if err != nil {
				return nil, err
			}
			if resp.StatusCode != http.StatusOK {
				defer resp.Body.Close()
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				var env envelope
				_ = json.Unmarshal(b, &env)
				return nil, &FeishuError{Code: env.Code, Msg: firstNonEmpty(env.Msg, "导出文件下载失败"), HTTPStatus: resp.StatusCode}
			}
			name := firstNonEmpty(r.Name, token)
			if !strings.HasSuffix(strings.ToLower(name), "."+ext) {
				name += "." + ext
			}
			return &Download{Body: resp.Body, ContentType: resp.Header.Get("Content-Type"), Filename: name, Size: resp.ContentLength}, nil
		case 1, 2:
			timer := time.NewTimer(3 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		default:
			return nil, fmt.Errorf("飞书导出失败（状态 %d）：%s", *r.Status, firstNonEmpty(r.Error, "请检查下载/导出权限或文档大小后重试"))
		}
	}
}

func (s *Server) exportDocument(ctx context.Context, e DLEntry) (*dlFetched, error) {
	kind, token := strings.TrimPrefix(e.ResType, "export:"), e.FileKey
	if kind == "wiki" {
		r, err := s.svc.api.WikiResolve(ctx, token)
		if err != nil {
			return nil, err
		}
		kind, token = r.Type, r.Token
	}
	var d *Download
	var err error
	if kind == "file" {
		d, err = s.svc.api.DriveDownload(ctx, token)
	} else {
		d, err = s.svc.api.DriveExport(ctx, token, kind)
	}
	if err != nil {
		return nil, err
	}
	defer d.Body.Close()
	if err = os.MkdirAll(s.resDir, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(s.resDir, ".export-*")
	if err != nil {
		return nil, err
	}
	_, err = copyWithDownloadProgress(ctx, tmp, d.Body, d.Size)
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	return &dlFetched{ResourceType: kind, Path: tmp.Name(), Name: d.Filename, Mime: d.ContentType, Cleanup: func(bool) { os.Remove(tmp.Name()) }}, nil
}

// Export calls must stop on rate limits instead of retrying job creation.
func (c *FeishuClient) exportCall(ctx context.Context, method, path string, query url.Values, body []byte, out any) error {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token, err := c.accessToken(rctx, idUser)
	if err != nil {
		return err
	}
	if err = c.wait(rctx); err != nil {
		return err
	}
	target := c.base + apiPrefix + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(rctx, method, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return scrubErr(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	var env envelope
	if err = json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("飞书导出响应无效（HTTP %d）", resp.StatusCode)
	}
	if env.Code != 0 || resp.StatusCode >= 400 {
		return &FeishuError{Code: env.Code, Msg: firstNonEmpty(env.Msg, "飞书导出请求失败"), HTTPStatus: resp.StatusCode}
	}
	if out != nil {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}
