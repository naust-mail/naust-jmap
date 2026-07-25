package auth

import "testing"

// TestCredentialKey checks the fallback RFC 8620 section 7.2 push
// subscriptions rely on: a push subscription is tied to the creating
// credential when the Authenticator distinguishes credentials from
// identities, and falls back to Username when it doesn't (Credential is
// "the identity IS the credential" in that case).
func TestCredentialKey(t *testing.T) {
	cases := []struct {
		name string
		i    Identity
		want string
	}{
		{"credential set", Identity{Username: "alice", Credential: "token-1"}, "token-1"},
		{"credential empty falls back to username", Identity{Username: "alice", Credential: ""}, "alice"},
		{"both empty", Identity{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.i.CredentialKey(); got != c.want {
				t.Errorf("CredentialKey() = %q, want %q", got, c.want)
			}
		})
	}
}
