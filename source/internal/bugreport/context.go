package bugreport

import "context"

type storeKey struct{}

func WithStore(ctx context.Context, store *Store) context.Context {
	return context.WithValue(ctx, storeKey{}, store)
}
func FromContext(ctx context.Context) *Store {
	store, _ := ctx.Value(storeKey{}).(*Store)
	return store
}

// Actor names are presentation only and are resolved after the policy lease.
type actorNameKey struct{}

func WithActorName(ctx context.Context, resolve func(string) string) context.Context {
	return context.WithValue(ctx, actorNameKey{}, resolve)
}
func ActorName(ctx context.Context, id string) string {
	if resolve, ok := ctx.Value(actorNameKey{}).(func(string) string); ok {
		if name := resolve(id); name != "" {
			return Redact(name)
		}
	}
	return "administrator"
}
