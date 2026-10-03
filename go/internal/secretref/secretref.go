// Package secretref resolves a secret from either an environment variable or a FILE pointing at it,
// and removes both from this process's environment once it has.
//
// WHY A FILE AT ALL. Until SEC-006 every credential in this project arrived the same way: `.env` ->
// compose environment -> process environment. That works and it is why `make dev` needs no secret
// provisioning step. It also means the value is in `docker inspect`, in the container's initial
// environment block, and in the environment of every process the service spawns — and this
// repository spawns real ones: the worker starts the `claude` CLI (which inherits os.environ
// wholesale, see python/aeon_adapters/claude_agent_sdk/adapter.py) and `aeon eval run` execs a
// Python interpreter with no cmd.Env, so it inherits everything too.
//
// The `<NAME>_FILE` convention is deliberately boring: it is what the postgres/mysql/redis images
// use, and it is what every real secret store can already produce. Docker Compose `secrets:`,
// Kubernetes secret volumes, Vault Agent templates and a SOPS decrypt step all end in "there is a
// file". So this package is the seam that makes a real secret store usable without this project
// having to know which one — which is exactly the entry criterion backlog.md recorded for this
// entry, read the other way round: we cannot bring Vault into the repo, but we can stop being the
// reason it would not help.
//
// WHAT IT DOES NOT FIX, measured rather than assumed (see docs/secrets.md): unsetting a variable
// does NOT rewrite /proc/<pid>/environ on Linux. That file reports the environment block the
// process was exec'd with, so a secret that arrived through the environment stays visible there for
// the life of the process no matter what this package does. Scrubbing closes os.Environ() and
// therefore every child process; only never putting the value in the environment closes
// /proc/<pid>/environ and `docker inspect`. That is the difference between the two paths, and it is
// the reason the file path exists rather than just the scrub.
package secretref

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// FileSuffix is appended to a variable's name to name the file holding its value:
// PROMETHEUS_CLIENT_SECRET_FILE for PROMETHEUS_CLIENT_SECRET.
const FileSuffix = "_FILE"

// Source says where a resolved value came from, for startup logs that need to state which path a
// deployment is on without printing the value.
type Source string

const (
	// SourceAbsent: neither variable was set. The value is empty and that is not an error — a
	// deployment with no Anthropic key simply has no Anthropic adapter.
	SourceAbsent Source = "absent"
	// SourceEnv: the plain environment variable. Convenient, and visible in `docker inspect`.
	SourceEnv Source = "env"
	// SourceFile: <NAME>_FILE. Never in the environment block, never inherited by a child.
	SourceFile Source = "file"
)

type resolved struct {
	value  string
	source Source
	err    error
}

var (
	mu    sync.Mutex
	cache = map[string]resolved{}
)

// Take resolves name and removes both name and name+FileSuffix from this process's environment.
//
// MEMOISED, and not as an optimisation: several credentials in this project are read more than once
// per process (PROMETHEUS_CLIENT_SECRET is read twice in aeon-modelgw — routing and the MDL-015
// scope derivation — and twice in aeon-toolgw), and a destructive read that is not memoised would
// hand the first caller a secret and the second an empty string. The failure would be a provider
// that authenticates on one code path and 401s on another, in the same process, which reads as the
// provider being flaky.
//
// Errors are for a deployment that is configured WRONG, never for one that is not configured:
//
//   - both name and name_FILE set: refused. Two sources for one secret means a rotation can be
//     applied to one of them and have no effect, with everything looking configured. There is no
//     precedence rule here on purpose; any rule picks a winner silently.
//   - name_FILE set and unreadable, or holding nothing: refused. A secret store that is down, or a
//     volume that did not mount, must not come back as "no credential configured" — that state is
//     indistinguishable from a deployment that deliberately has no such provider, so routing would
//     fall through to the next candidate and the run would quietly cost something different and
//     answer from a different model.
func Take(name string) (string, Source, error) {
	mu.Lock()
	defer mu.Unlock()
	if got, ok := cache[name]; ok {
		return got.value, got.source, got.err
	}
	got := read(name)
	cache[name] = got
	// Scrub even on refusal: the value is in the environment either way, and a process that is
	// about to log.Fatal still has time to spawn nothing — but a caller that chooses to continue
	// past the error should not keep the plaintext in os.Environ() as a consolation prize.
	os.Unsetenv(name)
	os.Unsetenv(name + FileSuffix)
	return got.value, got.source, got.err
}

func read(name string) resolved {
	direct, directSet := os.LookupEnv(name)
	path, pathSet := os.LookupEnv(name + FileSuffix)
	// An empty variable is not a configured one. `FOO=` is what both an unset compose variable
	// (`${FOO:-}`) and `.env.example` copied without editing produce, and treating it as "set" is
	// how the ambiguity refusal below would start firing on deployments that configured one source.
	directSet = directSet && direct != ""
	pathSet = pathSet && strings.TrimSpace(path) != ""

	switch {
	case directSet && pathSet:
		return resolved{source: SourceAbsent, err: fmt.Errorf(
			"secretref: both %s and %s%s are set; remove one — with two sources for one secret, rotating either leaves the other in force and nothing fails",
			name, name, FileSuffix)}
	case pathSet:
		path = strings.TrimSpace(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			return resolved{source: SourceAbsent, err: fmt.Errorf(
				"secretref: %s%s points at %s, which cannot be read: %w", name, FileSuffix, path, err)}
		}
		// Trim trailing newlines only. `echo secret > file`, a text editor saving the file and
		// `kubectl create secret --from-file` all differ by exactly one \n, and a credential with a
		// stray newline fails at the provider as an authentication error rather than as a
		// configuration one. Nothing else is trimmed: leading and interior whitespace can be part
		// of a secret, and a resolver that quietly rewrites a credential is worse than one that
		// hands over what the file says.
		value := strings.TrimRight(string(raw), "\r\n")
		if value == "" {
			return resolved{source: SourceAbsent, err: fmt.Errorf(
				"secretref: %s%s points at %s, which is empty; an unmounted volume and a provider this deployment deliberately does not have would otherwise look the same",
				name, FileSuffix, path)}
		}
		return resolved{value: value, source: SourceFile}
	case directSet:
		return resolved{value: direct, source: SourceEnv}
	default:
		return resolved{source: SourceAbsent}
	}
}

// TakeOrFatal is Take for a credential whose misconfiguration should stop the process, with the
// error wrapped so the log line names the service. It returns the value and whether anything was
// configured at all; callers decide what an absent credential means, because for most of them it
// means "this deployment has no such provider" and not "this deployment is broken".
func TakeOrFatal(serviceName, name string, fatal func(args ...any)) (string, bool) {
	value, source, err := Take(name)
	if err != nil {
		fatal(fmt.Sprintf("%s: %v", serviceName, err))
		return "", false
	}
	return value, source != SourceAbsent
}

// ResetForTest drops the memoised values. Only tests need this; a process resolves each secret once.
func ResetForTest() {
	mu.Lock()
	defer mu.Unlock()
	cache = map[string]resolved{}
}
