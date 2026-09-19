package relaylink

import (
	"context"
	"encoding/json"
	"net/http"
)

// Response is the application-level answer the main returns inside the sealed
// transport response. The MAIN decides what may be cached and for how long;
// the relay just obeys.
type Response struct {
	Status int      `json:"s"`
	Body   []byte   `json:"b,omitempty"`
	TTL    int      `json:"t,omitempty"` // seconds the relay may cache this; 0 = never
	Tags   []string `json:"g,omitempty"` // invalidation tags (see the sync stream)
}

// RPCHandler serves the transport with a Response-returning dispatcher.
func (s *Server) RPCHandler(dispatch func(context.Context, *Request) *Response) http.Handler {
	return s.Handler(func(req *Request) (int, []byte) {
		resp := dispatch(req.Ctx, req)
		if resp == nil {
			resp = &Response{Status: http.StatusInternalServerError}
		}
		out, err := json.Marshal(resp)
		if err != nil {
			out, _ = json.Marshal(&Response{Status: http.StatusInternalServerError})
		}
		return http.StatusOK, out
	})
}

// Call performs one request against the main and decodes the Response.
func (c *Client) Call(ctx context.Context, method, path string, body []byte) (*Response, error) {
	status, raw, err := c.Do(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, ErrBadResponse
	}
	var r Response
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, ErrBadResponse
	}
	return &r, nil
}

// JSON is a small helper for handlers: a JSON body with the given status,
// cache TTL (seconds) and tags.
func JSON(status int, v any, ttl int, tags ...string) *Response {
	b, err := json.Marshal(v)
	if err != nil {
		return &Response{Status: http.StatusInternalServerError}
	}
	return &Response{Status: status, Body: b, TTL: ttl, Tags: tags}
}
