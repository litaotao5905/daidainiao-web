package main

import (
	"context"
	"strconv"
	"strings"
	"time"
)

type materialName struct {
	Name      string `json:"name"`
	Reference int64  `json:"reference,omitempty"`
	Failed    bool   `json:"failed,omitempty"`
}

type refreshMaterialNamesKey struct{}

func unknownMaterialName(a Attachment) bool {
	return a.ResourceToken != "" && a.ResourceType != "folder" &&
		(a.Name == "" || a.Name == a.URL || strings.HasPrefix(a.Name, "https://") || strings.HasPrefix(a.Name, "http://"))
}
func materialIdentity(a Attachment) string { return a.ResourceType + ":" + a.ResourceToken }
func materialNameCacheKey(uid string, a Attachment) string {
	return "material-name:" + materialIdentity(a) + "@" + uid
}

// Called with fileMu held. Known local names and persistent cached names are
// applied immediately. New references and explicit refreshes revalidate titles
// in the background without checking versions or downloading file bytes.
func (s *Service) startMaterialNamesLocked(rec *topicFileRecord, tc TopicCtx, force bool) {
	uid := s.auth.UserID()
	type localName struct {
		name    string
		checked int64
	}
	known := map[string]localName{}
	if s.dl != nil {
		for _, e := range s.dl.List(tc.TopicKey) {
			kind := e.ResourceType
			if e.ResType == "drive" {
				kind = "file"
			}
			if strings.HasPrefix(e.ResType, "export:") {
				kind = strings.TrimPrefix(e.ResType, "export:")
			}
			name := firstNonEmpty(e.CloudName, e.Name)
			if name == "" || strings.HasPrefix(name, "http://") || strings.HasPrefix(name, "https://") {
				name = e.NameHint
			}
			if name != "" && !strings.HasPrefix(name, "http://") && !strings.HasPrefix(name, "https://") {
				known[kind+":"+e.FileKey] = localName{name, max(e.LastChecked, e.Updated)}
			}
		}
	}
	if rec.nameAttempts == nil {
		rec.nameAttempts = map[string]time.Time{}
	}
	force = force && rec.nameJobs == 0
	var pending []Attachment
	now := time.Now()
	for i, a := range rec.attachments {
		if a.ResourceToken == "" || a.ResourceType == "folder" {
			continue
		}
		identity := materialIdentity(a)
		local := known[identity]
		if local.name != "" && unknownMaterialName(a) {
			rec.attachments[i].Name = local.name
		}
		var cached materialName
		if age, ok := s.cache.Get(materialNameCacheKey(uid, a), &cached); ok {
			if cached.Name != "" {
				rec.attachments[i].Name = cached.Name
				if s.dl != nil {
					s.dl.setCloudName(tc.TopicKey, a, cached.Name)
				}
			}
			if !force {
				if cached.Failed {
					if age < 5*time.Minute && cached.Reference >= a.LastReferenced {
						continue
					}
				} else if cached.Reference >= a.LastReferenced || (cached.Reference == 0 && time.Now().Add(-age).UnixMilli() >= a.LastReferenced) {
					continue
				}
			}
		} else if !force {
			if local.name != "" && local.checked >= a.LastReferenced {
				s.cache.Set(materialNameCacheKey(uid, a), materialName{Name: local.name, Reference: a.LastReferenced})
				continue
			}
			if !unknownMaterialName(a) && a.LastReferenced == 0 {
				continue
			}
		}
		attempt := identity + ":" + strconv.FormatInt(a.LastReferenced, 10)
		if !force && now.Before(rec.nameAttempts[attempt]) {
			continue
		}
		rec.nameAttempts[attempt] = now.Add(5 * time.Minute)
		pending = append(pending, a)
	}
	if len(pending) == 0 {
		return
	}
	if s.materialNameSem == nil {
		s.materialNameSem = make(chan struct{}, 2)
	}
	if rec.nameJobs == 0 {
		rec.done = make(chan struct{})
	}
	rec.nameJobs++
	rec.state.NamesLoading = true
	go s.loadMaterialNames(rec, tc, pending, uid, s.api, s.materialNameSem)
}

func (s *Service) loadMaterialNames(rec *topicFileRecord, tc TopicCtx, pending []Attachment, uid string, api API, sem chan struct{}) {
	defer func() {
		s.fileMu.Lock()
		defer s.fileMu.Unlock()
		rec.nameJobs--
		rec.state.NamesLoading = rec.nameJobs > 0
		if rec.nameJobs == 0 {
			close(rec.done)
		}
	}()
	sem <- struct{}{}
	defer func() { <-sem }()
	if s.auth.UserID() != uid {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	names := map[string]string{}
	var refs []DriveRef
	for _, a := range pending {
		if a.ResourceType != "wiki" {
			refs = append(refs, DriveRef{Token: a.ResourceToken, Type: a.ResourceType})
		}
	}
	if len(refs) > 0 {
		metas, _ := api.DriveMetas(ctx, refs)
		for identity, r := range metas {
			if strings.TrimSpace(r.Name) != "" {
				names[identity] = r.Name
			}
		}
	}
	for _, a := range pending {
		if a.ResourceType == "wiki" && ctx.Err() == nil {
			if r, err := api.WikiResolve(ctx, a.ResourceToken); err == nil && r != nil {
				names[materialIdentity(a)] = r.Name
			}
		}
	}
	if s.auth.UserID() != uid {
		return
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	requested := map[string]int64{}
	for _, a := range pending {
		identity := materialIdentity(a)
		requested[identity] = a.LastReferenced
		var existing materialName
		_, hit := s.cache.Get(materialNameCacheKey(uid, a), &existing)
		if hit && existing.Reference > a.LastReferenced && existing.Name != "" {
			names[identity] = existing.Name
			requested[identity] = existing.Reference
		}
		if !hit || existing.Reference <= a.LastReferenced {
			name := names[identity]
			failed := name == ""
			if failed {
				name = existing.Name
			}
			s.cache.Set(materialNameCacheKey(uid, a), materialName{Name: name, Reference: a.LastReferenced, Failed: failed})
		}
	}
	for i, a := range rec.attachments {
		identity := materialIdentity(a)
		if name := names[identity]; name != "" && requested[identity] >= a.LastReferenced {
			rec.attachments[i].Name = name
			if s.dl != nil {
				s.dl.setCloudName(tc.TopicKey, a, name)
			}
		}
	}
	if s.dl != nil {
		s.dl.Catalog(tc, rec.attachments)
	}
}

// Updating the cloud label never changes a saved path or local bytes.
func (m *DownloadManager) setCloudName(topic string, a Attachment, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, _, key, ok := dlSpec(a)
	if !ok {
		return
	}
	for _, e := range m.entries {
		if e.TopicKey == topic && e.ResType == rt && e.FileKey == key {
			e.CloudName = name
			e.NameHint = name
		}
	}
}
