package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Request is what the page asks the worker. It arrives as JSON from
// postMessage, so it is decoded strictly and bounded.
type Request struct {
	Op    string `json:"op"`
	Path  string `json:"path,omitempty"`
	Off   int64  `json:"off,omitempty"`
	N     int64  `json:"n,omitempty"`
	Cache *bool  `json:"cache,omitempty"`
}

// Response is the worker's answer. Bytes travel beside it, not inside it.
type Response struct {
	OK      bool     `json:"ok"`
	Error   string   `json:"error,omitempty"`
	Info    *Info    `json:"info,omitempty"`
	Listing *Listing `json:"listing,omitempty"`
	Size    int64    `json:"size,omitempty"` // the file's size, for read
	Stats   *Stats   `json:"stats,omitempty"`
	Ms      float64  `json:"ms"`
}

// MaxRequest bounds a request's JSON.
const MaxRequest = 16 << 10

// DecodeRequest parses one request, refusing unknown fields, trailing data
// and oversize input.
func DecodeRequest(b []byte) (Request, error) {
	var r Request
	if len(b) > MaxRequest {
		return r, fmt.Errorf("request of %d bytes exceeds %d", len(b), MaxRequest)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return Request{}, fmt.Errorf("bad request: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return Request{}, errors.New("bad request: trailing data")
	}
	return r, nil
}

// Handle answers one request. data is a file's bytes (read) or the image's
// bytes (sample), or nil.
func (s *Session) Handle(req []byte) (resp []byte, data []byte) {
	t0 := time.Now()
	var out Response
	data, err := s.handle(req, &out)
	if err != nil {
		out = Response{Error: err.Error(), Info: out.Info}
		data = nil
	} else {
		out.OK = true
	}
	out.Ms = msSince(t0)
	b, _ := json.Marshal(out) // a Response always marshals
	return b, data
}

func (s *Session) handle(b []byte, out *Response) ([]byte, error) {
	r, err := DecodeRequest(b)
	if err != nil {
		return nil, err
	}
	switch r.Op {
	case "list":
		l, err := s.List(r.Path)
		if err != nil {
			return nil, err
		}
		out.Listing = &l
		return nil, nil
	case "read":
		data, size, err := s.Read(r.Path, r.Off, r.N)
		out.Size = size
		return data, err
	case "image":
		data, size, err := s.ReadImage(r.Off, r.N)
		out.Size = size
		return data, err
	case "stats":
		st := s.Stats()
		out.Stats = &st
		return nil, nil
	case "info":
		info := s.Info()
		if info.Type == "" {
			return nil, ErrNotOpen
		}
		out.Info = &info
		return nil, nil
	case "close":
		s.Close()
		return nil, nil
	case "sample":
		if r.Cache != nil {
			s.SetCache(*r.Cache)
		}
		img, err := Sample()
		if err != nil {
			return nil, err
		}
		info, err := s.Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			return nil, err
		}
		out.Info = &info
		return img, nil
	}
	return nil, fmt.Errorf("unknown op %q", r.Op)
}

// OpenResponse encodes the answer to an open, which the worker performs
// itself because the image is a browser File, not JSON.
func OpenResponse(info Info, err error, ms float64) []byte {
	out := Response{OK: err == nil, Ms: ms, Info: &info}
	if err != nil {
		out.Error = err.Error()
	}
	b, _ := json.Marshal(out)
	return b
}
