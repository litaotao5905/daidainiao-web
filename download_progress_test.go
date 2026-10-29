package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type gatedProgressReader struct {
	stage   int
	release <-chan struct{}
}

func (r *gatedProgressReader) Read(b []byte) (int, error) {
	switch r.stage {
	case 0:
		r.stage++
		return copy(b, []byte("abcd")), nil
	case 1:
		<-r.release
		r.stage++
		return copy(b, []byte("efgh")), nil
	default:
		return 0, io.EOF
	}
}

func TestDownloadAPIReportsPartialBytesBeforeCompletion(t *testing.T) {
	e := newDLEnv(t)
	release := make(chan struct{})
	dir := t.TempDir()
	dm := NewDownloadManager(filepath.Join(dir, "files"), "", filepath.Join(dir, "index.json"), func(ctx context.Context, entry DLEntry, force bool) (*dlFetched, error) {
		p := filepath.Join(dir, "source")
		f, err := os.Create(p)
		if err != nil {
			return nil, err
		}
		_, err = copyWithDownloadProgress(ctx, f, &gatedProgressReader{release: release}, 8)
		f.Close()
		return &dlFetched{Path: p, Name: "progress.txt"}, err
	})
	e.svc.dl = dm
	a := att("file", "progress.txt", "progress-message", "progress-key")
	dm.Register(tc1, []Attachment{a})
	defer func() { close(release); waitEntry(t, dm, a) }()
	deadline := time.Now().Add(time.Second)
	for {
		r := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/downloads?topic="+tc1.TopicKey, nil)
		req.Host = "localhost:8080"
		e.h.ServeHTTP(r, req)
		var res downloadsResp
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &res) != nil {
			t.Fatalf("download status endpoint failed: %d %s", r.Code, r.Body.String())
		}
		if len(res.Items) == 1 && res.Items[0].DownloadedBytes == 4 {
			v := res.Items[0]
			if v.Status != dlRunning || v.TotalBytes != 8 || v.ContentHash != "" || v.CloudModTime != 0 {
				t.Fatalf("partial bytes committed prematurely: %+v", v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("partial progress not visible: %+v", res.Items)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestUnknownDownloadLengthStaysUnknown(t *testing.T) {
	var received, total int64
	ctx := context.WithValue(context.Background(), downloadProgressKey{}, downloadProgress(func(n, size int64) { received, total = n, size }))
	var dst bytes.Buffer
	n, err := copyWithDownloadProgress(ctx, &dst, bytes.NewBufferString("unknown length"), -1)
	if err != nil || received != n || n != 14 || total != 0 || dst.String() != "unknown length" {
		t.Fatalf("progress %d/%d copy %d %v", received, total, n, err)
	}
}
