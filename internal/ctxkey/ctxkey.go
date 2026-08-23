// Пакет ctxkey — типизированные ключи для context.Context.
package ctxkey

import "context"

// marker занимает ненулевой размер, поэтому каждый экземпляр
// получает собственный адрес и ключи не совпадают между собой.
type marker struct {
	_ byte
}

type Key[T any] struct {
	id   *marker
	name string
}

func New[T any](name string) Key[T] {
	return Key[T]{
		id:   &marker{},
		name: name,
	}
}

func (k Key[T]) With(ctx context.Context, value T) context.Context {
	return context.WithValue(ctx, k.id, value)
}

func (k Key[T]) Get(ctx context.Context) (T, bool) {
	value, ok := ctx.Value(k.id).(T)
	return value, ok
}

func (k Key[T]) MustGet(ctx context.Context) T {
	value, ok := k.Get(ctx)
	if !ok {
		panic("ctxkey: value not found: " + k.name)
	}

	return value
}
