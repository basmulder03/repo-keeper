// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/config"
	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/paths"
	"github.com/basmulder03/repo-keeper/internal/provider"
	"github.com/basmulder03/repo-keeper/internal/provider/github"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

// newProvider builds a provider for an account with its own limiter (CLI runs are short-lived).
func (a *app) newProvider(acct config.Account, tok secrets.Token) (provider.Provider, *ratelimit.Limiter, error) {
	lim := ratelimit.New(ratelimit.Config{}, clock.Real{}, nil)
	hc, err := httpx.New(httpx.Config{
		UserAgent: "repo-keeper/" + version + " (+https://github.com/basmulder03/repo-keeper)",
		Limiter:   lim, MaxWait: 30 * time.Second,
	})
	if err != nil {
		return nil, nil, err
	}
	p, err := provider.New(provider.Kind(acct.Provider), provider.Config{BaseURL: acct.BaseURL, Token: tok, HTTP: hc, Remotes: acct.URLs})
	return p, lim, err
}

func (a *app) credSource(acct config.Account) secrets.Source {
	return secrets.Source{Env: acct.TokenEnv, File: acct.TokenFile, Key: "account/" + acct.Name, Optional: acct.Provider == "git"}
}

func (a *app) loadConfig(path string) (config.Config, string, error) {
	c := commonFlags{configPath: path}
	if err := a.resolve(&c); err != nil {
		return config.Config{}, "", err
	}
	cfg, err := config.Load(c.configPath)
	return cfg, c.configPath, err
}

func findAccount(cfg config.Config, name string) (config.Account, bool) {
	for _, ac := range cfg.Accounts {
		if ac.Name == name {
			return ac, true
		}
	}
	return config.Account{}, false
}

func (a *app) cmdAccounts(ctx context.Context, args []string) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(a.err, "usage: repo-keeper accounts <add|list|check|rm> ...")
		return 2
	}
	switch args[0] {
	case "add":
		return a.accountsAdd(ctx, args[1:])
	case "list":
		return a.accountsList(ctx, args[1:])
	case "check":
		return a.accountsCheck(ctx, args[1:])
	case "rm":
		return a.accountsRm(ctx, args[1:])
	}
	_, _ = fmt.Fprintf(a.err, "unknown accounts command %q\n", args[0])
	return 2
}

func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReaderSize(io.LimitReader(r, 4096), 4096).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func csv(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func quoteList(l []string) string {
	q := make([]string, len(l))
	for i, s := range l {
		q[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func (a *app) accountsAdd(ctx context.Context, args []string) int {
	fs := a.newFlagSet("accounts add")
	cfgPath := fs.String("config", "", "config file")
	prov := fs.String("provider", "github", "platform: github | gitlab | gitea | forgejo | bitbucket | azuredevops | git")
	base := fs.String("base-url", "", "API base URL (GitHub Enterprise: https://host/api/v3, self-managed GitLab: https://host)")
	stdin := fs.Bool("token-stdin", false, "read the token from the first line of stdin")
	device := fs.Bool("device", false, "log in with the OAuth device flow")
	clientID := fs.String("client-id", "", "OAuth/GitHub App client id for --device")
	scope := fs.String("scope", "", "OAuth scope for --device (leave empty for GitHub Apps)")
	webURL := fs.String("web-url", "", "web base URL for --device (GitHub Enterprise: https://host)")
	tokEnv := fs.String("token-env", "", "read the token from this environment variable instead of the keychain")
	tokFile := fs.String("token-file", "", "read the token from this file (mode 600) instead of the keychain")
	urls := fs.String("urls", "", "comma-separated clone URLs (generic git: https or ssh, no password inside)")
	noCred := fs.Bool("no-credential", false, "generic git only: use SSH keys or public repositories, no token")
	include := fs.String("include", "", "comma-separated include globs, e.g. me/*,my-org/*")
	exclude := fs.String("exclude", "", "comma-separated exclude globs")
	name, rest := "", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, rest = args[0], args[1:]
	}
	if fs.Parse(rest) != nil {
		return 2
	}
	if name == "" || fs.NArg() != 0 {
		_, _ = fmt.Fprintln(a.err, "usage: repo-keeper accounts add <name> [flags]")
		return 2
	}
	acct := config.Account{Name: name, Provider: *prov, BaseURL: *base, TokenEnv: *tokEnv, TokenFile: *tokFile, URLs: csv(*urls), Include: csv(*include), Exclude: csv(*exclude)}
	cfg, path, err := a.loadConfig(*cfgPath)
	if err != nil {
		_, _ = fmt.Fprintf(a.err, "%v\n(run `repo-keeper init` first)\n", err)
		return 1
	}
	store := a.secretStore(cfg)
	if _, dup := findAccount(cfg, name); dup {
		_, _ = fmt.Fprintf(a.err, "account %q already exists in %s\n", name, path)
		return 1
	}

	external := *tokEnv != "" || *tokFile != ""
	var tok secrets.Token
	switch {
	case *noCred && acct.Provider != "git":
		_, _ = fmt.Fprintln(a.err, "--no-credential is only for generic git accounts")
		return 2
	case *noCred && (external || *stdin || *device):
		_, _ = fmt.Fprintln(a.err, "--no-credential cannot be combined with a credential source")
		return 2
	case *noCred:
		// nothing to resolve
	case external:
		if tok, err = a.credSource(acct).Resolve(store); err != nil {
			_, _ = fmt.Fprintln(a.err, err)
			return 1
		}
	case *device && acct.Provider != "github":
		_, _ = fmt.Fprintf(a.err, "device login is only available for github; for %s use a personal, project or group access token with read_api and read_repository (--token-stdin, --token-file or --token-env)\n", acct.Provider)
		return 2
	case *device:
		if tok, err = a.deviceLogin(ctx, *clientID, *scope, *webURL); err != nil {
			_, _ = fmt.Fprintln(a.err, err)
			return 1
		}
	case *stdin:
		line, err := readLine(a.in)
		if err != nil || line == "" {
			_, _ = fmt.Fprintln(a.err, "no token on stdin")
			return 1
		}
		tok = secrets.New(line)
	default:
		_, _ = fmt.Fprintln(a.err, "choose a credential source: --token-stdin, --device, --token-env or --token-file")
		return 2
	}

	p, _, err := a.newProvider(acct, tok)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	auth, err := p.CheckAuth(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(a.err, "credential rejected: %v\n", err)
		return 1
	}
	a.printAuth(auth)
	if !external && !*noCred {
		if err := store.Set("account/"+name, tok); err != nil {
			_, _ = fmt.Fprintln(a.err, err, "\nHint: no keychain? use --token-file or --token-env instead.")
			return 1
		}
	}
	stanza := stanzaFor(acct)
	// #nosec G304 -- user-chosen config path
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // see #nosec above
	if err == nil {
		_, err = f.WriteString(stanza)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(a.err, "token stored, but could not update %s: %v\nAdd this yourself:\n%s", path, err, stanza)
		return 1
	}
	a.printf("added account %q to %s\n", name, path)
	if cfg.General.Root == "" {
		a.printf("Set [general] root = \"/path/to/clones\" in the config; repositories are cloned beneath it.\n")
	}
	return 0
}

func stanzaFor(ac config.Account) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n[[account]]\nname = %q\nprovider = %q\n", ac.Name, ac.Provider)
	if ac.BaseURL != "" {
		fmt.Fprintf(&b, "base_url = %q\n", ac.BaseURL)
	}
	if ac.TokenEnv != "" {
		fmt.Fprintf(&b, "token_env = %q\n", ac.TokenEnv)
	}
	if ac.TokenFile != "" {
		fmt.Fprintf(&b, "token_file = %q\n", ac.TokenFile)
	}
	if len(ac.URLs) > 0 {
		fmt.Fprintf(&b, "urls = %s\n", quoteList(ac.URLs))
	}
	if len(ac.Include) > 0 {
		fmt.Fprintf(&b, "include = %s\n", quoteList(ac.Include))
	}
	if len(ac.Exclude) > 0 {
		fmt.Fprintf(&b, "exclude = %s\n", quoteList(ac.Exclude))
	}
	return b.String()
}

func (a *app) deviceLogin(ctx context.Context, clientID, scope, webURL string) (secrets.Token, error) {
	if clientID == "" {
		return secrets.Token{}, errors.New("--device needs --client-id (register a GitHub App with device flow enabled and read-only permissions; see docs/PROVIDERS.md)")
	}
	lim := ratelimit.New(ratelimit.Config{}, clock.Real{}, nil)
	hc, err := httpx.New(httpx.Config{UserAgent: "repo-keeper/" + version, Limiter: lim})
	if err != nil {
		return secrets.Token{}, err
	}
	d := &github.Device{HTTP: hc, Clock: a.clock, WebBase: webURL, ClientID: clientID, Scope: scope}
	dc, err := d.Start(ctx)
	if err != nil {
		return secrets.Token{}, err
	}
	a.printf("Open %s and enter the code: %s\nWaiting for authorisation (Ctrl-C to abort)...\n", dc.VerificationURI, dc.UserCode)
	return d.Poll(ctx, dc)
}

func (a *app) printAuth(au provider.Auth) {
	if au.Login == "" {
		a.printf("no login to check (a plain list of repositories)\n")
		return
	}
	a.printf("authenticated as %s", au.Login)
	if len(au.Scopes) > 0 {
		a.printf(" (scopes: %s)", strings.Join(au.Scopes, ", "))
	}
	if !au.Expires.IsZero() {
		a.printf(" (expires %s)", au.Expires.Format("2006-01-02"))
	}
	a.printf("\n")
	for _, w := range au.Warnings {
		a.printf("  warning: %s\n", w)
	}
}

func (a *app) accountsList(_ context.Context, args []string) int {
	fs := a.newFlagSet("accounts list")
	cfgPath := fs.String("config", "", "config file")
	if fs.Parse(args) != nil {
		return 2
	}
	cfg, _, err := a.loadConfig(*cfgPath)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	store := a.secretStore(cfg)
	tw := tabwriter.NewWriter(a.out, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tPROVIDER\tAPI\tCREDENTIAL\tAVAILABLE")
	for _, ac := range cfg.Accounts {
		src, avail := "keychain", "yes"
		switch {
		case ac.TokenEnv != "":
			src = "env:" + ac.TokenEnv
		case ac.TokenFile != "":
			src = "file:" + ac.TokenFile
		}
		if tok, err := a.credSource(ac).Resolve(store); err != nil {
			avail = "no (" + shortErr(err) + ")"
		} else if tok.IsZero() {
			src = "none (SSH keys or public)"
		}
		base := ac.BaseURL
		if base == "" {
			base = "(default)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ac.Name, ac.Provider, base, src, avail)
	}
	_ = tw.Flush()
	return 0
}

func shortErr(err error) string {
	if errors.Is(err, secrets.ErrNotFound) {
		return "not stored"
	}
	return strings.TrimPrefix(err.Error(), "secrets: ")
}

func (a *app) accountsCheck(ctx context.Context, args []string) int {
	fs := a.newFlagSet("accounts check")
	cfgPath := fs.String("config", "", "config file")
	name, rest := "", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, rest = args[0], args[1:]
	}
	if fs.Parse(rest) != nil || name == "" {
		_, _ = fmt.Fprintln(a.err, "usage: repo-keeper accounts check <name>")
		return 2
	}
	cfg, _, err := a.loadConfig(*cfgPath)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	store := a.secretStore(cfg)
	ac, ok := findAccount(cfg, name)
	if !ok {
		_, _ = fmt.Fprintf(a.err, "no account %q in the configuration\n", name)
		return 1
	}
	tok, err := a.credSource(ac).Resolve(store)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	p, lim, err := a.newProvider(ac, tok)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	au, err := p.CheckAuth(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(a.err, "FAIL %v\n", err)
		return 1
	}
	a.printAuth(au)
	if q := lim.Budget(p.APIHost()); q.Limit > 0 {
		a.printf("API quota: %d/%d remaining, resets %s\n", q.Remaining, q.Limit, q.Reset.Format(time.RFC3339))
	}
	return 0
}

func (a *app) accountsRm(_ context.Context, args []string) int {
	fs := a.newFlagSet("accounts rm")
	cfgPath := fs.String("config", "", "config file")
	name, rest := "", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, rest = args[0], args[1:]
	}
	if fs.Parse(rest) != nil || name == "" {
		_, _ = fmt.Fprintln(a.err, "usage: repo-keeper accounts rm <name>")
		return 2
	}
	cfg, _, _ := a.loadConfig(*cfgPath) // best effort: only needed to know which backend holds the secret
	store := a.secretStore(cfg)
	if err := store.Delete("account/" + name); err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	a.printf("deleted the stored credential for %q.\nRemove its [[account]] block from the config to stop syncing it; cloned repositories are never deleted.\n", name)
	return 0
}

func (a *app) cmdDiscover(ctx context.Context, args []string) int {
	fs := a.newFlagSet("discover")
	cfgPath := fs.String("config", "", "config file")
	only := fs.String("account", "", "limit to one account")
	if fs.Parse(args) != nil {
		return 2
	}
	cfg, _, err := a.loadConfig(*cfgPath)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	store := a.secretStore(cfg)
	code := 0
	for _, ac := range cfg.Accounts {
		if *only != "" && ac.Name != *only {
			continue
		}
		s := cfg.ResolveAccount(ac)
		tok, err := a.credSource(ac).Resolve(store)
		if err != nil {
			_, _ = fmt.Fprintf(a.err, "%s: %v\n", ac.Name, err)
			code = 1
			continue
		}
		p, _, err := a.newProvider(ac, tok)
		if err != nil {
			_, _ = fmt.Fprintf(a.err, "%s: %v\n", ac.Name, err)
			code = 1
			continue
		}
		repos, err := p.ListRepos(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(a.err, "%s: %v\n", ac.Name, err)
			code = 1
			continue
		}
		tw := tabwriter.NewWriter(a.out, 2, 4, 2, ' ', 0)
		a.printf("account %s: %d repositories visible\n", ac.Name, len(repos))
		for _, r := range repos {
			why := ""
			switch {
			case r.Disabled:
				why = "skip (disabled)"
			case s.SkipArchivedRepos && r.Archived:
				why = "skip (archived)"
			case s.SkipForks && r.Fork:
				why = "skip (fork)"
			case !provider.Matches(ac.Include, ac.Exclude, r):
				why = "skip (filtered)"
			}
			dest := ""
			if why == "" {
				p, perr := provider.LocalPath(cfg.General.Root, provider.Kind(ac.Provider), r)
				switch {
				case perr != nil:
					why = "skip (" + perr.Error() + ")"
				case exists(p):
					why, dest = "tracked", p
				default:
					why, dest = "clone", p
				}
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.FullName, why, dest)
		}
		_ = tw.Flush()
	}
	return code
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// secretStore picks the secret backend from the configuration: the OS keychain (default) or the
// passphrase-encrypted file (headless hosts; passphrase from $REPO_KEEPER_PASSPHRASE_FILE).
func (a *app) secretStore(cfg config.Config) secrets.Store {
	if a.secrets != nil {
		return a.secrets // injected (tests)
	}
	if cfg.General.Secrets == "file" {
		p := cfg.General.SecretsFile
		if p == "" {
			if dir, err := paths.StateDir(); err == nil {
				p = filepath.Join(dir, "secrets.enc")
			}
		}
		return &secrets.EncryptedFile{Path: p, Passphrase: secrets.PassphraseFromEnvFile}
	}
	return secrets.Keyring{}
}
