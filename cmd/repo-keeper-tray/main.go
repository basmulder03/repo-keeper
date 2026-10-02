// SPDX-License-Identifier: Apache-2.0

//go:build linux || windows

// Command repo-keeper-tray shows a status icon for the repo-keeper daemon. It is optional, holds no
// state or secrets, and talks to the daemon only through its local API.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"fyne.io/systray"

	"github.com/basmulder03/repo-keeper/internal/browser"
	"github.com/basmulder03/repo-keeper/internal/control"
	"github.com/basmulder03/repo-keeper/internal/instance"
	"github.com/basmulder03/repo-keeper/internal/paths"
	"github.com/basmulder03/repo-keeper/internal/tray"
)

const pollEvery = 5 * time.Second

func main() {
	runtimeDir := flag.String("runtime-dir", "", "directory holding ui.json (default: per-user runtime dir)")
	flag.Parse()
	dir := *runtimeDir
	if dir == "" {
		var err error
		if dir, err = paths.RuntimeDir(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	lock, err := instance.Acquire(filepath.Join(dir, "tray.lock"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "repo-keeper-tray:", err)
		os.Exit(3)
	}
	defer lock.Release()

	client := &tray.Client{RuntimeDir: dir}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	systray.Run(func() { onReady(ctx, client) }, cancel)
}

func onReady(ctx context.Context, c *tray.Client) {
	systray.SetTitle("repo-keeper")
	systray.SetIcon(tray.Icon(tray.Down))
	systray.SetTooltip("repo-keeper")

	line := systray.AddMenuItem("Starting…", "")
	line.Disable()
	systray.AddSeparator()
	open := systray.AddMenuItem("Open interface", "Open the web interface in your browser")
	syncAll := systray.AddMenuItem("Sync all now", "Queue every repository for an immediate sync")
	pause := systray.AddMenuItem("Pause syncing", "Stop scheduled syncs until resumed")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit tray", "Close this icon; the daemon keeps running")

	var current control.Status
	var last tray.State = -1
	apply := func(st *control.Status) {
		v := tray.Summarize(st)
		if st != nil {
			current = *st
		}
		line.SetTitle(v.Line)
		pause.SetTitle(v.PauseLabel)
		systray.SetTooltip(v.Tooltip)
		if v.State != last {
			systray.SetIcon(tray.Icon(v.State))
			last = v.State
		}
		if v.State == tray.Down {
			open.Disable()
			syncAll.Disable()
			pause.Disable()
		} else {
			open.Enable()
			syncAll.Enable()
			pause.Enable()
		}
	}
	poll := func() {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		if st, err := c.Status(cctx); err == nil {
			apply(&st)
		} else {
			apply(nil)
		}
	}
	act := func(f func(context.Context) error) {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_ = f(cctx) // the next poll shows the outcome; failures simply leave the state unchanged
		poll()
	}

	poll()
	go func() {
		t := time.NewTicker(pollEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				poll()
			case <-open.ClickedCh:
				act(func(ctx context.Context) error {
					u, err := c.LoginURL(ctx)
					if err != nil {
						return err
					}
					return browser.Open(u)
				})
			case <-syncAll.ClickedCh:
				act(c.SyncAll)
			case <-pause.ClickedCh:
				paused := !current.Paused
				act(func(ctx context.Context) error { return c.SetPaused(ctx, paused) })
			case <-quit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}
