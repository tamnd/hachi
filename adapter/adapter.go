// Package adapter defines how hachi drives a brain: any AI coding agent,
// local or remote, wrapped behind two small interfaces and a driver
// registry in the style of database/sql. Nothing above this package knows
// which agent is running; drivers register by name and advertise
// capabilities, and the engine picks by name.
package adapter

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/tamnd/hachi/waggle"
)

// SteerMode says how a driver accepts mid-run redirection.
type SteerMode string

const (
	// SteerLive means the driver accepts messages while a turn runs.
	SteerLive SteerMode = "live"
	// SteerResume means steering stops the turn and resumes with the message.
	SteerResume SteerMode = "resume"
	// SteerNone means the driver cannot be redirected mid-run.
	SteerNone SteerMode = "none"
)

// Info describes a registered driver.
type Info struct {
	Name  string
	Steer SteerMode
}

// Session is the state a driver needs to continue a conversation.
// Resume is opaque to everything except the driver that wrote it.
type Session struct {
	ID     waggle.SessionID
	Dir    string
	Resume string
}

// Stream is a live run: events until the channel closes, and a stop.
type Stream interface {
	Events() <-chan waggle.Event
	Stop(ctx context.Context) error
}

// Adapter runs one turn of a session and streams what happens.
type Adapter interface {
	Run(ctx context.Context, sess Session, msg string) (Stream, error)
}

// Detector reports whether the driver's backing tool is usable on this
// machine. Drivers implement it so first-run can offer only real choices.
type Detector interface {
	Detect() error
}

// Factory builds a driver instance.
type Factory func() (Adapter, error)

type driver struct {
	info    Info
	factory Factory
}

var (
	mu      sync.RWMutex
	drivers = map[string]driver{}
)

// Register makes a driver available by name. It follows the database/sql
// convention: drivers call it from init, and duplicate names are a
// programmer error.
func Register(info Info, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := drivers[info.Name]; dup {
		panic("adapter: Register called twice for " + info.Name)
	}
	drivers[info.Name] = driver{info: info, factory: f}
}

// Open returns a driver instance by name.
func Open(name string) (Adapter, error) {
	mu.RLock()
	d, ok := drivers[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("adapter: unknown driver %q (registered: %v)", name, Names())
	}
	return d.factory()
}

// Lookup returns the Info for a registered driver.
func Lookup(name string) (Info, bool) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := drivers[name]
	return d.info, ok
}

// Names lists registered drivers, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(drivers))
	for n := range drivers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Detect returns the subset of registered drivers whose backing tools are
// usable right now, sorted by name.
func Detect() []Info {
	mu.RLock()
	defer mu.RUnlock()
	var out []Info
	for _, d := range drivers {
		a, err := d.factory()
		if err != nil {
			continue
		}
		if det, ok := a.(Detector); ok && det.Detect() != nil {
			continue
		}
		out = append(out, d.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
