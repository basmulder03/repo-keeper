// SPDX-License-Identifier: Apache-2.0

package tray

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sync"

	"github.com/basmulder03/repo-keeper/internal/control"
)

// State is what the tray icon expresses.
type State int

// Tray states, ordered by how much attention they deserve.
const (
	Down State = iota
	OK
	Attention
	Paused
)

func (s State) String() string { return [...]string{"down", "ok", "attention", "paused"}[s] }

// View is everything the tray shows, derived purely from the daemon's status.
type View struct {
	State      State
	Line       string // the disabled first menu entry
	Tooltip    string
	PauseLabel string
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Summarize maps a status (or its absence) to a View. Attention outranks pause: a problem must never be hidden.
func Summarize(st *control.Status) View {
	if st == nil {
		return View{State: Down, Line: "Daemon not running", Tooltip: "repo-keeper: daemon not running", PauseLabel: "Pause syncing"}
	}
	v := View{PauseLabel: "Pause syncing"}
	if st.Paused {
		v.PauseLabel = "Resume syncing"
	}
	switch {
	case st.NeedAttention > 0 || st.AccountsAttention > 0:
		v.State = Attention
		v.Line = plural(st.NeedAttention, "repository needs", "repositories need") + " attention"
		if st.NeedAttention == 0 {
			v.Line = plural(st.AccountsAttention, "account needs", "accounts need") + " attention"
		} else if st.AccountsAttention > 0 {
			v.Line += fmt.Sprintf(" · %s", plural(st.AccountsAttention, "account", "accounts"))
		}
	case st.Paused:
		v.State = Paused
		v.Line = "Paused · " + plural(st.Repos, "repository", "repositories")
	case st.Repos == 0:
		v.State = OK
		v.Line = "No repositories yet"
	case st.Pending > 0:
		v.State = OK
		v.Line = fmt.Sprintf("%d up to date · %d pending", st.UpToDate, st.Pending)
	default:
		v.State = OK
		v.Line = "All " + plural(st.Repos, "repository", "repositories") + " up to date"
	}
	v.Tooltip = "repo-keeper: " + v.Line
	return v
}

var (
	iconMu    sync.Mutex
	iconCache = map[State][]byte{}
)

var palette = map[State]color.RGBA{
	OK:        {0x2e, 0xa0, 0x5a, 0xff},
	Attention: {0xd9, 0x3a, 0x2f, 0xff},
	Paused:    {0x8a, 0x93, 0xa0, 0xff},
	Down:      {0x4a, 0x50, 0x59, 0xff},
}

// Icon returns a 64x64 PNG for the state, drawn in code so no binary assets ship.
func Icon(s State) []byte {
	iconMu.Lock()
	defer iconMu.Unlock()
	if b, ok := iconCache[s]; ok {
		return b
	}
	const n = 64
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	bg, white := palette[s], color.RGBA{0xff, 0xff, 0xff, 0xff}
	const c, r = 31.5, 30.0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)-c, float64(y)-c
			if d2 := dx*dx + dy*dy; d2 <= r*r {
				img.Set(x, y, bg)
			}
		}
	}
	rect := func(x0, y0, x1, y1 int) {
		draw.Draw(img, image.Rect(x0, y0, x1, y1), &image.Uniform{white}, image.Point{}, draw.Src)
	}
	switch s {
	case OK: // check mark
		for i := 0; i < 10; i++ {
			rect(16+i, 32+i, 22+i, 38+i)
		}
		for i := 0; i < 20; i++ {
			rect(25+i, 41-i, 31+i, 47-i)
		}
	case Attention: // exclamation mark
		rect(28, 14, 36, 38)
		rect(28, 44, 36, 52)
	case Paused: // pause bars
		rect(20, 16, 29, 48)
		rect(35, 16, 44, 48)
	case Down: // dash
		rect(16, 28, 48, 36)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic("tray: encoding icon: " + err.Error()) // cannot fail for an in-memory RGBA image
	}
	iconCache[s] = buf.Bytes()
	return iconCache[s]
}
