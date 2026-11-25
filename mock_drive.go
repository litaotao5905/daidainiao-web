package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	files := append([]DriveResource{}, m.workspaceDrive[folder]...)
	switch folder {
	case "mockDriveRoot":
		files = append(files, mockDriveFolder("mockWorkspaceFolder", "示例项目资料"))
	case "mockWorkspaceFolder":
		files = append(files, mockDriveFolder("mockWorkspaceReports", "归档"))
	case "mockWorkspaceReports":
		files = append(files, mockWorkspaceBudget)
	}
	return &DriveListPage{Files: files}, nil
}

var mockWorkspaceBudget = DriveResource{Token: "boxmockseed01", Type: "file", Name: "示例预算.txt", URL: "https://mock.feishu.cn/file/boxmockseed01"}

func (m *MockAPI) DriveSearchObjects(ctx context.Context, query string, docsTypes []string, offset, count int) (*DriveSearchPage, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(docsTypes))
	for _, kind := range docsTypes {
		allowed[kind] = true
	}
	matches := []DriveResource{}
	seen := map[string]bool{}
	add := func(item DriveResource) {
		if item.Type == "folder" || seen[item.Token] || !workspaceNameMatches(item.Name, query) {
			return
		}
		if len(allowed) > 0 && !allowed[item.Type] && !(item.Type == "docx" && allowed["doc"]) {
			return
		}
		matches = append(matches, item)
		seen[item.Token] = true
	}
	add(mockWorkspaceBudget)
	m.mu.RLock()
	for _, items := range m.workspaceDrive {
		for _, item := range items {
			add(item)
		}
	}
	m.mu.RUnlock()
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Name == matches[j].Name {
			return matches[i].Token < matches[j].Token
		}
		return matches[i].Name < matches[j].Name
	})
	start := min(max(offset, 0), len(matches))
	end := start + min(max(count, 0), len(matches)-start)
	return &DriveSearchPage{Items: matches[start:end], Total: len(matches), HasMore: end < len(matches)}, nil
}

func (m *MockAPI) WikiResolve(ctx context.Context, token string) (*DriveResource, error) {
	node, err := m.WikiNodeInfo(ctx, token)
	if err != nil {
		return nil, err
	}
	return &DriveResource{Token: node.ObjToken, Type: node.ObjType, Name: node.Title}, nil
}

func mockDriveFolder(token, name string) DriveResource {
	return DriveResource{Token: token, Type: "folder", Name: name, URL: "https://mock.feishu.cn/drive/folder/" + token}
}

func (m *MockAPI) WikiSpaces(ctx context.Context, pageToken string) (*WikiSpacePage, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	return &WikiSpacePage{Items: []WikiSpace{{SpaceID: "mockWikiSpace", Name: "示例项目知识库", URL: "https://mock.feishu.cn/wiki/space/mockWikiSpace"}}}, nil
}

func mockWikiNode(token string) WikiNodeDetail {
	node := WikiNodeDetail{SpaceID: "mockWikiSpace", NodeToken: token, ObjToken: "docmock" + token, ObjType: "docx", Title: "示例在线文档", URL: "https://mock.feishu.cn/wiki/" + token}
	switch token {
	case "mockWikiHome":
		node.Title, node.HasChild = "项目首页", true
	case "mockWikiArchive":
		node.Title, node.ParentNodeToken = "归档", "mockWikiHome"
	}
	return node
}

func (m *MockAPI) WikiNodes(ctx context.Context, spaceID, parentNodeToken, pageToken string) (*WikiNodePage, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	p := &WikiNodePage{Items: []WikiNodeDetail{}}
	if spaceID == "mockWikiSpace" {
		switch parentNodeToken {
		case "":
			p.Items = append(p.Items, mockWikiNode("mockWikiHome"))
		case "mockWikiHome":
			p.Items = append(p.Items, mockWikiNode("mockWikiArchive"))
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	parents := map[string]bool{}
	for _, n := range m.workspaceWiki {
		if n.SpaceID == spaceID {
			parents[n.ParentNodeToken] = true
			if n.ParentNodeToken == parentNodeToken {
				p.Items = append(p.Items, n)
			}
		}
	}
	for i := range p.Items {
		p.Items[i].HasChild = p.Items[i].HasChild || parents[p.Items[i].NodeToken]
	}
	sort.Slice(p.Items, func(i, j int) bool { return p.Items[i].Title < p.Items[j].Title })
	return p, nil
}

func (m *MockAPI) WikiSearchNodes(ctx context.Context, query, spaceID, pageToken string) (*WikiSearchPage, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	matches := []WikiSearchNode{}
	add := func(n WikiNodeDetail) {
		if (spaceID == "" || n.SpaceID == spaceID) && workspaceNameMatches(n.Title, query) {
			matches = append(matches, WikiSearchNode{NodeToken: n.NodeToken, SpaceID: n.SpaceID, Title: n.Title, URL: n.URL})
		}
	}
	add(mockWikiNode("mockWikiHome"))
	add(mockWikiNode("mockWikiArchive"))
	m.mu.RLock()
	for _, n := range m.workspaceWiki {
		add(n)
	}
	m.mu.RUnlock()
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Title == matches[j].Title {
			return matches[i].NodeToken < matches[j].NodeToken
		}
		return matches[i].Title < matches[j].Title
	})
	offset, _ := strconv.Atoi(pageToken)
	start := min(max(offset, 0), len(matches))
	end := min(start+50, len(matches))
	page := &WikiSearchPage{Items: matches[start:end], HasMore: end < len(matches)}
	if page.HasMore {
		page.PageToken = strconv.Itoa(end)
	}
	return page, nil
}

func (m *MockAPI) WikiNodeInfo(ctx context.Context, token string) (*WikiNodeDetail, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	if strings.Contains(token, "denied") {
		return nil, fmt.Errorf("没有 Wiki 查看权限")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.workspaceWiki[token]
	if !ok {
		n = mockWikiNode(token)
	}
	for _, child := range m.workspaceWiki {
		if child.ParentNodeToken == token {
			n.HasChild = true
			break
		}
	}
	if n.Modified == "" {
		if ms := m.docModTimes["wiki:"+token]; ms > 0 {
			n.Modified = strconv.FormatInt(ms, 10)
		}
	}
	return &n, nil
}

func (m *MockAPI) WikiMoveDocToSpace(ctx context.Context, spaceID, parentWikiToken, objType, objToken string) (*WikiMoveResult, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var source DriveResource
	for folder, files := range m.workspaceDrive {
		for i, file := range files {
			if file.Token == objToken {
				source = file
				m.workspaceDrive[folder] = append(files[:i], files[i+1:]...)
				break
			}
		}
		if source.Token != "" {
			break
		}
	}
	if source.Token == "" || source.Type != objType {
		return nil, fmt.Errorf("没有可移动的云盘副本")
	}
	m.nextSeq++
	token := fmt.Sprintf("mockWikiMoved%d", m.nextSeq)
	node := WikiNodeDetail{SpaceID: spaceID, NodeToken: token, ParentNodeToken: parentWikiToken, ObjToken: objToken, ObjType: objType, Title: source.Name, URL: "https://mock.feishu.cn/wiki/" + token}
	if m.workspaceWiki == nil {
		m.workspaceWiki = map[string]WikiNodeDetail{}
	}
	m.workspaceWiki[token] = node
	return &WikiMoveResult{WikiToken: token, Node: &node}, nil
}

func (m *MockAPI) DriveRootFolder(ctx context.Context) (string, error) {
	return "mockDriveRoot", m.gate(ctx)
}

func (m *MockAPI) DriveEnsurePrivateSubfolder(ctx context.Context, subName string) (string, error) {
	if err := m.gate(ctx); err != nil {
		return "", err
	}
	return "mockPrivate" + subName, nil
}

func (m *MockAPI) DriveUploadToFolder(ctx context.Context, folderToken, name string, data io.Reader, size int64) (string, error) {
	content, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	if int64(len(content)) != size {
		return "", fmt.Errorf("文件大小已变化")
	}
	token, err := m.DriveUpload(ctx, name, content)
	if err != nil {
		return "", err
	}
	m.SetDriveFile(token, name, content, time.Now().UnixMilli())
	m.addMockWorkspaceDrive(folderToken, DriveResource{Token: token, Type: "file", Name: name, URL: "https://mock.feishu.cn/file/" + token})
	return token, nil
}

func (m *MockAPI) addMockWorkspaceDrive(folder string, file DriveResource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workspaceDrive == nil {
		m.workspaceDrive = map[string][]DriveResource{}
	}
	m.workspaceDrive[folder] = append(m.workspaceDrive[folder], file)
}

func (m *MockAPI) DriveCopyFile(ctx context.Context, fileToken, docType, folderToken, name string) (*DriveResource, error) {
	if docType == "file" {
		d, err := m.DriveDownload(ctx, fileToken)
		if err != nil {
			return nil, err
		}
		defer d.Body.Close()
		token, err := m.DriveUploadToFolder(ctx, folderToken, name, d.Body, d.Size)
		if err != nil {
			return nil, err
		}
		return &DriveResource{Token: token, Type: docType, Name: name, URL: "https://mock.feishu.cn/file/" + token}, nil
	}
	token, err := m.DriveUpload(ctx, name, nil)
	if err != nil {
		return nil, err
	}
	file := DriveResource{Token: token, Type: docType, Name: name, URL: workspaceResourceURL("https://mock.feishu.cn", docType, token)}
	m.addMockWorkspaceDrive(folderToken, file)
	return &file, nil
}

func (m *MockAPI) DriveDeleteFile(ctx context.Context, token, docType string) error {
	if err := m.gate(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for folder, files := range m.workspaceDrive {
		for i, f := range files {
			if f.Token == token && f.Type == docType {
				m.workspaceDrive[folder] = append(files[:i], files[i+1:]...)
				break
			}
		}
	}
	delete(m.driveNames, token)
	delete(m.driveFiles, token)
	delete(m.driveModTimes, token)
	delete(m.driveMembers, token)
	delete(m.privateClosed, token)
	delete(m.linkShares, token)
	return nil
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
			m.mu.RLock()
			mod := m.docModTimes[r.Type+":"+r.Token]
			m.mu.RUnlock()
			modified := ""
			if mod > 0 {
				modified = strconv.FormatInt(mod, 10)
			}
			out[r.Type+":"+r.Token] = DriveResource{Token: r.Token, Type: r.Type, Name: "在线文档 " + r.Token, Modified: modified}
		}
	}
	return out, nil
}

func (m *MockAPI) SetDocModified(kind, token string, ms int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.docModTimes == nil {
		m.docModTimes = map[string]int64{}
	}
	m.docModTimes[kind+":"+token] = ms
}

func (m *MockAPI) DriveExport(ctx context.Context, token, kind string) (*Download, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	ext := exportExtension(kind)
	if ext == "" {
		return nil, fmt.Errorf("不支持此类型导出")
	}
	data, err := mockOfficeFile(ext, "飞书模拟导出 "+token)
	if err != nil {
		return nil, err
	}
	return &Download{Body: io.NopCloser(bytes.NewReader(data)), Filename: "示例在线文档." + ext, ContentType: "application/octet-stream", Size: int64(len(data))}, nil
}

func mockOfficeFile(ext, text string) ([]byte, error) {
	const office = "http://schemas.openxmlformats.org/"
	const relations = office + "officeDocument/2006/relationships"
	files := map[string]string{}
	var label bytes.Buffer
	_ = xml.EscapeText(&label, []byte(text))
	var mainPart, contentType string
	switch ext {
	case "docx":
		mainPart, contentType = "word/document.xml", "wordprocessingml.document.main+xml"
		files[mainPart] = `<w:document xmlns:w="` + office + `wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + label.String() + `</w:t></w:r></w:p></w:body></w:document>`
	case "xlsx":
		mainPart, contentType = "xl/workbook.xml", "spreadsheetml.sheet.main+xml"
		ns := office + "spreadsheetml/2006/main"
		files[mainPart] = `<workbook xmlns="` + ns + `" xmlns:r="` + relations + `"><sheets><sheet name="示例" sheetId="1" r:id="rId1"/></sheets></workbook>`
		files["xl/_rels/workbook.xml.rels"] = mockOfficeRelationship("worksheet", "worksheets/sheet1.xml")
		files["xl/worksheets/sheet1.xml"] = `<worksheet xmlns="` + ns + `"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>` + label.String() + `</t></is></c></row></sheetData></worksheet>`
	case "pptx":
		mainPart, contentType = "ppt/presentation.xml", "presentationml.presentation.main+xml"
		files[mainPart] = `<p:presentation xmlns:p="` + office + `presentationml/2006/main"><p:sldSz cx="9144000" cy="6858000"/><p:notesSz cx="6858000" cy="9144000"/></p:presentation>`
	default:
		return nil, fmt.Errorf("不支持此 Office 格式")
	}
	extra := ""
	if ext == "xlsx" {
		extra = `<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`
	}
	files["[Content_Types].xml"] = `<Types xmlns="` + office + `package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Override PartName="/` + mainPart + `" ContentType="application/vnd.openxmlformats-officedocument.` + contentType + `"/>` + extra + `</Types>`
	files["_rels/.rels"] = mockOfficeRelationship("officeDocument", mainPart)
	var data bytes.Buffer
	archive := zip.NewWriter(&data)
	for name, content := range files {
		part, err := archive.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(part, content); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

func mockOfficeRelationship(kind, target string) string {
	return `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/` + kind + `" Target="` + target + `"/></Relationships>`
}

// The mock keeps Feishu-shaped URLs in API responses. The browser opens those
// samples here so inline files and folder navigation work without a tenant.
func (s *Server) handleMockWorkspacePreview(w http.ResponseWriter, r *http.Request) {
	uid, loggedIn := s.workspaceUser(w, r)
	if !loggedIn {
		return
	}
	mock, ok := s.svc.api.(*MockAPI)
	token, kind := r.PathValue("token"), r.PathValue("kind")
	if !ok || !keyRe.MatchString(token) {
		http.NotFound(w, r)
		return
	}
	ctx := workspaceContext(r, uid)
	w.Header().Set("Cache-Control", "private, no-store")
	title := "模拟飞书预览"
	var links []DriveResource
	switch kind {
	case "file":
		mock.mu.RLock()
		data, loaded := mock.driveFiles[token]
		name := mock.driveNames[token]
		mock.mu.RUnlock()
		if name == "" {
			http.NotFound(w, r)
			return
		}
		if !loaded {
			data = []byte("mock drive file " + token + "\n")
		}
		title = name
		ct := contentTypeFor("", name, func() string { return http.DetectContentType(data) })
		download := r.URL.Query().Get("dl") == "1"
		if download || inlineTypes[ct] {
			disposition := "inline"
			if download {
				disposition = "attachment"
			}
			w.Header().Set("Content-Type", ct)
			if strings.HasPrefix(ct, "text/") {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			}
			w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
			return
		}
		links = append(links, DriveResource{Name: "下载模拟文件", URL: "?dl=1"})
	case "folder":
		page, err := mock.DriveList(ctx, token, "")
		if err != nil {
			writeErr(w, err)
			return
		}
		title = "示例飞书云盘文件夹"
		links = page.Files
	case "wiki", "space":
		space, parent := token, ""
		title = "示例知识库"
		if kind == "wiki" {
			node, err := mock.WikiNodeInfo(ctx, token)
			if err != nil {
				writeErr(w, err)
				return
			}
			title, space, parent = node.Title, node.SpaceID, node.NodeToken
			if node.ObjType == "file" {
				links = append(links, DriveResource{Name: node.Title, Type: "file", Token: node.ObjToken})
			}
		}
		page, err := mock.WikiNodes(ctx, space, parent, "")
		if err != nil {
			writeErr(w, err)
			return
		}
		for _, node := range page.Items {
			links = append(links, DriveResource{Name: node.Title, Type: "wiki", Token: node.NodeToken})
		}
	default:
		if workspaceResourceURL("", kind, token) == "" {
			http.NotFound(w, r)
			return
		}
		title = "示例飞书文档 · " + token
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>%s</title><style>body{font:16px/1.6 system-ui;margin:48px auto;padding:0 20px;max-width:680px}</style><p>飞书模拟预览</p><h1>%s</h1>`, html.EscapeString(title), html.EscapeString(title))
	for _, link := range links {
		href := "?dl=1"
		if link.Token != "" {
			href = "/api/mock/workspace/" + link.Type + "/" + link.Token
		}
		fmt.Fprintf(w, `<p><a href="%s">%s</a></p>`, html.EscapeString(href), html.EscapeString(link.Name))
	}
	fmt.Fprint(w, "</html>")
}
