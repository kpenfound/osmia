package tailnet

import (
	"context"
	"errors"
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

type fakeWhoIs struct {
	who  *apitype.WhoIsResponse
	err  error
	addr string
}

func (f *fakeWhoIs) WhoIs(_ context.Context, addr string) (*apitype.WhoIsResponse, error) {
	f.addr = addr
	return f.who, f.err
}

func TestWhoIsNamesANodeByItsStableIDAndShortName(t *testing.T) {
	t.Parallel()
	client := &fakeWhoIs{who: &apitype.WhoIsResponse{Node: &tailcfg.Node{StableID: "nABC123CNTRL", Name: "laptop.tail1234.ts.net."}}}
	node, err := whoIs(context.Background(), client, "100.64.0.7:51234")
	if err != nil || node.ID != "nABC123CNTRL" || node.Name != "laptop" || client.addr != "100.64.0.7:51234" {
		t.Fatalf("%+v %v, asked about %q", node, err, client.addr)
	}
}

func TestWhoIsFailsForACallerTheTailnetDoesNotKnow(t *testing.T) {
	t.Parallel()
	for name, client := range map[string]*fakeWhoIs{
		"an error":     {err: errors.New("no peer")},
		"no node":      {who: &apitype.WhoIsResponse{}},
		"no stable ID": {who: &apitype.WhoIsResponse{Node: &tailcfg.Node{Name: "x.ts.net."}}},
	} {
		if node, err := whoIs(context.Background(), client, "100.64.0.9:1"); err == nil {
			t.Errorf("%s: identified as %+v", name, node)
		}
	}
}
