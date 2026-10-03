package secretref

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestASecretFromAFileNeverEntersTheEnvironment is SEC-006's acceptance test: it exercises both
// delivery paths and asserts the property that distinguishes them — what a child process can read.
//
// The child process is real (`sh -c env`) and not a check of os.Environ(), because os.Environ() is
// what this package edits and asserting on it would only prove that Unsetenv unsets. The question
// SEC-006 exists to answer is what the `claude` CLI the worker spawns, and the Python interpreter
// `aeon eval run` execs, can see — and that is decided by the environment Go hands to exec, which is
// what this test reads back.
func TestASecretFromAFileNeverEntersTheEnvironment(t *testing.T) {
	const name = "AEON_TEST_SECRET"
	const sentinel = "sentinel-value-7f3a1c" // distinctive enough to grep a whole environment for

	t.Run("a value in the environment is scrubbed, so children do not inherit it", func(t *testing.T) {
		ResetForTest()
		t.Setenv(name, sentinel)
		if !childEnvContains(t, sentinel) {
			t.Fatal("negative control failed: the sentinel must be visible to a child BEFORE Take, " +
				"or this test proves nothing about Take")
		}

		value, source, err := Take(name)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		if value != sentinel {
			t.Fatalf("value = %q, want the sentinel", value)
		}
		if source != SourceEnv {
			t.Fatalf("source = %q, want %q", source, SourceEnv)
		}
		if childEnvContains(t, sentinel) {
			t.Fatal("a child process still inherits the secret after Take — the scrub did nothing")
		}
	})

	t.Run("a value in a file is never in the environment at any point", func(t *testing.T) {
		ResetForTest()
		path := filepath.Join(t.TempDir(), "secret")
		// With the trailing newline `echo` would leave, because that is the realistic file and the
		// credential has to survive it.
		if err := os.WriteFile(path, []byte(sentinel+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name+FileSuffix, path)

		if childEnvContains(t, sentinel) {
			t.Fatal("the sentinel is in the environment before Take; the file path is supposed to " +
				"mean the value was never there")
		}
		value, source, err := Take(name)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		if value != sentinel {
			t.Fatalf("value = %q, want the sentinel with its trailing newline trimmed", value)
		}
		if source != SourceFile {
			t.Fatalf("source = %q, want %q", source, SourceFile)
		}
		if childEnvContains(t, sentinel) {
			t.Fatal("the secret reached the environment via the file path")
		}
		// The POINTER is scrubbed too, so a child cannot read the file either just by being told
		// where it is. It can still reach the path if it is mounted into the same filesystem, which
		// is a mount decision and not this package's to make.
		if _, ok := os.LookupEnv(name + FileSuffix); ok {
			t.Fatalf("%s%s survived Take", name, FileSuffix)
		}
	})
}

// TestTwoSourcesForOneSecretIsRefused fixes the refusal rather than a precedence rule. A rule would
// be convenient and would make a rotation applied to the losing source have no effect at all, with
// every surface reporting a configured deployment.
func TestTwoSourcesForOneSecretIsRefused(t *testing.T) {
	ResetForTest()
	const name = "AEON_TEST_SECRET"
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("from-the-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(name, "from-the-environment")
	t.Setenv(name+FileSuffix, path)

	value, _, err := Take(name)
	if err == nil {
		t.Fatalf("both sources set returned %q and no error", value)
	}
	if value != "" {
		t.Fatalf("a refusal still handed back %q", value)
	}
	for _, want := range []string{name, name + FileSuffix} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// TestAnUnreadableSecretStoreIsNotAnUnconfiguredProvider is the three-state rule this project keeps
// re-learning, on credentials this time: absent, present, and broken are three states, and
// collapsing the third into the first makes a mounted-volume failure look like a deployment that
// deliberately has no such provider. Routing would then fall through to the next candidate in the
// bundle and the run would answer from a different model at a different price, successfully.
func TestAnUnreadableSecretStoreIsNotAnUnconfiguredProvider(t *testing.T) {
	const name = "AEON_TEST_SECRET"

	t.Run("a missing file is refused, not treated as absent", func(t *testing.T) {
		ResetForTest()
		t.Setenv(name+FileSuffix, filepath.Join(t.TempDir(), "never-mounted"))
		if _, source, err := Take(name); err == nil {
			t.Fatalf("a missing secret file resolved to source %q with no error", source)
		}
	})

	t.Run("an empty file is refused", func(t *testing.T) {
		ResetForTest()
		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name+FileSuffix, path)
		if _, _, err := Take(name); err == nil {
			t.Fatal("a file holding only a newline resolved with no error")
		}
	})

	t.Run("neither set is absent and NOT an error", func(t *testing.T) {
		ResetForTest()
		os.Unsetenv(name)
		os.Unsetenv(name + FileSuffix)
		value, source, err := Take(name)
		if err != nil {
			t.Fatalf("an unconfigured credential must not be an error: %v", err)
		}
		if value != "" || source != SourceAbsent {
			t.Fatalf("value = %q, source = %q; want empty and %q", value, source, SourceAbsent)
		}
	})
}

// TestASecondReadGetsTheSameSecret is the memoisation, and it is load-bearing: aeon-modelgw reads
// PROMETHEUS_CLIENT_SECRET twice (routing, and MDL-015's scope derivation) and aeon-toolgw twice
// (the RAG embedder and its own client). A destructive read without this would authenticate on the
// first path and 401 on the second, in one process, which looks like an intermittent provider.
func TestASecondReadGetsTheSameSecret(t *testing.T) {
	ResetForTest()
	const name = "AEON_TEST_SECRET"
	t.Setenv(name, "read-me-twice")

	first, _, err := Take(name)
	if err != nil {
		t.Fatal(err)
	}
	second, source, err := Take(name)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second read = %q, first = %q", second, first)
	}
	if source != SourceEnv {
		t.Fatalf("second read reported source %q", source)
	}
	if got := os.Getenv(name); got != "" {
		t.Fatalf("%s is still in the environment after two reads: the scrub is not holding", name)
	}
}

// childEnvContains reports whether a real child process can see needle anywhere in its environment.
func childEnvContains(t *testing.T, needle string) bool {
	t.Helper()
	out, err := exec.Command("sh", "-c", "env").Output()
	if err != nil {
		t.Fatalf("reading a child process's environment: %v", err)
	}
	return strings.Contains(string(out), needle)
}
