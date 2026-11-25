package client

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/isolation"
)

var errNoView = errors.New("the turn has no view on this client")

// handleRequest carries out one view or skill operation the server asks for and
// replies on the request's own route.
func (c *Client) handleRequest(req api.ServerRequest) {
	reply := c.perform(req)
	for attempt := 0; attempt < 5; attempt++ {
		lease := c.currentLease()
		if lease != "" {
			err := c.post(c.rootCtx, api.ClientRoute(lease, "requests/"+req.ID+"/reply"), reply, &api.ServerReplyResponse{}, callTimeout)
			if err == nil || !transient(err) {
				return
			}
		}
		select {
		case <-c.rootCtx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// perform carries out an operation on the running turn's own view or prepared
// skills. A path that is absolute, climbs out of the view or names version
// control metadata is refused before it reaches the files.
func (c *Client) perform(req api.ServerRequest) api.ServerReply {
	c.mu.Lock()
	t := c.running[req.Turn]
	c.mu.Unlock()
	if t == nil {
		return api.ServerReply{Error: "the turn does not run on this client"}
	}
	files, ok := t.fileSet()
	if !ok {
		return api.ServerReply{Error: errNoView.Error()}
	}
	result, err := carryOut(t.ctx, files, req)
	if err != nil {
		return api.ServerReply{Error: err.Error()}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return api.ServerReply{Error: err.Error()}
	}
	return api.ServerReply{Result: raw}
}

func carryOut(ctx context.Context, files isolation.Files, req api.ServerRequest) (any, error) {
	switch req.Op {
	case api.OpViewRead:
		var p api.ViewReadParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err
		}
		if p.Path != "." {
			if err := isolation.CheckPath(p.Path); err != nil {
				return nil, err
			}
		}
		data, err := files.View.Read(p.Path)
		return api.ViewReadResult{Data: data}, err
	case api.OpViewWrite:
		var p api.ViewWriteParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err
		}
		if err := isolation.CheckPath(p.Path); err != nil {
			return nil, err
		}
		return struct{}{}, files.View.Put(p.Path, p.Content)
	case api.OpSkillRead:
		var p api.ViewReadParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err
		}
		if files.Skills == nil {
			return nil, errors.New("the turn has no skills on this client")
		}
		data, err := files.Skills.Read(ctx, p.Path)
		return api.ViewReadResult{Data: data}, err
	}
	return nil, errors.New("this client does not carry out " + req.Op)
}
