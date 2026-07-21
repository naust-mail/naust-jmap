package notify_test

import (
	"testing"

	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/providers/notify/notifytest"
)

// TestInProcessContract runs the shared notifier contract suite against the
// single-node in-process notifier.
func TestInProcessContract(t *testing.T) {
	notifytest.Run(t, func(t *testing.T) notify.Notifier { return notify.NewInProcess() })
}

// TestInProcessLinked runs the cross-instance contract with a single in-process
// notifier serving both ends: it fans out to every subscription it holds, so a
// subscriber and a publisher on the same instance observe each other.
func TestInProcessLinked(t *testing.T) {
	notifytest.RunLinked(t, func(t *testing.T) (a, b notify.Notifier) {
		n := notify.NewInProcess()
		return n, n
	})
}
