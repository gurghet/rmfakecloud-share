package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
)

// uploadError is a client error with an HTTP status.
type uploadError struct {
	status int
	msg    string
}

func (e *uploadError) Error() string { return e.msg }

func bad(format string, a ...any) error {
	return &uploadError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, a...)}
}

// Upload is a parsed xochitl share request.
type Upload struct {
	DocID    string
	PageIDs  []string
	MailList []string
	Pages    [][]byte
}

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

type metadata struct {
	DocID    *string   `json:"DocID"`
	MailList []string  `json:"MailList"`
	Pages    *[]string `json:"Pages"`
}

// parseUpload parses the multipart body sent by xochitl: a "metadata" JSON part
// and one "page" part per PNG, in page order. The boundary is quoted and may
// contain dots and underscores, which mime.ParseMediaType handles.
func parseUpload(contentType string, body io.Reader, maxPages int) (*Upload, error) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, bad("expected multipart/form-data with a boundary")
	}
	mr := multipart.NewReader(body, params["boundary"])
	u := &Upload{}
	var md *metadata
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, asUploadError(err, "malformed multipart body")
		}
		switch part.FormName() {
		case "metadata":
			if md != nil {
				return nil, bad("duplicate metadata part")
			}
			b, err := io.ReadAll(io.LimitReader(part, 1<<20))
			if err != nil {
				return nil, asUploadError(err, "reading metadata")
			}
			md = &metadata{}
			if err := json.Unmarshal(b, md); err != nil {
				return nil, bad("metadata is not valid JSON")
			}
			if md.DocID == nil || md.Pages == nil {
				return nil, bad("metadata needs DocID (string) and Pages (array)")
			}
		case "page":
			if len(u.Pages) >= maxPages {
				return nil, bad("too many pages (max %d)", maxPages)
			}
			b, err := io.ReadAll(part)
			if err != nil {
				return nil, asUploadError(err, "reading page")
			}
			if !bytes.HasPrefix(b, pngMagic) {
				return nil, bad("page %d is not a PNG", len(u.Pages)+1)
			}
			u.Pages = append(u.Pages, b)
		default:
			// Ignore unknown parts.
			if _, err := io.Copy(io.Discard, part); err != nil {
				return nil, asUploadError(err, "reading part")
			}
		}
	}
	if md == nil {
		return nil, bad("missing metadata part")
	}
	if len(u.Pages) == 0 {
		return nil, bad("no page parts")
	}
	u.DocID = *md.DocID
	u.PageIDs = *md.Pages
	u.MailList = md.MailList
	return u, nil
}

// asUploadError maps body-size errors to 413 and everything else to 400.
func asUploadError(err error, msg string) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return &uploadError{status: http.StatusRequestEntityTooLarge, msg: "request body too large"}
	}
	return bad("%s: %v", msg, err)
}
