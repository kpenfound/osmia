package clientturns

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/isolation"
)

type remoteFiles struct {
	ctx  context.Context
	call func(ctx context.Context, op string, params any) (json.RawMessage, error)
}

var _ isolation.View = remoteFiles{}

func (f remoteFiles) Read(name string) ([]byte, error) {
	name = strings.TrimSuffix(name, "/")
	if name != "." {
		if err := isolation.CheckPath(name); err != nil {
			return nil, err
		}
	}
	raw, err := f.call(f.ctx, api.OpViewRead, api.ViewReadParams{Path: name})
	if err != nil {
		return nil, err
	}
	var out api.ViewReadResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func (f remoteFiles) Write(name string, data []byte) error { return f.put(name, data) }

func (f remoteFiles) Put(name string, data []byte) error { return f.put(name, data) }

func (f remoteFiles) put(name string, data []byte) error {
	if err := isolation.CheckPath(name); err != nil {
		return err
	}
	_, err := f.call(f.ctx, api.OpViewWrite, api.ViewWriteParams{Path: name, Content: data})
	return err
}

type remoteSkills struct {
	call func(ctx context.Context, op string, params any) (json.RawMessage, error)
}

var _ isolation.Skills = remoteSkills{}

var errSkillPath = errors.New("a skill is read at an absolute path with no traversal")

func (s remoteSkills) Read(ctx context.Context, path string) ([]byte, error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return nil, errSkillPath
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return nil, errSkillPath
		}
		if isolation.VCSMetadata(part) {
			return nil, errors.New("VCS metadata is not exposed")
		}
	}
	raw, err := s.call(ctx, api.OpSkillRead, api.ViewReadParams{Path: path})
	if err != nil {
		return nil, err
	}
	var out api.ViewReadResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}
