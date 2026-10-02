// SPDX-License-Identifier: Apache-2.0

package browser

import "testing"

func TestOpen_RefusesNonLoopback(t *testing.T) {
	for _, u := range []string{"https://127.0.0.1/x", "http://example.com/", "file:///etc/passwd", "javascript:alert(1)", "http://localhost/", "::bad", "http://[::1"} {
		if err := Open(u); err == nil {
			t.Errorf("%q must be refused", u)
		}
	}
}
