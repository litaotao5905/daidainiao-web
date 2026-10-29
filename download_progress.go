package main

import (
	"context"
	"io"
	"time"
)

type downloadProgressKey struct{}
type downloadProgress func(received, total int64)

// Report the bytes actually received. A zero total means unknown length;
// callers must display indeterminate progress instead of guessing a percentage.
func copyWithDownloadProgress(ctx context.Context, dst io.Writer, src io.Reader, total int64) (int64, error) {
	report, _ := ctx.Value(downloadProgressKey{}).(downloadProgress)
	if report == nil {
		return io.Copy(dst, src)
	}
	if total < 0 {
		total = 0
	}
	report(0, total)
	r := &downloadProgressReader{src: src, total: total, report: report}
	n, err := io.Copy(dst, r)
	report(r.received, total)
	return n, err
}

type downloadProgressReader struct {
	src             io.Reader
	total, received int64
	last            time.Time
	report          downloadProgress
}

func (r *downloadProgressReader) Read(b []byte) (int, error) {
	n, err := r.src.Read(b)
	r.received += int64(n)
	if time.Since(r.last) >= 100*time.Millisecond || err != nil {
		r.report(r.received, r.total)
		r.last = time.Now()
	}
	return n, err
}
