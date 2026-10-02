package toolexec

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newRepositoryFixture builds a small repository plus a file OUTSIDE it, and returns the executor and
// the two roots. The outside file is the target of every escape attempt below: an escape that succeeds
// returns its contents, so the assertions can check for a value rather than for an error alone.
func newRepositoryFixture(t *testing.T) (*Executor, string, string) {
	t.Helper()
	base := t.TempDir()
	repoDir := filepath.Join(base, "repo")
	outsideDir := filepath.Join(base, "outside")
	for _, d := range []string{repoDir, outsideDir, filepath.Join(repoDir, "docs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	write := func(p, content string) {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	write(filepath.Join(repoDir, "README.md"), "# Aeon\nline two\nline three\n")
	write(filepath.Join(repoDir, "docs", "adr.md"), strings.Repeat("a line\n", 10))
	write(filepath.Join(outsideDir, "secret.txt"), "SECRET-OUTSIDE-THE-ROOT")

	root, err := os.OpenRoot(repoDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	e := &Executor{fns: map[string]ExecuteFunc{}}
	RegisterRepositoryReadTool(e, root, repoDir)
	return e, repoDir, outsideDir
}

func callRead(t *testing.T, e *Executor, args map[string]any) (map[string]any, error) {
	t.Helper()
	return e.Execute("repository.read", args)
}

// TestRepositoryReadReturnsRealFileContents is TOOL-008's happy path: a real file off a real disk.
//
// `repository.read` was in the policy bundle and in the agent manifest's tools.allow with nothing
// behind it, so the manifest declared a tool the gateway answered `unknown tool` for. The point of this
// test is that the content is the file's, not that the call succeeded.
func TestRepositoryReadReturnsRealFileContents(t *testing.T) {
	e, _, _ := newRepositoryFixture(t)

	out, err := callRead(t, e, map[string]any{"path": "README.md"})
	if err != nil {
		t.Fatalf("repository.read: %v", err)
	}
	if got := out["content"]; got != "# Aeon\nline two\nline three\n" {
		t.Fatalf("content = %q", got)
	}
	if out["bytes"] != 27 {
		t.Fatalf("bytes = %v, want 27", out["bytes"])
	}
	if out["lines_total"] != 3 {
		t.Fatalf("lines_total = %v, want 3 — a trailing newline does not add an empty last line", out["lines_total"])
	}

	t.Run("a nested path resolves under the root", func(t *testing.T) {
		out, err := callRead(t, e, map[string]any{"path": "docs/adr.md"})
		if err != nil {
			t.Fatalf("repository.read: %v", err)
		}
		if out["lines_total"] != 10 {
			t.Fatalf("lines_total = %v, want 10", out["lines_total"])
		}
	})
}

// TestRepositoryReadCannotEscapeItsRoot is the assertion the whole tool rests on, and it is MEASURED
// rather than taken from os.Root's documentation.
//
// The symlink case is the one that matters. A hand-written guard that cleans the path and compares the
// prefix passes it: `docs/escape` is textually inside the root, and cleaning a path does not follow
// links. Every one of these attempts must come back refused AND must not contain the file's contents —
// checking only for an error would pass against an implementation that returned the secret alongside
// one.
func TestRepositoryReadCannotEscapeItsRoot(t *testing.T) {
	e, repoDir, outsideDir := newRepositoryFixture(t)
	secret := filepath.Join(outsideDir, "secret.txt")

	// A symlink INSIDE the repository pointing at the file outside it.
	if err := os.Symlink(secret, filepath.Join(repoDir, "docs", "escape")); err != nil {
		t.Skipf("cannot create a symlink on this filesystem: %v", err)
	}
	// And one pointing at the whole outside directory, so a path can traverse THROUGH the link.
	if err := os.Symlink(outsideDir, filepath.Join(repoDir, "elsewhere")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	for _, attempt := range []struct {
		name string
		path string
	}{
		{"parent traversal", "../outside/secret.txt"},
		{"traversal with a legitimate prefix", "docs/../../outside/secret.txt"},
		{"deep traversal", "../../../../../../etc/passwd"},
		{"an absolute path", secret},
		{"a symlink to a file outside", "docs/escape"},
		{"a path through a symlinked directory", "elsewhere/secret.txt"},
		{"a cleaned-looking path that still escapes", "./docs/./../../outside/secret.txt"},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			out, err := callRead(t, e, map[string]any{"path": attempt.path})
			if err == nil {
				t.Fatalf("the read SUCCEEDED and returned %v — this is a file disclosure, not a test failure", out["content"])
			}
			if out != nil && strings.Contains(fmt.Sprint(out["content"]), "SECRET-OUTSIDE-THE-ROOT") {
				t.Fatalf("the error came back WITH the file's contents: %v", out)
			}
			if strings.Contains(err.Error(), "SECRET-OUTSIDE-THE-ROOT") {
				t.Fatalf("the contents leaked through the error message: %v", err)
			}
		})
	}
}

// TestRepositoryReadRefusesWhatItCannotReturnHonestly covers every way the answer would otherwise be a
// quiet lie: a truncated body, mojibake from a binary file, a hang on a device, an empty string for a
// range past the end.
func TestRepositoryReadRefusesWhatItCannotReturnHonestly(t *testing.T) {
	e, repoDir, _ := newRepositoryFixture(t)

	t.Run("a file over the byte limit, and the error says how to read part of it", func(t *testing.T) {
		big := filepath.Join(repoDir, "big.txt")
		if err := os.WriteFile(big, []byte(strings.Repeat("x", maxRepositoryReadBytes+1)), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := callRead(t, e, map[string]any{"path": "big.txt"})
		if err == nil {
			t.Fatal("an oversized file was returned. Truncating it with a flag is the defect this refuses: " +
				"the answer looks like the file and the flag is in a field nobody read")
		}
		for _, want := range []string{"start_line", "end_line"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the error does not mention %q, so a caller is refused with no way forward: %v", want, err)
			}
		}
	})

	t.Run("a binary file is refused, not returned as mojibake", func(t *testing.T) {
		// 0xff is never valid UTF-8. json.Marshal would replace it with U+FFFD, so the call would succeed
		// and hand a model something that looks like text and is not the file.
		if err := os.WriteFile(filepath.Join(repoDir, "logo.png"), []byte{0x89, 0x50, 0x4e, 0x47, 0xff, 0xfe}, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := callRead(t, e, map[string]any{"path": "logo.png"})
		if err == nil {
			t.Fatal("a binary file came back as text")
		}
		if !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("the refusal does not say why: %v", err)
		}
	})

	t.Run("a directory is refused", func(t *testing.T) {
		if _, err := callRead(t, e, map[string]any{"path": "docs"}); err == nil {
			t.Fatal("reading a directory succeeded")
		}
	})

	t.Run("a FIFO is refused instead of hanging", func(t *testing.T) {
		fifo := filepath.Join(repoDir, "pipe")
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Skipf("cannot create a FIFO here: %v", err)
		}
		// No goroutine writes to it, so an implementation that opens and reads blocks forever. The
		// regular-file check is what makes this return instead of taking the gateway down with it.
		done := make(chan error, 1)
		go func() {
			_, err := callRead(t, e, map[string]any{"path": "pipe"})
			done <- err
		}()
		select {
		case <-time.After(5 * time.Second):
			// An explicit deadline rather than the test's own context: a regression here BLOCKS, so
			// without this the whole package hangs until the go test timeout and the failure reads as
			// "the suite stopped responding" instead of naming this case. Measured: the first version of
			// the tool opened the file before checking its mode, and this hung for ten minutes.
			t.Fatal("reading a FIFO did not return within 5s — the gateway would be hung, not failed. " +
				"The mode check has to happen BEFORE the open: open(2) on a FIFO with no writer blocks")
		case err := <-done:
			if err == nil {
				t.Fatal("reading a FIFO succeeded")
			}
			if !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("the refusal does not say what it was: %v", err)
			}
		}
	})

	t.Run("a range past the end of the file is an error, not an empty string", func(t *testing.T) {
		_, err := callRead(t, e, map[string]any{"path": "README.md", "start_line": 99.0})
		if err == nil {
			t.Fatal("a range past the end returned successfully — an empty body reads as 'that part is blank'")
		}
		if !strings.Contains(err.Error(), "past the end") {
			t.Fatalf("the refusal does not say why: %v", err)
		}
	})

	t.Run("end_line without start_line is refused rather than guessed", func(t *testing.T) {
		if _, err := callRead(t, e, map[string]any{"path": "README.md", "end_line": 2.0}); err == nil {
			t.Fatal("end_line alone was accepted, so start_line was guessed as 1 — a caller who meant " +
				"'the last two lines' got the first two and nothing said so")
		}
	})

	t.Run("a missing path arg is named", func(t *testing.T) {
		if _, err := callRead(t, e, map[string]any{}); err == nil {
			t.Fatal("a call with no path succeeded")
		}
	})
}

// TestRepositoryReadLineRangeReturnsExactlyThoseLines checks the half that makes a big file usable.
func TestRepositoryReadLineRangeReturnsExactlyThoseLines(t *testing.T) {
	e, repoDir, _ := newRepositoryFixture(t)
	numbered := make([]string, 0, 20)
	for i := 1; i <= 20; i++ {
		numbered = append(numbered, fmt.Sprintf("line %d", i))
	}
	if err := os.WriteFile(filepath.Join(repoDir, "numbered.txt"), []byte(strings.Join(numbered, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := callRead(t, e, map[string]any{"path": "numbered.txt", "start_line": 5.0, "end_line": 7.0})
	if err != nil {
		t.Fatalf("repository.read: %v", err)
	}
	if got := out["content"]; got != "line 5\nline 6\nline 7" {
		t.Fatalf("content = %q, want lines 5..7 and nothing else", got)
	}
	if out["start_line"] != 5 || out["end_line"] != 7 {
		t.Fatalf("range reported as %v..%v", out["start_line"], out["end_line"])
	}
	if _, counted := out["lines_total"]; counted {
		t.Fatalf("lines_total is present on a ranged read (%v). The whole file was never read, so any "+
			"total here would be the lines we happened to look at wearing the name of the file's length",
			out["lines_total"])
	}

	t.Run("an open-ended range runs to the end of the file", func(t *testing.T) {
		out, err := callRead(t, e, map[string]any{"path": "numbered.txt", "start_line": 18.0})
		if err != nil {
			t.Fatalf("repository.read: %v", err)
		}
		if got := out["content"]; got != "line 18\nline 19\nline 20" {
			t.Fatalf("content = %q", got)
		}
		if out["end_line"] != 20 {
			t.Fatalf("end_line = %v, want the last line actually returned", out["end_line"])
		}
	})
}

// TestARootHoldingCredentialsIsRefusedAtStartup is the check that exists because of a measurement, not
// a worry.
//
// The first version of TOOL-008 mounted the whole repository as the root — "repository.read should read
// the repository" — and a probe through the real gateway returned `.env`: 1654 bytes including
// PROMETHEUS_CLIENT_SECRET and OPENAI_COMPATIBLE_API_KEY, to a caller holding the public development
// token. The tool was doing exactly what it was told. The defect was the configuration, and nothing
// anywhere would have reported it.
func TestARootHoldingCredentialsIsRefusedAtStartup(t *testing.T) {
	t.Run("a directory with a .env is refused and the file is named", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("API_KEY=real\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		err := CheckRepositoryRoot(dir)
		if err == nil {
			t.Fatal("a root containing .env was accepted")
		}
		if !strings.Contains(err.Error(), ".env") {
			t.Fatalf("the refusal does not name the file, so the operator has to go looking: %v", err)
		}
		if !strings.Contains(err.Error(), "AEON_REPOSITORY_ROOT") {
			t.Fatalf("the refusal does not name the setting to change: %v", err)
		}
	})

	t.Run("a nested credential is found too", func(t *testing.T) {
		dir := t.TempDir()
		nested := filepath.Join(dir, "deploy", "keys")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(nested, "server.pem"), []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := CheckRepositoryRoot(dir); err == nil {
			t.Fatal("a .pem three directories down was accepted — a top-level-only check would pass this")
		}
	})

	t.Run(".env.example is not a credential", func(t *testing.T) {
		// Deliberate: it exists to be read and committed. Refusing a root for containing it would make
		// this check the kind an operator switches off, and then it protects nothing.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".env.example"), []byte("API_KEY=\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := CheckRepositoryRoot(dir); err != nil {
			t.Fatalf("a root with only .env.example was refused: %v", err)
		}
	})

	t.Run("the compose default root passes, and the repository root does not", func(t *testing.T) {
		// Asserted against the real directories, because the whole point is which one the deployment
		// points at. `docs/` is what deploy/compose mounts; the repository root is what it mounted first
		// and what produced the disclosure.
		repoRoot := repositoryRootForTest(t)
		if err := CheckRepositoryRoot(filepath.Join(repoRoot, "docs")); err != nil {
			t.Fatalf("docs/ — the compose default — was refused: %v", err)
		}
		if err := CheckRepositoryRoot(repoRoot); err == nil {
			t.Fatal("the repository root was ACCEPTED. It holds .env on any machine that has run this " +
				"stack, and this test is the one that would have caught the first version of TOOL-008")
		}
	})
}

// repositoryRootForTest locates the repo root from this file's own path.
func repositoryRootForTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// go/internal/toolexec/repository_tool_test.go -> four levels up
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}
