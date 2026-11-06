// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/basmulder03/repo-keeper/internal/daemon"
	"github.com/basmulder03/repo-keeper/internal/instance"
)

func (a *app) cmdAutostart(ctx context.Context, args []string) int {
	var lf lifecycleFlags
	if a.lifecycleFlagSet("autostart", &lf).Parse(args) != nil {
		return 2
	}
	if err := a.resolve(&lf.commonFlags); err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	c, err := a.lifecycleClient(&lf)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	running := "no"
	if st, err := c.Status(ctx); err == nil {
		running = fmt.Sprintf("yes (version %s)", st.Version)
	} else if _, lockPath, _ := daemon.StatePaths(lf.stateDir); lockPath != "" {
		if pid, held := instance.Holder(lockPath); held {
			running = "yes (pid " + pidText(pid) + ", no web interface)"
		}
	}
	state := "unknown"
	if s, err := newSvc(); err == nil {
		state = s.state(ctx)
	}
	a.printf("running:   %s\nautostart: %s\n", running, state)
	return 0
}
