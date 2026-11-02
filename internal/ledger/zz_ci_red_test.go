package ledger

import "testing"

func TestIntentionalFailure(t *testing.T) {
	t.Fatal("intentional CI red check")
}
