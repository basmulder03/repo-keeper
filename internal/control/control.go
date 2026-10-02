// SPDX-License-Identifier: Apache-2.0

// Package control holds the small types shared by the daemon, the CLI and the tray helper.
package control

import "time"

// RuntimeFile is written by the daemon (mode 0600) so local clients can find and authenticate to it.
type RuntimeFile struct {
	Addr    string    `json:"addr"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Control string    `json:"control"`
	Version string    `json:"version"`
}

// Status is the compact fleet summary served to the tray helper.
type Status struct {
	Version           string `json:"version"`
	Repos             int    `json:"repos"`
	UpToDate          int    `json:"up_to_date"`
	NeedAttention     int    `json:"need_attention"`
	Pending           int    `json:"pending"`
	Accounts          int    `json:"accounts"`
	AccountsAttention int    `json:"accounts_attention"`
	Paused            bool   `json:"paused"`
}
