package mcpconfig

import (
	"context"
	"testing"
)

func TestVersionFallbackAndContextIsolation(t *testing.T) {
	for _, input := range []string{"", " ", "9.9.9-test"} {
		want := "dev"
		if input == "9.9.9-test" {
			want = input
		}
		ctx := WithVersion(context.Background(), input)
		if Version(input) != want || ContextVersion(ctx) != want {
			t.Fatal("version normalization failed")
		}
		if ContextVersion(context.Background()) != "dev" {
			t.Fatal("version escaped context")
		}
	}
}
