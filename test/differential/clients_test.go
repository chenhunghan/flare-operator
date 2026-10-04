//go:build differential

package differential

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhunghan/flare-operator/test/differential/harness"
)

// cacheDir is where `make differential-tools` puts the pinned clients.
func cacheDir() string {
	if d := os.Getenv("FLARE_DIFF_CACHE"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "flare-operator", "differential")
}

// findClient returns the path of a pinned client binary: $envVar, else the cache location,
// else (only when allowPath) the one on PATH. It skips the test when none exists.
func findClient(t *testing.T, envVar, cached, name string, allowPath bool) string {
	t.Helper()
	if p := os.Getenv(envVar); p != "" {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s=%s: %v", envVar, p, err)
		}
		return p
	}
	if _, err := os.Stat(cached); err == nil {
		return cached
	}
	if allowPath {
		if p, err := exec.LookPath(name); err == nil {
			t.Logf("%s: pinned copy not found at %s; using %s from PATH (version may differ)", name, cached, p)
			return p
		}
	}
	t.Skipf("%s not installed (looked at $%s and %s); run `make differential-tools`", name, envVar, cached)
	return ""
}

// result is one client invocation.
type result struct {
	args           []string
	stdout, stderr string
	err            error
}

func (r result) String() string {
	return fmt.Sprintf("%s: err=%v\nstdout:\n%s\nstderr:\n%s", strings.Join(r.args, " "), r.err, r.stdout, r.stderr)
}

// failed returns an error describing a failed invocation, or nil.
func (r result) failed() error {
	if r.err == nil {
		return nil
	}
	return fmt.Errorf("%s: %v: %s", strings.Join(r.args, " "), r.err, lastLines(r.stderr+"\n"+r.stdout, 12))
}

func lastLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(stripANSI(l))
		if l == "" || strings.Contains(l, "Proxy environment variables detected") || strings.HasPrefix(l, "🪵") {
			continue
		}
		keep = append(keep, l)
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, " | ")
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// run executes bin with args in dir and env (appended to a minimal base environment).
func run(t *testing.T, dir string, env []string, bin string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(baseEnv(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Stdin = strings.NewReader("")
	err := cmd.Run()
	r := result{args: append([]string{filepath.Base(bin)}, args...), stdout: stripANSI(out.String()), stderr: stripANSI(errb.String()), err: err}
	t.Logf("$ %s (exit %v)", strings.Join(r.args, " "), err)
	return r
}

// baseEnv passes through only what a Node or Go binary needs to start. Every variable that
// could point a client at the real Cloudflare API or at real credentials is left out.
func baseEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "TMPDIR", "LANG", "TERM", "NVM_DIR", "NVM_BIN"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env,
		// Safety net: any HTTPS/HTTP request that does not go to loopback (telemetry, update
		// checks, a mistyped base URL) goes to a closed port instead of the internet.
		"HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9",
		"https_proxy=http://127.0.0.1:9", "http_proxy=http://127.0.0.1:9",
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost",
	)
}

// logRequests logs the requests made since mark.
func logRequests(t *testing.T, f *harness.Fake, mark int) {
	t.Helper()
	harness.Log(t, f.RequestsSince(mark))
}
