package toolexec

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// maxRepositoryReadBytes is how much content one repository.read call may return.
//
// A LIMIT THAT REFUSES RATHER THAN TRUNCATES. Returning the first 256 KiB of a megabyte file with a
// `truncated: true` beside it is the shape of defect this project keeps finding in its own artefacts:
// the answer looks like the file, a model reads it as the file, and the flag is in a field nobody
// consumed. So an oversized read FAILS, and the error names the size and the two arguments that let a
// caller ask for part of it on purpose. A tool that cannot give you the whole thing should say so, not
// hand you some of it.
const maxRepositoryReadBytes = 256 * 1024

// RegisterRepositoryReadTool wires `repository.read`: a file out of the repository, and nothing else.
//
// WHY THIS EXISTS AT ALL, which is not "the agent needed it". `repository.read` has been in
// examples/deep-research/policy_bundle.yaml and in the agent manifest's `tools.allow` since those files
// were written, with no implementation behind it — so the manifest declared a tool the gateway answered
// `unknown tool` for. Found by CI-001 counting what the suite skips: the reference deployment
// registered exactly two tools and BOTH need something external (`search.web` the public web, which
// rate-limits us, and `search.rag` the platform's embedding credentials), so there was no permitted
// tool that runs offline and the only end-to-end tool test was flaky by construction.
//
// Registering a stub would have closed that and is exactly what TOOL-007 removed: a tool that answers
// `{"status":"executed"}` without doing anything is worse than an absent one, because an absent one
// fails on the first call. This reads a real file off a real disk.
//
// CONTAINMENT IS os.Root's, NOT OURS. Every path check written by hand gets symlinks wrong: a
// `filepath.Clean`-and-prefix-compare passes a symlink inside the root that points at /etc/passwd,
// because cleaning a path does not follow links. Go 1.24's os.Root performs each operation relative to
// the directory and refuses to escape it, links included — so the guarantee comes from the standard
// library rather than from this file being clever, which is the same reasoning as using Argus's SDK
// instead of hand-rolling their attributes. TestRepositoryReadCannotEscapeItsRoot measures it instead
// of trusting the documentation, including the symlink case.
func RegisterRepositoryReadTool(e *Executor, root *os.Root, rootPath string) {
	e.Register("repository.read", func(args map[string]any) (map[string]any, error) {
		rel, _ := args["path"].(string)
		rel = strings.TrimSpace(rel)
		if rel == "" {
			return nil, fmt.Errorf("toolexec: repository.read: missing required string arg %q", "path")
		}
		// Absolute paths are refused BEFORE os.Root sees them. os.Root would reject them too, with its
		// own message about the operation; refusing here lets the error say what a caller should have
		// sent, because the fix is "make it relative to the repository root" and nothing in a path error
		// says that.
		if path.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.Contains(rel, `\`) {
			return nil, fmt.Errorf("toolexec: repository.read: %q is absolute — paths are relative to the repository root", rel)
		}

		start, end, err := repositoryLineRange(args)
		if err != nil {
			return nil, err
		}

		// STAT BEFORE OPEN, and the order is the whole point of these three checks.
		//
		// The first version of this did it the other way round — open, then check the mode — and the FIFO
		// test hung the suite for ten minutes. `open(2)` on a FIFO with no writer BLOCKS, so the
		// regular-file guard was placed after the operation it exists to prevent and could never run. The
		// same shape as a bound that folds its own input before checking for the fold: a guard that
		// arrives after the damage.
		//
		// Stat and not Lstat: the target's type is what matters, and os.Root keeps the resolution inside
		// the root either way.
		info, err := root.Stat(rel)
		if err != nil {
			// os.Root's refusal is reported verbatim. A traversal attempt and a missing file both land
			// here and they are DIFFERENT facts, so collapsing them into "not found" would hide the first
			// — and the first is the one worth seeing in a trace.
			return nil, fmt.Errorf("toolexec: repository.read: %w", err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("toolexec: repository.read: %q is a directory, not a file", rel)
		}
		if !info.Mode().IsRegular() {
			// A FIFO or a device. `mode` is in the message because "not a regular file" alone leaves the
			// reader guessing what it is.
			return nil, fmt.Errorf("toolexec: repository.read: %q is not a regular file (mode %s)", rel, info.Mode())
		}

		f, err := root.Open(rel)
		if err != nil {
			return nil, fmt.Errorf("toolexec: repository.read: %w", err)
		}
		defer f.Close()

		result := map[string]any{"status": "executed", "tool": "repository.read", "path": rel}
		if start == 0 {
			// The whole file, which only happens when the whole file fits. The size is checked BEFORE the
			// read so a 2 GB file is never loaded into this process to then be rejected.
			if info.Size() > maxRepositoryReadBytes {
				return nil, fmt.Errorf(
					"toolexec: repository.read: %q is %d bytes, over the %d-byte limit for one read — "+
						"pass start_line and end_line to read part of it",
					rel, info.Size(), maxRepositoryReadBytes)
			}
			raw, err := io.ReadAll(f)
			if err != nil {
				return nil, fmt.Errorf("toolexec: repository.read: %w", err)
			}
			if !utf8.Valid(raw) {
				// JSON cannot carry invalid UTF-8: encoding it replaces each bad byte with U+FFFD, so a
				// binary file would come back as plausible-looking mojibake and a model would read it as
				// the file's contents. Refusing is the only answer that is not a quiet corruption.
				return nil, fmt.Errorf("toolexec: repository.read: %q is not valid UTF-8 text (%d bytes) — this tool reads text", rel, len(raw))
			}
			content := string(raw)
			result["content"] = content
			result["bytes"] = len(raw)
			// Counted only here, where the whole file was actually read. With a line range the count is
			// absent rather than partial: reporting the lines we happened to read as the file's total is
			// the same lie as a truncated body without a flag.
			result["lines_total"] = countLines(content)
			return result, nil
		}

		content, lastLine, err := readLineRange(f, start, end)
		if err != nil {
			return nil, fmt.Errorf("toolexec: repository.read: %q: %w", rel, err)
		}
		result["content"] = content
		result["bytes"] = len(content)
		result["start_line"] = start
		result["end_line"] = lastLine
		return result, nil
	})
}

// repositoryLineRange reads the optional 1-based inclusive range. start == 0 means "the whole file".
func repositoryLineRange(args map[string]any) (start, end int, err error) {
	// float64 because this arrives as decoded JSON. An int that came through as something else is
	// reported rather than silently treated as absent, which would read the whole file instead of the
	// range the caller asked for — a bigger answer than they wanted, which is the wrong direction to
	// guess in.
	read := func(key string) (int, error) {
		raw, present := args[key]
		if !present || raw == nil {
			return 0, nil
		}
		f, ok := raw.(float64)
		if !ok {
			return 0, fmt.Errorf("toolexec: repository.read: %s must be a number, got %T", key, raw)
		}
		if f != float64(int(f)) {
			return 0, fmt.Errorf("toolexec: repository.read: %s must be a whole number, got %v", key, f)
		}
		return int(f), nil
	}
	if start, err = read("start_line"); err != nil {
		return 0, 0, err
	}
	if end, err = read("end_line"); err != nil {
		return 0, 0, err
	}
	switch {
	case start == 0 && end == 0:
		return 0, 0, nil
	case start == 0:
		// end without start. Defaulting start to 1 would be a guess, and the guess is unbounded: a caller
		// who meant "the last ten lines" would get the first N instead and never know.
		return 0, 0, fmt.Errorf("toolexec: repository.read: end_line was given without start_line")
	case start < 1:
		return 0, 0, fmt.Errorf("toolexec: repository.read: start_line is 1-based, got %d", start)
	case end == 0:
		return start, 0, nil // open-ended: to the end of the file, still bounded by the byte limit
	case end < start:
		return 0, 0, fmt.Errorf("toolexec: repository.read: end_line %d is before start_line %d", end, start)
	}
	return start, end, nil
}

// readLineRange returns lines [start, end] (1-based, inclusive; end == 0 means to EOF) and the last
// line number actually returned.
func readLineRange(f io.Reader, start, end int) (string, int, error) {
	scanner := bufio.NewScanner(f)
	// A long single line is a real shape — minified JS, a one-line JSON fixture — and bufio's default
	// 64 KiB would fail on it with "token too long", which says nothing about the file.
	scanner.Buffer(make([]byte, 0, 64*1024), maxRepositoryReadBytes+1)

	var b strings.Builder
	line, last := 0, 0
	for scanner.Scan() {
		line++
		if line < start {
			continue
		}
		if end != 0 && line > end {
			break
		}
		if b.Len()+len(scanner.Bytes())+1 > maxRepositoryReadBytes {
			return "", 0, fmt.Errorf(
				"the requested range exceeds the %d-byte limit for one read (stopped at line %d) — ask for fewer lines",
				maxRepositoryReadBytes, line)
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.Write(scanner.Bytes())
		last = line
	}
	if err := scanner.Err(); err != nil {
		return "", 0, err
	}
	if last == 0 {
		// Asking past the end of a file is a mistake worth reporting, not an empty success: an empty
		// string reads as "that part of the file is blank".
		return "", 0, fmt.Errorf("start_line %d is past the end of the file (%d line(s))", start, line)
	}
	if !utf8.ValidString(b.String()) {
		return "", 0, fmt.Errorf("the requested range is not valid UTF-8 text — this tool reads text")
	}
	return b.String(), last, nil
}

// countLines counts lines the way an editor does: a trailing newline does not add an empty last line.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// secretLookingNames are filenames that mean "this directory is not safe to expose", checked once at
// startup by CheckRepositoryRoot. Patterns are matched against the base name, case-insensitively.
var secretLookingNames = []string{
	".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx",
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
	"credentials", "credentials.json", ".npmrc", ".netrc", ".pgpass", "kubeconfig", ".dockercfg",
	"service-account*.json", "*.jks",
}

// CheckRepositoryRoot refuses a root that obviously should not be readable by an agent.
//
// WHY THIS IS A STARTUP CHECK AND NOT A PER-CALL FILTER, because the difference decides whether it is
// worth anything. A deny-list consulted on every read is a security boundary made of a list somebody
// has to keep complete, and it fails on the first name nobody thought of. This is not that: it is an
// assertion about the DEPLOYMENT, made once, that fails loudly and names the file. The boundary is
// still the root itself — os.Root — and this only stops an operator from pointing that root somewhere
// catastrophic.
//
// IT IS NOT HYPOTHETICAL AND THE MEASUREMENT IS WHY IT EXISTS. The first version of this feature
// mounted the whole repository as the root, because "repository.read should read the repository". A
// probe through the real gateway then returned `.env` — 1654 bytes including PROMETHEUS_CLIENT_SECRET
// and OPENAI_COMPATIBLE_API_KEY — to a caller holding the public development token. The tool was doing
// exactly what it was told; the configuration was the defect, and nothing anywhere would have said so.
//
// What it does NOT claim: that a root which passes is safe. A directory with a secret in a file this
// list does not name still exposes it, and the doc below says so rather than letting a green startup
// imply otherwise.
func CheckRepositoryRoot(rootPath string) error {
	var offenders []string
	err := filepath.WalkDir(rootPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subdirectory is not a reason to refuse to start: the walk is a courtesy check,
			// and a root the gateway cannot fully read is a root it cannot fully expose either.
			return nil //nolint:nilerr // see comment
		}
		name := strings.ToLower(d.Name())
		if d.IsDir() {
			// .git holds every version of every file ever committed, which makes it both enormous and the
			// single worst thing to expose; skipping it also keeps this walk fast on a real repository.
			if name == ".git" || name == "node_modules" || name == ".venv" {
				return fs.SkipDir
			}
			if name == ".ssh" || name == ".aws" || name == ".gnupg" {
				offenders = append(offenders, p)
				return fs.SkipDir
			}
			return nil
		}
		for _, pattern := range secretLookingNames {
			if ok, _ := filepath.Match(pattern, name); ok {
				// `.env.example` is the one exception, and it is deliberate: it exists to be read and
				// committed, and refusing a root for containing it would make this check something an
				// operator disables.
				if name == ".env.example" || name == ".env.sample" {
					return nil
				}
				offenders = append(offenders, p)
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("toolexec: walking the repository root %s: %w", rootPath, err)
	}
	if len(offenders) > 0 {
		shown := offenders
		if len(shown) > 5 {
			shown = shown[:5]
		}
		return fmt.Errorf(
			"toolexec: %s contains %d file(s) that look like credentials and would be readable by any agent "+
				"permitted repository.read: %s — point AEON_REPOSITORY_ROOT at a narrower directory. "+
				"NOTE: a root that passes this check is not therefore safe; the check names the obvious cases "+
				"and the root is the boundary",
			rootPath, len(offenders), strings.Join(shown, ", "))
	}
	return nil
}
