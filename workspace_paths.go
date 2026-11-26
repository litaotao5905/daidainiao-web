package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type workspaceBreadcrumb struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type workspacePlace struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Icon  string `json:"icon"`
	Path  string `json:"path"`
}

type workspaceLocalMatch struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Hint string `json:"hint"`
}

func expandTildePath(path, home string) string {
	path = strings.TrimSpace(path)
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return filepath.Join(home, strings.TrimLeft(path[2:], `/\`))
	}
	return path
}

func (s *Service) workspaceHomeDir() string {
	if s.cfg.HostHome != "" {
		return s.cfg.HostHome
	}
	home, _ := os.UserHomeDir()
	return home
}

func (s *Service) workspaceMappingEnabled() bool {
	return s.cfg.HostHome != "" && s.cfg.ContainerHome != ""
}

func (s *Service) workspaceResolveContainer(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", badRequest("请选择本地文件夹")
	}
	expanded := expandTildePath(input, s.workspaceHomeDir())
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(abs)
	if s.workspaceMappingEnabled() {
		if rel, err := filepath.Rel(s.cfg.ContainerHome, clean); err == nil && workspaceRelative(rel) {
			return clean, nil
		}
		if rel, err := filepath.Rel(s.cfg.HostHome, clean); err == nil && workspaceRelative(rel) {
			return filepath.Join(s.cfg.ContainerHome, rel), nil
		}
	}
	if s.cfg.HostDownloadDir != "" && s.cfg.DownloadDir != "" {
		if rel, err := filepath.Rel(s.cfg.HostDownloadDir, clean); err == nil && workspaceRelative(rel) {
			return filepath.Join(s.cfg.DownloadDir, rel), nil
		}
	}
	return clean, nil
}

func (s *Service) workspaceDisplayPath(containerPath string) string {
	containerPath = filepath.Clean(containerPath)
	if s.workspaceMappingEnabled() {
		containerHome := filepath.Clean(s.cfg.ContainerHome)
		if rel, err := filepath.Rel(containerHome, containerPath); err == nil && workspaceRelative(rel) {
			hostHome := filepath.Clean(s.cfg.HostHome)
			if rel == "." {
				return "~"
			}
			hostPath := filepath.Join(hostHome, rel)
			return workspacePreferTilde(hostPath, hostHome)
		}
	}
	if s.cfg.HostDownloadDir != "" && s.cfg.DownloadDir != "" {
		download := filepath.Clean(s.cfg.DownloadDir)
		if rel, err := filepath.Rel(download, containerPath); err == nil && workspaceRelative(rel) {
			return filepath.Join(filepath.Clean(s.cfg.HostDownloadDir), rel)
		}
	}
	return containerPath
}

func workspacePreferTilde(absPath, home string) string {
	absPath = filepath.Clean(absPath)
	home = filepath.Clean(home)
	if absPath == home {
		return "~"
	}
	if rel, err := filepath.Rel(home, absPath); err == nil && workspaceRelative(rel) {
		return filepath.ToSlash(filepath.Join("~", rel))
	}
	return absPath
}

func (s *Service) workspaceLocalRoot(stored string) (string, error) {
	container, err := s.workspaceResolveContainer(stored)
	if err != nil {
		return "", err
	}
	return realDirectory(container)
}

func workspaceEvalClean(path string) string {
	clean := filepath.Clean(path)
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return clean
	}
	return real
}

func (s *Service) workspaceOpenerHostPath(containerPath string) string {
	containerPath = workspaceEvalClean(containerPath)
	if s.workspaceMappingEnabled() {
		containerHome := workspaceEvalClean(s.cfg.ContainerHome)
		if rel, err := filepath.Rel(containerHome, containerPath); err == nil && workspaceRelative(rel) {
			return filepath.Join(workspaceEvalClean(s.cfg.HostHome), rel)
		}
	}
	if s.cfg.HostDownloadDir != "" && s.cfg.DownloadDir != "" {
		download := filepath.Clean(s.cfg.DownloadDir)
		if rel, err := filepath.Rel(download, containerPath); err == nil && workspaceRelative(rel) {
			return filepath.Join(filepath.Clean(s.cfg.HostDownloadDir), rel)
		}
	}
	return containerPath
}

func workspaceHiddenDir(name string) bool {
	return strings.HasPrefix(name, ".")
}

func (s *Service) workspaceLocalPlaces() []workspacePlace {
	home := s.workspaceHomeDir()
	candidates := []workspacePlace{
		{ID: "desktop", Label: "桌面", Icon: "🖥", Path: workspacePreferTilde(filepath.Join(home, "Desktop"), home)},
		{ID: "documents", Label: "文稿", Icon: "📄", Path: workspacePreferTilde(filepath.Join(home, "Documents"), home)},
		{ID: "downloads", Label: "下载", Icon: "⬇️", Path: workspacePreferTilde(filepath.Join(home, "Downloads"), home)},
		{ID: "home", Label: "个人主目录", Icon: "🏠", Path: "~"},
	}
	if s.cfg.HostDownloadDir != "" {
		candidates = append(candidates, workspacePlace{ID: "feishu_downloads", Label: "飞书下载目录", Icon: "📥", Path: s.cfg.HostDownloadDir})
	} else if s.cfg.DownloadDir != "" {
		candidates = append(candidates, workspacePlace{ID: "feishu_downloads", Label: "飞书下载目录", Icon: "📥", Path: workspacePreferTilde(s.cfg.DownloadDir, home)})
	}
	out := make([]workspacePlace, 0, len(candidates))
	for _, place := range candidates {
		container, err := s.workspaceResolveContainer(place.Path)
		if err != nil {
			continue
		}
		real, err := realDirectory(container)
		if err != nil {
			continue
		}
		place.Path = s.workspaceDisplayPath(real)
		out = append(out, place)
	}
	return out
}

func (s *Service) workspaceBrowseDir(input string) (map[string]any, error) {
	container, err := s.workspaceResolveContainer(input)
	if err != nil {
		return nil, err
	}
	real, err := realDirectory(container)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		return nil, err
	}
	type directory struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	items := []directory{}
	for _, e := range entries {
		if workspaceHiddenDir(e.Name()) {
			continue
		}
		path := filepath.Join(real, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			path, err = realDirectory(path)
			if err != nil {
				continue
			}
		} else if !e.IsDir() {
			continue
		}
		items = append(items, directory{Name: e.Name(), Path: s.workspaceDisplayPath(path)})
	}
	sort.Slice(items, func(i, j int) bool { return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name) })
	parent := ""
	if parentReal := filepath.Dir(real); parentReal != real {
		parent = s.workspaceDisplayPath(parentReal)
	}
	breadcrumbs := s.workspaceBreadcrumbs(real)
	return map[string]any{
		"dir":         s.workspaceDisplayPath(real),
		"parent":      parent,
		"breadcrumbs": breadcrumbs,
		"items":       items,
	}, nil
}

func (s *Service) workspaceSearchLocalRoot(input string) (string, string, error) {
	container, err := s.workspaceResolveContainer(input)
	if err != nil {
		return "", "", err
	}
	inHome := input == "~" || strings.HasPrefix(input, "~/") || strings.HasPrefix(input, `~\`)
	if !inHome && !filepath.IsAbs(input) && !workspaceRelative(filepath.Clean(input)) {
		return "", "", ErrOutsideDir
	}
	real, err := realDirectory(container)
	if err != nil {
		return "", "", err
	}
	roots := []string{s.cfg.ContainerHome}
	if s.cfg.HostDownloadDir != "" {
		roots = append(roots, s.cfg.DownloadDir)
	}
	if inHome {
		home, err := s.workspaceResolveContainer("~")
		if err != nil {
			return "", "", err
		}
		rel, err := filepath.Rel(workspaceEvalClean(home), real)
		if err != nil || !workspaceRelative(rel) {
			return "", "", ErrOutsideDir
		}
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		if rel, err := filepath.Rel(root, container); err == nil && workspaceRelative(rel) {
			if rel, err := filepath.Rel(workspaceEvalClean(root), real); err != nil || !workspaceRelative(rel) {
				return "", "", ErrOutsideDir
			}
		}
	}
	return container, real, nil
}

func (s *Service) workspaceSearchLocal(ctx context.Context, query, dir string) ([]workspaceLocalMatch, error) {
	const maxDepth, maxDirs, maxResults = 4, 1200, 40
	type directory struct {
		path  string
		real  string
		depth int
	}
	roots := []string{dir}
	if dir == "" {
		roots = []string{"~/Desktop", "~/Documents", "~/Downloads"}
		if s.cfg.DownloadDir != "" {
			roots = append(roots, s.cfg.DownloadDir)
		}
	}
	queue := []directory{}
	seen := map[string]bool{}
	for _, root := range roots {
		path, real, err := s.workspaceSearchLocalRoot(root)
		if err != nil {
			if dir != "" {
				return nil, err
			}
			continue
		}
		if !seen[real] {
			queue = append(queue, directory{path: path, real: real})
			seen[real] = true
		}
	}
	matches := []workspaceLocalMatch{}
	// Search descendants without following symlinks, including links to another search root.
	for i := 0; i < len(queue) && len(matches) < maxResults; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parent := queue[i]
		if parent.depth > 0 && workspaceNameMatches(filepath.Base(parent.path), query) {
			matches = append(matches, workspaceLocalMatch{
				Name: filepath.Base(parent.path), Path: s.workspaceDisplayPath(parent.path),
				Hint: s.workspaceDisplayPath(filepath.Dir(parent.path)),
			})
		}
		if parent.depth == maxDepth || len(queue) == maxDirs || len(matches) == maxResults {
			continue
		}
		f, err := os.Open(parent.real)
		if err != nil {
			if i == 0 && dir != "" {
				return nil, err
			}
			continue
		}
		for ctx.Err() == nil && len(queue) < maxDirs {
			entries, err := f.ReadDir(128)
			for _, entry := range entries {
				if !entry.IsDir() || workspaceHiddenDir(entry.Name()) {
					continue
				}
				real := filepath.Join(parent.real, entry.Name())
				if !seen[real] {
					queue = append(queue, directory{filepath.Join(parent.path, entry.Name()), real, parent.depth + 1})
					seen[real] = true
				}
				if len(queue) == maxDirs {
					break
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && i == 0 && dir != "" {
					f.Close()
					return nil, err
				}
				break
			}
		}
		f.Close()
	}
	return matches, ctx.Err()
}

func (s *Service) workspaceBreadcrumbs(real string) []workspaceBreadcrumb {
	real = filepath.Clean(real)
	stop := ""
	if s.workspaceMappingEnabled() {
		stop = filepath.Clean(s.cfg.ContainerHome)
	} else if home, err := os.UserHomeDir(); err == nil {
		stop = filepath.Clean(home)
	}
	var crumbs []workspaceBreadcrumb
	cursor := real
	for {
		crumbs = append([]workspaceBreadcrumb{{
			Name: breadcrumbName(s, cursor, stop),
			Path: s.workspaceDisplayPath(cursor),
		}}, crumbs...)
		parent := filepath.Dir(cursor)
		if parent == cursor || cursor == stop {
			break
		}
		cursor = parent
	}
	return crumbs
}

func breadcrumbName(s *Service, path, stop string) string {
	if path == stop {
		return "~"
	}
	name := filepath.Base(path)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return s.workspaceDisplayPath(path)
	}
	return name
}
