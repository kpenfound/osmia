package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

const uploadTimeout = 30 * time.Minute

var plainName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// The states of a turn's record on disk.
const (
	stateRunning  = "running"
	stateFinished = "finished"
)

// record is what the client keeps of a turn under its turn directory: the
// finished view and the result to send, until the server has answered for
// both. A restarted client reads the records and sends what is left.
type record struct {
	Turn       string `json:"turn"`
	Project    string `json:"project"`
	Workstream string `json:"workstream"`
	Thread     string `json:"thread"`
	Role       string `json:"role"`
	ReturnView bool   `json:"return_view"`
	// Token is the bearer token the view upload carries.
	Token string `json:"token"`
	// ViewDir holds the files to return, empty when the turn built none.
	ViewDir    string `json:"view_dir,omitempty"`
	State      string `json:"state"`
	ViewDone   bool   `json:"view_done,omitempty"`
	ResultDone bool   `json:"result_done,omitempty"`

	mu     sync.Mutex
	result *api.TurnResult
	busy   bool
	fails  int
	next   time.Time
}

func (c *Client) turnDir(turn string) string { return filepath.Join(c.dir, "turns", turn) }

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Client) saveRecord(r *record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(c.turnDir(r.Turn), "meta.json"), data)
}

func (c *Client) saveResult(r *record, res api.TurnResult) error {
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(c.turnDir(r.Turn), "result.json"), data); err != nil {
		return err
	}
	r.result = &res
	return nil
}

// loadPending reads the turns an earlier run of the client left behind. A turn
// that was still running died with that run: its view is sent with a result
// that says it was interrupted, which the server answers like any lost turn.
func (c *Client) loadPending() error {
	entries, err := os.ReadDir(filepath.Join(c.dir, "turns"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		dir := filepath.Join(c.dir, "turns", entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil {
			continue
		}
		r := new(record)
		if json.Unmarshal(data, r) != nil || r.Turn != entry.Name() {
			c.log.Warn("ignoring an unreadable turn record", "directory", dir)
			continue
		}
		if raw, err := os.ReadFile(filepath.Join(dir, "result.json")); err == nil {
			var res api.TurnResult
			if json.Unmarshal(raw, &res) == nil {
				r.result = &res
			}
		}
		if r.ViewDir == "" && r.ReturnView {
			if views, _ := filepath.Glob(filepath.Join(dir, "views", "turn-*")); len(views) > 0 {
				r.ViewDir = views[0]
			}
		}
		if r.result == nil {
			res := api.TurnResult{Result: coreadapter.SessionResult{StartedAt: c.now().UTC(), Cancelled: true, IsError: true, ErrorSubtype: "interrupted"}}
			if err := c.saveResult(r, res); err != nil {
				return err
			}
		}
		r.State = stateFinished
		if err := c.saveRecord(r); err != nil {
			return err
		}
		c.log.Info("a turn from an earlier run is waiting to be sent", "turn", r.Turn)
		c.pending[r.Turn] = r
	}
	return nil
}

func definitive(a api.ViewAnswer) bool {
	return a == api.ViewAccepted || a == api.ViewAlreadyRecorded || a == api.ViewSuperseded
}

// kick asks the delivery loop to try now.
func (c *Client) kick() {
	select {
	case c.deliver <- struct{}{}:
	default:
	}
}

// deliverLoop sends the finished views and results the server has not yet
// answered for, again and again while it cannot be reached, and discards a turn
// only once the server has answered accepted, already recorded or superseded for
// everything it holds of it.
func (c *Client) deliverLoop(ctx context.Context) {
	tick := time.NewTicker(c.retryEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.deliver:
		case <-tick.C:
		}
		c.mu.Lock()
		var due []*record
		for _, r := range c.pending {
			due = append(due, r)
		}
		c.mu.Unlock()
		var wg sync.WaitGroup
		for _, r := range due {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.send(ctx, r)
			}()
		}
		wg.Wait()
	}
}

func (c *Client) send(ctx context.Context, r *record) {
	r.mu.Lock()
	if r.busy || c.now().Before(r.next) {
		r.mu.Unlock()
		return
	}
	r.busy = true
	r.mu.Unlock()
	err := c.sendNow(ctx, r)
	r.mu.Lock()
	r.busy = false
	if err != nil && ctx.Err() == nil {
		r.fails++
		r.next = c.now().Add(min(c.retryEvery<<min(r.fails, 5), 2*time.Minute))
		c.log.Warn("cannot send a turn's files or result yet; keeping them", "turn", r.Turn, "error", err)
	} else {
		r.fails, r.next = 0, time.Time{}
	}
	r.mu.Unlock()
}

func (c *Client) sendNow(ctx context.Context, r *record) error {
	r.mu.Lock()
	viewDone, resultDone, viewDir := r.ViewDone, r.ResultDone, r.ViewDir
	token := r.Token
	r.mu.Unlock()
	if !viewDone && r.ReturnView && viewDir != "" {
		upCtx, cancel := context.WithTimeout(ctx, uploadTimeout)
		answer, err := c.upload(upCtx, r.Turn, token, viewDir)
		cancel()
		if err != nil {
			return fmt.Errorf("send the view: %w", err)
		}
		if !definitive(answer) {
			return fmt.Errorf("the server answered %q for the view", answer)
		}
		c.log.Info("the server answered for a turn's view", "turn", r.Turn, "answer", answer)
		viewDone = true
		r.mu.Lock()
		r.ViewDone = true
		err = c.saveRecord(r)
		r.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if !resultDone {
		lease := c.currentLease()
		if lease == "" {
			return errors.New("not connected to the server")
		}
		var out api.TurnResultResponse
		if err := c.post(ctx, api.ClientRoute(lease, "turns/"+r.Turn+"/result"), r.result, &out, resultTimeout); err != nil {
			return fmt.Errorf("send the result: %w", err)
		}
		if !definitive(out.Answer) {
			return fmt.Errorf("the server answered %q for the result", out.Answer)
		}
		c.log.Info("the server answered for a turn's result", "turn", r.Turn, "answer", out.Answer)
		r.mu.Lock()
		r.ResultDone = true
		err := c.saveRecord(r)
		r.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if err := os.RemoveAll(c.turnDir(r.Turn)); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.pending, r.Turn)
	c.mu.Unlock()
	c.changed()
	return nil
}
