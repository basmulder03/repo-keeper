// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"log/slog"
	"sync"

	"github.com/basmulder03/repo-keeper/internal/obs"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type testLog struct {
	logger *slog.Logger
	red    *obs.Redactor
}

func newTestLog(w *syncBuf) testLog {
	r := &obs.Redactor{}
	return testLog{logger: obs.New(w, slog.LevelDebug, false, r), red: r}
}
