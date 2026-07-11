package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/backendtest"
)

// TestContract runs the identical suite every Backend must pass,
// including the persistence tests via the Reopen hook: close the store
// and reopen the same file, everything must still be there.
func TestContract(t *testing.T) {
	paths := map[backend.Backend]string{}
	backendtest.Run(t, backendtest.Config{
		Open: func(t *testing.T) backend.Backend {
			path := filepath.Join(t.TempDir(), "kv.sqlite")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			paths[s] = path
			return s
		},
		Reopen: func(t *testing.T, b backend.Backend) backend.Backend {
			path := paths[b]
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			paths[s] = path
			return s
		},
	})
}
