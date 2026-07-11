package memory

import (
	"testing"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/backendtest"
)

func TestContract(t *testing.T) {
	backendtest.Run(t, backendtest.Config{
		Open: func(t *testing.T) backend.Backend { return New() },
		// No Reopen: memory has no persistence, the suite skips it.
	})
}
