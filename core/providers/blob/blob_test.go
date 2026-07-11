package blob

import (
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

func TestIdFor(t *testing.T) {
	a := IdFor([]byte("hello"))
	b := IdFor([]byte("hello"))
	c := IdFor([]byte("world"))
	if a != b {
		t.Errorf("IdFor is not deterministic: %s != %s", a, b)
	}
	if a == c {
		t.Error("distinct content produced the same blobId")
	}
	// Content addresses must be valid section 1.2 Ids, and the leading
	// letter keeps them out of the risky forms the section lists.
	for _, id := range []jmap.Id{a, c, IdFor(nil)} {
		if !id.Valid() {
			t.Errorf("IdFor produced an invalid Id: %v", id)
		}
	}
	if a[0] != 'G' {
		t.Errorf("blobId %s does not start with the server letter", a)
	}
	// SHA-256 in unpadded base64url plus the prefix: always 44 chars.
	if len(a) != 44 {
		t.Errorf("blobId length %d, want 44", len(a))
	}
}
