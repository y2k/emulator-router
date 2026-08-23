package ctxkey

import (
	"context"
	"testing"
)

func TestKeysAreIndependent(t *testing.T) {
	a := New[int]("a")
	b := New[int]("b")

	ctx := a.With(context.Background(), 42)

	if got := a.MustGet(ctx); got != 42 {
		t.Fatalf("a.MustGet = %d, ожидалось 42", got)
	}
	if _, ok := b.Get(ctx); ok {
		t.Fatal("b.Get нашёл значение чужого ключа")
	}
}

func TestMustGetPanicsWhenMissing(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustGet не паникует на пустом контексте")
		}
	}()

	New[string]("token").MustGet(context.Background())
}
