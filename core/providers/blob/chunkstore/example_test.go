package chunkstore_test

import (
	"context"
	"fmt"
	"io"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob/chunkstore"
)

// Example stores a blob by streaming its bytes in, then reads them back.
// The store splits the content into fixed pieces internally so a whole blob
// is never held in memory; callers see only the streaming writer and the
// content-addressed id that Commit returns. Any backend.Backend works - here
// the in-memory one, but a SQLite backend behaves identically.
func Example() {
	store, err := chunkstore.New(memory.New())
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	acct := jmap.Id("Aexample")

	// Write the content in pieces; Commit returns its content address.
	w, err := store.Create(ctx, acct)
	if err != nil {
		panic(err)
	}
	if _, err := io.WriteString(w, "hello, "); err != nil {
		panic(err)
	}
	if _, err := io.WriteString(w, "blob store"); err != nil {
		panic(err)
	}
	id, err := w.Commit()
	if err != nil {
		panic(err)
	}

	// Read it back as a stream.
	rc, size, err := store.Open(ctx, acct, id)
	if err != nil {
		panic(err)
	}
	defer rc.Close()
	content, err := io.ReadAll(rc)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%d bytes: %s\n", size, content)

	// Output:
	// 17 bytes: hello, blob store
}
