// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gensupport

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/nildev/account/Godeps/_workspace/src/golang.org/x/net/context"
	"github.com/nildev/account/Godeps/_workspace/src/golang.org/x/net/context/ctxhttp"
)

const (
	// statusResumeIncomplete is the code returned by the Google uploader when the transfer is not yet complete.
	statusResumeIncomplete = 308
)

// DefaultBackoffStrategy returns a default strategy to use for retrying failed upload requests.
func DefaultBackoffStrategy() BackoffStrategy {
	return &ExponentialBackoff{
		Base: 250 * time.Millisecond,
		Max:  16 * time.Second,
	}
}

// ResumableUpload is used by the generated APIs to provide resumable uploads.
// It is not used by developers directly.
type ResumableUpload struct {
	Client *http.Client
	// URI is the resumable resource destination provided by the server after specifying "&uploadType=resumable".
	URI       string
	UserAgent string // User-Agent for header of the request
	// Media is the object being uploaded.
	Media *ResumableBuffer
	// MediaType defines the media type, e.g. "image/jpeg".
	MediaType string

	mu       sync.Mutex // guards progress
	progress int64      // number of bytes uploaded so far

	// Callback is an optional function that will be periodically called with the cumulative number of bytes uploaded.
	Callback func(int64)

	// If not specified, a default exponential backoff strategy will be used.
	Backoff BackoffStrategy
}

// Progress returns the number of bytes uploaded at this point.
func (rx *ResumableUpload) Progress() int64 {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	return rx.progress
}

// doUploadRequest performs a single HTTP request to upload data.
// off specifies the offset in rx.Media from which data is drawn.
// size is the number of bytes in data.
// final specifies whether data is the final chunk to be uploaded.
func (rx *ResumableUpload) doUploadRequest(ctx context.Context, data io.Reader, off, size int64, final bool) (*http.Response, error) {
	req, err := http.NewRequest("POST", rx.URI, data)
	if err != nil {
		return nil, err
	}

	req.ContentLength = size
	var contentRange string
	if final {
		if size == 0 {
			contentRange = fmt.Sprintf("bytes */%v", off)
		} else {
			contentRange = fmt.Sprintf("bytes %v-%v/%v", off, off+size-1, off+size)
		}
	} else {
		contentRange = fmt.Sprintf("bytes %v-%v/*", off, off+size-1)
	}
	req.Header.Set("Content-Range", contentRange)
	req.Header.Set("Content-Type", rx.MediaType)
	req.Header.Set("User-Agent", rx.UserAgent)
	return ctxhttp.Do(ctx, rx.Client, req)

}

// reportProgress calls a user-supplied callback to report upload progress.
// If old==updated, the callback is not called.
func (rx *ResumableUpload) reportProgress(old, updated int64) {
	if updated-old == 0 {
		return
	}
	rx.mu.Lock()
	rx.progress = updated
	rx.mu.Unlock()
	if rx.Callback != nil {
		rx.Callback(updated)
	}
}

// transferChunk performs a single HTTP request to upload a single chunk from rx.Media.
func (rx *ResumableUpload) transferChunk(ctx context.Context) (*http.Response, error) {
	chunk, off, size, err := rx.Media.Chunk()

	done := err == io.EOF
	if !done && err != nil {
		return nil, err
	}

	res, err := rx.doUploadRequest(ctx, chunk, off, int64(size), done)
	if err != nil {
		return res, err
	}

	if res.StatusCode == statusResumeIncomplete || res.StatusCode == http.StatusOK {
		rx.reportProgress(off, off+int64(size))
	}

	if res.StatusCode == statusResumeIncomplete {
		rx.Media.Next()
	}
	return res, nil
}

func contextDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// Upload starts the process of a resumable upload with a cancellable context.
// It retries using the provided back off strategy until cancelled or the
// strategy indicates to stop retrying.
// It is called from the auto-generated API code and is not visible to the user.
// rx is private to the auto-generated API code.
// Exactly one of resp or err will be nil.  If resp is non-nil, the caller must call resp.Body.Close.
func (rx *ResumableUpload) Upload(ctx context.Context) (resp *http.Response, err error) {
	var pause time.Duration
	backoff := rx.Backoff
	if backoff == nil {
		backoff = DefaultBackoffStrategy()
	}

	for {
		// Ensure that we return in the case of cancelled context, even if pause is 0.
		if contextDone(ctx) {
			return nil, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pause):
		}

		resp, err = rx.transferChunk(ctx)
		// It's possible for err and resp to both be non-nil here, but we expose a simpler
		// contract to our callers: exactly one of resp and err will be non-nil.  This means
		// that any response body must be closed here before returning a non-nil error.
		if err != nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			return nil, err
		}
		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		if resp.StatusCode == statusResumeIncomplete {
			pause = 0
			backoff.Reset()
		} else {
			var retry bool
			pause, retry = backoff.Pause()
			if !retry {
				// Return the last response with its failing HTTP code.
				return resp, nil
			}
		}
		resp.Body.Close()
	}
}
