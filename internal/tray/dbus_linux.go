//go:build linux

package tray

import (
	"context"
	"time"

	"github.com/godbus/dbus/v5"
)

// sessionBusNames lists names currently owned on the session bus.
func sessionBusNames(timeoutMs int) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	type result struct {
		names []string
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := dbus.ConnectSessionBus()
		if err != nil {
			ch <- result{nil, err}
			return
		}
		defer conn.Close()
		var names []string
		obj := conn.Object("org.freedesktop.DBus", "/org/freedesktop/DBus")
		if err := obj.Call("org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
			ch <- result{nil, err}
			return
		}
		ch <- result{names, nil}
	}()
	select {
	case r := <-ch:
		return r.names, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
