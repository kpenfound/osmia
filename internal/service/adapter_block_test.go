package service

import (
	config "github.com/kpenfound/osmia/internal/config"
)

// block makes the named turn signal entered and wait for release or for its
// context to end.
func (f *twoProjectFixture) block(id config.ProjectID, name string) (entered, release chan struct{}) {
	return f.turns.Block(turn(id, name))
}
