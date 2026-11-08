package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	uploadBase = "https://upload.uploadcare.com"
	// PartSize is the Upload API's default and minimum multipart part size.
	PartSize = 5 << 20
	// MinMultipart is the smallest file the Upload API takes in parts.
	MinMultipart = 10 << 20
	// MaxBase is the largest file a base upload takes.
	MaxBase       = 100 << 20
	partWorkers   = 4
	partAttempts  = 3
	throttleTries = 5
)

// Upload is one file to send through the Upload API, always stored.
type Upload struct {
	Name     string
	Size     int64
	MIME     string
	Data     io.ReaderAt
	Metadata map[string]string
	Tags     []string
	// Multipart sends the file in parts; the caller decides by size, within
	// MinMultipart and MaxBase.
	Multipart bool
	// Started is called with the uuid a multipart upload gets at its start,
	// before any part is sent.
	Started func(uuid string)
}

// Uploaded is what the Upload API said about a finished upload. A base
// upload answers with the uuid only, so MIME is empty then.
type Uploaded struct {
	UUID string
	MIME string
}

// UploadError is an Upload API answer other than success.
type UploadError struct {
	Status  int
	Message string
}

func (e *UploadError) Error() string {
	return fmt.Sprintf("upload API: HTTP %d: %s", e.Status, e.Message)
}

// Upload sends u with a base upload or in parts, retrying throttled
// requests as Retry-After asks.
func (c *Client) Upload(ctx context.Context, u Upload) (Uploaded, error) {
	if u.Multipart {
		return c.uploadParts(ctx, u)
	}
	var res map[string]string
	err := c.uploadForm(ctx, "/base/", func(w *multipart.Writer) error {
		if err := c.writeCommon(w, u); err != nil {
			return err
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, quoteEscaper.Replace(u.Name)))
		h.Set("Content-Type", u.MIME)
		part, err := w.CreatePart(h)
		if err != nil {
			return err
		}
		_, err = io.Copy(part, io.NewSectionReader(u.Data, 0, u.Size))
		return err
	}, &res)
	if err != nil {
		return Uploaded{}, err
	}
	if res["file"] == "" {
		return Uploaded{}, errors.New("upload API: no uuid in the base upload answer")
	}
	return Uploaded{UUID: res["file"]}, nil
}

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func (c *Client) writeCommon(w *multipart.Writer, u Upload) error {
	fields := [][2]string{{"UPLOADCARE_PUB_KEY", c.pubKey}, {"UPLOADCARE_STORE", "1"}}
	keys := make([]string, 0, len(u.Metadata))
	for k := range u.Metadata {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fields = append(fields, [2]string{"metadata[" + k + "]", u.Metadata[k]})
	}
	if len(u.Tags) > 0 {
		fields = append(fields, [2]string{"tags", strings.Join(u.Tags, ",")})
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return err
		}
	}
	return nil
}

// uploadForm posts a multipart form built by fill, streaming it, and
// decodes the JSON answer into out.
func (c *Client) uploadForm(ctx context.Context, path string, fill func(*multipart.Writer) error, out any) error {
	for try := 1; ; try++ {
		pr, pw := io.Pipe()
		w := multipart.NewWriter(pw)
		go func() {
			err := fill(w)
			if err == nil {
				err = w.Close()
			}
			_ = pw.CloseWithError(err)
		}()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadBase+path, pr)
		if err != nil {
			_ = pr.Close()
			return err
		}
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.Header.Set("User-Agent", "ucfs")
		wait, err := c.do(req, out)
		_ = pr.Close()
		if wait == 0 || try >= throttleTries {
			return err
		}
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// do sends req and decodes a 2xx JSON answer into out. A 429 returns how
// long to wait before trying again, along with the error.
func (c *Client) do(req *http.Request, out any) (time.Duration, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		uerr := &UploadError{Status: resp.StatusCode, Message: strings.TrimSpace(string(body))}
		var detail struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(body, &detail) == nil && detail.Detail != "" {
			uerr.Message = detail.Detail
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = time.Duration(s) * time.Second
			}
			return wait, uerr
		}
		return 0, uerr
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return 0, nil
	}
	return 0, json.NewDecoder(resp.Body).Decode(out)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// uploadParts runs /multipart/start/, PUTs the parts to the URLs it
// returns, then /multipart/complete/.
func (c *Client) uploadParts(ctx context.Context, u Upload) (Uploaded, error) {
	var start struct {
		UUID  string   `json:"uuid"`
		Parts []string `json:"parts"`
	}
	err := c.uploadForm(ctx, "/multipart/start/", func(w *multipart.Writer) error {
		if err := c.writeCommon(w, u); err != nil {
			return err
		}
		for _, f := range [][2]string{{"filename", u.Name}, {"size", strconv.FormatInt(u.Size, 10)},
			{"content_type", u.MIME}, {"part_size", strconv.Itoa(PartSize)}} {
			if err := w.WriteField(f[0], f[1]); err != nil {
				return err
			}
		}
		return nil
	}, &start)
	if err != nil {
		return Uploaded{}, err
	}
	if start.UUID == "" {
		return Uploaded{}, errors.New("upload API: no uuid in the multipart start answer")
	}
	if u.Started != nil {
		u.Started(start.UUID)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(partWorkers)
	for i, url := range start.Parts {
		off := int64(i) * PartSize
		n := min(PartSize, u.Size-off)
		if n <= 0 {
			break
		}
		g.Go(func() error { return c.putPart(gctx, url, u.MIME, io.NewSectionReader(u.Data, off, n), n) })
	}
	if err := g.Wait(); err != nil {
		return Uploaded{}, err
	}

	var info struct {
		UUID string `json:"uuid"`
		MIME string `json:"mime_type"`
	}
	err = c.uploadForm(ctx, "/multipart/complete/", func(w *multipart.Writer) error {
		if err := w.WriteField("UPLOADCARE_PUB_KEY", c.pubKey); err != nil {
			return err
		}
		return w.WriteField("uuid", start.UUID)
	}, &info)
	if err != nil {
		return Uploaded{}, err
	}
	return Uploaded{UUID: start.UUID, MIME: info.MIME}, nil
}

// putPart retries a part on network errors and 5xx answers: a part is idempotent.
func (c *Client) putPart(ctx context.Context, url, mime string, body *io.SectionReader, n int64) error {
	var err error
	for try := 1; try <= partAttempts; try++ {
		if _, serr := body.Seek(0, io.SeekStart); serr != nil {
			return serr
		}
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodPut, url, io.NopCloser(body))
		if err != nil {
			return err
		}
		req.ContentLength = n
		req.Header.Set("Content-Type", mime)
		var wait time.Duration
		wait, err = c.do(req, nil)
		var uerr *UploadError
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.As(err, &uerr) && uerr.Status < 500 && wait == 0:
			return err
		}
		if serr := sleep(ctx, max(wait, time.Duration(try)*time.Second)); serr != nil {
			return serr
		}
	}
	return err
}
