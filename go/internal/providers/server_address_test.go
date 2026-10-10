package providers

import "testing"

// TestHostOfNeverLeaksACredential is the security-relevant half of VRT-AXO-002's `server.address`,
// and it is in CI rather than in the Tempo-backed test because it needs no infrastructure and the
// thing it protects is the one that cannot be walked back: telemetry is copied to collectors,
// retained, and read by people who were never meant to see a password.
func TestHostOfNeverLeaksACredential(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain host", "https://api.example.test", "api.example.test"},
		{"host with a port", "http://127.0.0.1:9000", "127.0.0.1"},
		{"host with a path", "https://api.example.test/v1/chat", "api.example.test"},
		// THE ONE THAT MATTERS. A base URL may legitimately carry userinfo, and a span attribute is
		// the last place it should land.
		{"userinfo is dropped", "https://user:s3cret@api.example.test:9000/v1", "api.example.test"},
		{"userinfo with no password", "https://user@api.example.test", "api.example.test"},
		// Absent rather than half-parsed: a malformed endpoint is a configuration problem the call
		// itself reports far more clearly than a span attribute could.
		{"empty", "", ""},
		{"not a URL", "://nonsense", ""},
		{"a bare host with no scheme", "api.example.test:9000", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostOf(tc.in); got != tc.want {
				t.Errorf("HostOf(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("no output of HostOf ever contains a credential separator", func(t *testing.T) {
		// A property rather than another case: whatever the input, an "@" or a ":" in the result
		// would mean userinfo or a port survived. This fails for any future rewrite that starts
		// returning host:port under `server.address`, which is a different OTel attribute.
		for _, raw := range []string{
			"https://user:pass@h.test:1/x", "http://u@h.test", "https://h.test:65535",
		} {
			got := HostOf(raw)
			for _, bad := range []string{"@", ":"} {
				if len(got) > 0 && contains(got, bad) {
					t.Errorf("HostOf(%q) = %q, which still carries %q", raw, got, bad)
				}
			}
		}
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
