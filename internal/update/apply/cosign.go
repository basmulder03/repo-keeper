// SPDX-License-Identifier: Apache-2.0

package apply

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CosignCLI verifies with the cosign program, which must be installed. The identity is passed exactly (never a
// pattern), so a signature made for another tag or another workflow does not verify.
type CosignCLI struct {
	Path string // default "cosign"
}

func (c CosignCLI) bin() string {
	if c.Path != "" {
		return c.Path
	}
	return "cosign"
}

// Available reports whether the cosign program can be found.
func (c CosignCLI) Available() bool {
	_, err := exec.LookPath(c.bin())
	return err == nil
}

// Verify implements Verifier.
func (c CosignCLI) Verify(ctx context.Context, checksums, sig, cert []byte, identity, issuer string) error {
	bin, err := exec.LookPath(c.bin())
	if err != nil {
		return ErrNoCosign
	}
	dir, err := os.MkdirTemp("", "repo-keeper-verify-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	paths := map[string][]byte{"checksums.txt": checksums, "checksums.txt.sig": sig, "checksums.txt.pem": cert}
	for name, data := range paths {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// #nosec G204 -- the cosign program with fixed flags; identity and issuer are data, not shell
	cmd := exec.CommandContext(ctx, bin, "verify-blob", filepath.Join(dir, "checksums.txt"), //nolint:gosec,forbidigo // sanctioned: signature verification is the one reason this package runs a program
		"--certificate", filepath.Join(dir, "checksums.txt.pem"), "--signature", filepath.Join(dir, "checksums.txt.sig"),
		"--certificate-identity", identity, "--certificate-oidc-issuer", issuer)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("cosign: %w: %s", err, msg)
	}
	return nil
}
